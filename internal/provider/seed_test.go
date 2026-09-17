package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ps2api/internal/store"
)

// TestSplitMessagesContextSeedReplacesTailWithSummaryInstruction:
// ContextSeed 请求走折叠路径时，tail 不再是最新 user 消息，而是摘要指令；
// 折叠历史 context 与 [User (task)] 前置渲染保持不变（复用三条契约）。
func TestSplitMessagesContextSeedReplacesTailWithSummaryInstruction(t *testing.T) {
	p := New()
	msgs := []ChatMessage{mustMsg(t, "user", "TASK_MARKER 原始任务描述")}
	for i := 0; i < 3; i++ {
		res := &Result{Content: strings.Repeat("历史结论. ", 100)}
		msgs = append(msgs, *assistantFollowup(res))
		msgs = append(msgs, mustMsg(t, "user", "第"+strings.Repeat("x", 50)+"轮指令"))
	}
	msgs = append(msgs, mustMsg(t, "user", "最新一轮消息"))

	split := p.splitMessagesSeed(msgs, "", false, true)
	q := split.Query

	if !strings.Contains(q, "TASK_MARKER") {
		t.Fatal("seed query lost folded history (task marker)")
	}
	if !strings.Contains(q, seedSummaryInstruction[:20]) {
		t.Fatalf("seed query must end with summary instruction, got: %q", q[len(q)-120:])
	}
	if strings.Contains(q, "最新一轮消息") {
		t.Fatal("seed query must NOT include the latest user message in tail")
	}
}

// TestShouldSeed: 触发条件四条全满足才补种。
func TestShouldSeed(t *testing.T) {
	p := New()
	history := []ChatMessage{mustMsg(t, "user", "任务")}
	for i := 0; i < 4; i++ {
		history = append(history, *assistantFollowup(&Result{Content: "回复"}))
		history = append(history, mustMsg(t, "user", "跟进"))
	}
	history = append(history, mustMsg(t, "user", "最新")) // 共 11 条

	// 冷启动 + 普通 + >6 条 + 开关默认开 → true
	req := &ChatRequest{Messages: history}
	if !p.shouldSeed(1, req) {
		t.Fatal("cold start with long history should seed")
	}

	// 命中已有会话 → false
	p.setConversationID(1, history[:len(history)-1], "conv-seeded")
	if p.shouldSeed(1, req) {
		t.Fatal("warm conversation must not seed")
	}

	// 短历史（≤6 条）→ false
	short := []ChatMessage{mustMsg(t, "user", "a"), mustMsg(t, "user", "最新")}
	if p.shouldSeed(1, &ChatRequest{Messages: short}) {
		t.Fatal("short history must not seed")
	}

	// 恰好 7 条含 assistant 的历史 → true（len > 6 即触发）
	exactly7 := []ChatMessage{mustMsg(t, "user", "任务")}
	for i := 0; i < 2; i++ {
		exactly7 = append(exactly7, *assistantFollowup(&Result{Content: "回复"}))
		exactly7 = append(exactly7, mustMsg(t, "user", "跟进"))
	}
	exactly7 = append(exactly7, *assistantFollowup(&Result{Content: "回复"}))
	exactly7 = append(exactly7, mustMsg(t, "user", "最新")) // 共 7 条
	if len(exactly7) != 7 {
		t.Fatalf("fixture must have exactly 7 messages, got %d", len(exactly7))
	}
	if !p.shouldSeed(2, &ChatRequest{Messages: exactly7}) {
		t.Fatal("exactly 7 messages with assistant history should seed")
	}

	// 纯 user 的 9 条历史（无 assistant/tool）→ false（白种配额）
	userOnly := make([]ChatMessage, 0, 9)
	for i := 0; i < 9; i++ {
		userOnly = append(userOnly, mustMsg(t, "user", "u"))
	}
	if p.shouldSeed(1, &ChatRequest{Messages: userOnly}) {
		t.Fatal("user-only history must not seed")
	}

	// WafProbe → false
	reqProbe := &ChatRequest{Messages: history, WafProbe: true}
	if p.shouldSeed(1, reqProbe) {
		t.Fatal("probe request must never seed")
	}

	// 开关关闭 → false
	t.Setenv("GATEWAY_CONTEXT_SEED", "0")
	if p.shouldSeed(1, req) {
		t.Fatal("disabled by env must not seed")
	}
}

// TestSeedConversationStoresPrefixMapping: seed 成功后按
// conversationFingerprint(messages[:queryIdx]) 前缀存映射，
// 第二轮 LookupConversation 必须命中。
func TestSeedConversationStoresPrefixMapping(t *testing.T) {
	// mock 上游：返回一个带 conversationId 的成功流。
	var bodies []map[string]interface{}
	srv := mockPostmanServer(t, "conv-seed-123", &bodies)
	client := &http.Client{Transport: redirectTransport{base: http.DefaultTransport, target: srv.URL}}
	p := New()
	p.Client = client
	acc := seedTestAccount(t, srv)

	msgs := []ChatMessage{mustMsg(t, "user", "TASK 原始任务")}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, *assistantFollowup(&Result{Content: "回复" + strings.Repeat("y", 100)}))
		msgs = append(msgs, mustMsg(t, "user", "跟进"+strings.Repeat("z", 100)))
	}
	msgs = append(msgs, mustMsg(t, "user", "最新消息"))
	req := &ChatRequest{Model: "claude-opus-4-8", Messages: msgs}
	tokens := &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}

	res := p.seedConversation(context.Background(), acc, req, tokens, "CLAUDE_OPUS_48_BEDROCK")
	if !res.Success {
		t.Fatalf("seed failed: %s", res.Error)
	}
	if got := p.LookupConversation(acc.ID, msgs); got != "conv-seed-123" {
		t.Fatalf("second turn must hit seeded conversation, got %q", got)
	}
	// 补种请求出站体：query 含摘要指令、conversationId 为 null。
	if len(bodies) != 1 {
		t.Fatalf("seed should issue exactly 1 upstream request, got %d", len(bodies))
	}
	input := bodies[0]["input"].(map[string]interface{})
	if input["conversationId"] != nil {
		t.Fatalf("seed request must send conversationId=null, got %v", input["conversationId"])
	}
	q := input["query"].(string)
	if !strings.Contains(q, "总结当前任务状态") {
		t.Fatalf("seed query must contain summary instruction")
	}
}

// TestStreamInternalSeedsColdStart: 端到端——长会话冷启动时 streamInternal
// 先发补种轮（query=折叠历史+摘要指令、conversationId=null），
// 再发主轮（query=仅最新消息、conversationId=补种返回值）。
func TestStreamInternalSeedsColdStart(t *testing.T) {
	var bodies []map[string]interface{}
	srv := mockPostmanServer(t, "conv-e2e", &bodies)
	p := New()
	p.Client = &http.Client{Transport: redirectTransport{base: http.DefaultTransport, target: srv.URL}}
	acc := seedTestAccount(t, srv)

	msgs := []ChatMessage{mustMsg(t, "user", "TASK_MARKER 原始任务描述")}
	for i := 0; i < 5; i++ {
		msgs = append(msgs, *assistantFollowup(&Result{Content: strings.Repeat("结论. ", 200)}))
		msgs = append(msgs, mustMsg(t, "user", strings.Repeat("跟进", 60)))
	}
	msgs = append(msgs, mustMsg(t, "user", "最新一轮消息"))

	req := &ChatRequest{Model: "claude-opus-4-8", Messages: msgs}
	res := p.Chat(context.Background(), acc, req)
	if !res.Success {
		t.Fatalf("chat failed: %s", res.Error)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected seed + main = 2 upstream calls, got %d", len(bodies))
	}
	seedInput := bodies[0]["input"].(map[string]interface{})
	mainInput := bodies[1]["input"].(map[string]interface{})
	if seedInput["conversationId"] != nil {
		t.Fatal("seed call must have conversationId=null")
	}
	if got := mainInput["conversationId"]; got != "conv-e2e" {
		t.Fatalf("main call must carry seeded conversationId, got %v", got)
	}
	seedQuery := seedInput["query"].(string)
	mainQuery := mainInput["query"].(string)
	if !strings.Contains(seedQuery, "TASK_MARKER") || !strings.Contains(seedQuery, "总结当前任务状态") {
		t.Fatal("seed query must contain folded history + summary instruction")
	}
	if strings.Contains(seedQuery, "最新一轮消息") {
		t.Fatal("seed query must not leak the latest message")
	}
	if !strings.Contains(mainQuery, "最新一轮消息") || strings.Contains(mainQuery, "TASK_MARKER") {
		t.Fatal("main query must contain only the latest turn")
	}
}

// TestStreamInternalSeedFallbackOnFailure: 补种轮非账号级失败（如上游 500）时
// 回落单发折叠路径——出站共 2 个请求（seed + folded main），主 query 为折叠产物
// （含最新消息 tail）。注意 403 属账号级失败（AuthFailed），按设计上抛换号、
// 不回落，故此处用 500 模拟可回落失败。
func TestStreamInternalSeedFallbackOnFailure(t *testing.T) {
	var bodies []map[string]interface{}
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if call == 1 { // 补种轮 → 500（非账号级，静默回落）
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"eventType\":\"done\",\"postbotNative\":true}\n\n")
	}))
	t.Cleanup(srv.Close)
	p := New()
	p.Client = &http.Client{Transport: redirectTransport{base: http.DefaultTransport, target: srv.URL}}
	acc := seedTestAccount(t, srv)

	msgs := []ChatMessage{mustMsg(t, "user", "TASK_MARKER 原始任务")}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, *assistantFollowup(&Result{Content: strings.Repeat("结论. ", 100)}))
		msgs = append(msgs, mustMsg(t, "user", strings.Repeat("跟进", 60)))
	}
	msgs = append(msgs, mustMsg(t, "user", "最新消息"))

	req := &ChatRequest{Model: "claude-opus-4-8", Messages: msgs}
	res := p.Chat(context.Background(), acc, req)
	_ = res
	if len(bodies) != 2 {
		t.Fatalf("fallback path = seed(500) + main(folded) = 2 calls, got %d", len(bodies))
	}
	mainQuery := bodies[1]["input"].(map[string]interface{})["query"].(string)
	if !strings.Contains(mainQuery, "TASK_MARKER") || !strings.Contains(mainQuery, "最新消息") {
		t.Fatal("fallback main query must be the folded product (history + latest)")
	}
}

// TestStreamInternalSeedAccountFailurePropagates: 补种轮 quota 耗尽时
// 主 res 带 QuotaExhausted（router 据此换号），且主轮不再出站。
// quota SSE 真实形状（sse.go handleUsage）：eventType=usage，data 即 Usage 对象，
// usageState ∈ {EXCEEDED, UNAVAILABLE, BLOCKED} 置 QuotaExceeded。
func TestStreamInternalSeedAccountFailurePropagates(t *testing.T) {
	var bodies []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"data\":{\"usageState\":\"EXCEEDED\"},\"eventType\":\"usage\",\"postbotNative\":true}\n\n")
	}))
	t.Cleanup(srv.Close)
	p := New()
	p.Client = &http.Client{Transport: redirectTransport{base: http.DefaultTransport, target: srv.URL}}
	acc := seedTestAccount(t, srv)

	msgs := []ChatMessage{mustMsg(t, "user", "TASK 原始任务")}
	for i := 0; i < 4; i++ {
		msgs = append(msgs, *assistantFollowup(&Result{Content: "回复"}))
		msgs = append(msgs, mustMsg(t, "user", "跟进"))
	}
	msgs = append(msgs, mustMsg(t, "user", "最新"))

	res := p.Chat(context.Background(), acc, &ChatRequest{Model: "claude-opus-4-8", Messages: msgs})
	if res.QuotaExhausted != true {
		t.Fatalf("seed quota exhaustion must propagate to main res, got %+v", res)
	}
	if len(bodies) != 1 {
		t.Fatalf("seed quota failure must abort before main round, got %d calls", len(bodies))
	}
	if q := bodies[0]["input"].(map[string]interface{})["query"].(string); !strings.Contains(q, "总结当前任务状态") {
		t.Fatal("first call must be the seed round (summary instruction in query)")
	}
}

// mockPostmanServer 模拟 Postman _gw/chat：记录每个请求体，返回一个带
// conversationId 与一段正文的成功 SSE 流。
func mockPostmanServer(t *testing.T, conversationID string, bodies *[]map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*bodies = append(*bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"data\":\"postman-agentmode-2025-06-25\",\"eventType\":\"streamingFormat\",\"postbotNative\":true}\n\n")
		fmt.Fprintf(w, "data: {\"data\":{\"id\":\"%s\",\"interactionCount\":1},\"eventType\":\"conversation\",\"postbotNative\":true}\n\n", conversationID)
		fmt.Fprintf(w, "data: {\"data\":{\"metadata\":{\"conversationId\":\"%s\"},\"textContent\":\"已总结。\"},\"eventType\":\"textChunk\",\"postbotNative\":true}\n\n", conversationID)
		fmt.Fprintf(w, "data: {\"eventType\":\"done\",\"postbotNative\":true}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedTestAccount 构造指向 mock server 的账号。
func seedTestAccount(t *testing.T, _ *httptest.Server) *store.Account {
	t.Helper()
	tokens, _ := json.Marshal(map[string]string{
		"access_token": "x", "user_id": "u", "workspace_id": "w",
	})
	return &store.Account{ID: 77, Tokens: string(tokens), Enabled: true}
}

// redirectTransport 把所有请求重定向到 mock server。
type redirectTransport struct {
	base   http.RoundTripper
	target string
}

func (rt redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := r.URL.Parse(rt.target)
	req := r.Clone(r.Context())
	req.URL = u
	return rt.base.RoundTrip(req)
}
