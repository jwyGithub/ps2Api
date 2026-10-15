// toolsets_test.go —— ToolsetsProvider 透传的关键路径回归：
// 200 + error JSON（非 SSE）必须分类为 RateLimited 供 api 层换号重试，
// 而不是当成 Success 让客户端收到空流（2026-09-24 修复引入）。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ps2api/internal/store"
)

func toolsetsTestAccount(t *testing.T) *store.Account {
	t.Helper()
	tokens, _ := json.Marshal(map[string]string{
		"access_token": "x", "user_id": "u", "workspace_id": "w",
	})
	return &store.Account{ID: 88, Tokens: string(tokens), Enabled: true, Status: "active"}
}

func newToolsetsTestProvider(t *testing.T, handler http.HandlerFunc) *ToolsetsProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := New()
	p.Client = &http.Client{Transport: redirectTransport{base: http.DefaultTransport, target: srv.URL}}
	return NewToolsetsProvider(p)
}

// 流式收到 200 + upstream_unavailable JSON（非 SSE）时必须 RateLimited，不得 Success。
func TestToolsetsStreamNonSSEErrorClassified(t *testing.T) {
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"code":"upstream_unavailable","message":"The model provider is unavailable. Please try again."}}`)
	})
	var got []string
	res := tp.StreamChat(context.Background(), toolsetsTestAccount(t), []byte(`{"model":"claude-opus-5","messages":[]}`), "claude-opus-5", 0, func(event string, data []byte) error {
		got = append(got, event)
		return nil
	})
	if res.Success {
		t.Fatal("non-SSE error body must not be Success (client would hang on empty stream)")
	}
	if !res.RateLimited {
		t.Fatalf("upstream_unavailable must classify RateLimited for account failover, got error: %s", res.Error)
	}
	if len(got) != 0 {
		t.Fatalf("no events should be emitted, got %v", got)
	}
}

// 流式收到 rate_limited error JSON 时分类 RateLimited。
func TestToolsetsStreamRateLimitedJSON(t *testing.T) {
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"code":"rate_limited","message":"Too many requests. Please try again shortly."}}`)
	})
	res := tp.StreamChat(context.Background(), toolsetsTestAccount(t), []byte(`{"model":"claude-opus-5","messages":[]}`), "claude-opus-5", 0, func(string, []byte) error { return nil })
	if res.Success || !res.RateLimited {
		t.Fatalf("rate_limited JSON must classify RateLimited, got success=%v rateLimited=%v err=%s", res.Success, res.RateLimited, res.Error)
	}
}

// 正常 SSE 流必须逐事件透传、model 已被改写为三段式路由名。
func TestToolsetsStreamPassthrough(t *testing.T) {
	var upstreamModel string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		json.Unmarshal(body["model"], &upstreamModel)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	var events []string
	res := tp.StreamChat(context.Background(), toolsetsTestAccount(t), []byte(`{"model":"claude-opus-5","messages":[]}`), "claude-opus-5", 0, func(event string, data []byte) error {
		events = append(events, event)
		return nil
	})
	if !res.Success {
		t.Fatalf("normal SSE stream must succeed, err=%s", res.Error)
	}
	if upstreamModel != "claude-opus-5/anthropic/anthropic-messages" {
		t.Fatalf("upstream model must be three-segment route, got %q", upstreamModel)
	}
	want := []string{"message_start", "content_block_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// 非流式 200 + error JSON 同样分类（原有 Chat 路径回归）。
func TestToolsetsChatNonSSEErrorClassified(t *testing.T) {
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"code":"rate_limited","message":"Too many requests."}}`)
	})
	res, _ := tp.Chat(context.Background(), toolsetsTestAccount(t), []byte(`{"model":"claude-opus-5","messages":[]}`), "claude-opus-5", 0)
	if res.Success || !res.RateLimited {
		t.Fatalf("rate_limited JSON must classify RateLimited, got success=%v err=%s", res.Success, res.Error)
	}
}

// 出站适配：max_tokens 夹紧到上游硬上限 8096，超长工具名（MCP 名 >64 字符）压缩改写。
func TestToolsetsUpstreamAdaptation(t *testing.T) {
	var got struct {
		MaxTokens int             `json:"max_tokens"`
		Tools     []map[string]any `json:"tools"`
	}
	longName := "mcp__plugin_chrome-devtools-mcp_chrome-devtools__performance_analyze_insight" // 74 字符
	var tp *ToolsetsProvider // handler 执行时（Chat 出站中）已赋值
	tp = newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"claude-opus-5","content":[{"type":"tool_use","id":"t1","name":%q,"input":{}}]}`,
			mappedNameForTest(t, tp, longName))
	})
	raw := fmt.Sprintf(`{"model":"claude-opus-5","max_tokens":32000,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":%q,"description":"","input_schema":{"type":"object"}}]}`, longName)
	res, body := tp.Chat(context.Background(), toolsetsTestAccount(t), []byte(raw), "claude-opus-5", 0)
	if !res.Success {
		t.Fatalf("chat must succeed, err=%s", res.Error)
	}
	if got.MaxTokens != 8096 {
		t.Fatalf("max_tokens must be clamped to 8096, got %d", got.MaxTokens)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools must survive rewrite, got %d", len(got.Tools))
	}
	upName := got.Tools[0]["name"].(string)
	if len(upName) > 64 || !toolsetsToolNameRe.MatchString(upName) {
		t.Fatalf("upstream tool name must be <=64 & legal, got %q", upName)
	}
	// 响应里的 tool_use.name 必须回写客户端原名。
	var resp struct {
		Content []struct {
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Content) != 1 {
		t.Fatalf("bad response body: %s", body)
	}
	if resp.Content[0].Name != longName {
		t.Fatalf("response tool name must be original %q, got %q", longName, resp.Content[0].Name)
	}
}

// mappedNameForTest 取出 provider 对 longName 的实际改写名（供 mock 返回上游视角）。
func mappedNameForTest(t *testing.T, tp *ToolsetsProvider, orig string) string {
	t.Helper()
	mapped := tp.mapToolName(orig)
	if mapped == orig || len(mapped) > 64 {
		t.Fatalf("mapToolName must compress, got %q", mapped)
	}
	return mapped
}

// 续聊：messages 历史里回传的 tool_use（客户端原名）出站也必须改写，否则第二轮 400。
func TestToolsetsUpstreamAdaptsHistoryToolUse(t *testing.T) {
	longName := strings.Repeat("b", 70)
	var upstreamName string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			for _, c := range m.Content {
				if c.Type == "tool_use" {
					upstreamName = c.Name
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)
	})
	raw := fmt.Sprintf(`{"model":"claude-opus-5","max_tokens":1024,"messages":[`+
		`{"role":"user","content":[{"type":"text","text":"hi"}]},`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":%q,"input":{}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}]}`, longName)
	res, _ := tp.Chat(context.Background(), toolsetsTestAccount(t), []byte(raw), "claude-opus-5", 0)
	if !res.Success {
		t.Fatalf("chat must succeed, err=%s", res.Error)
	}
	if upstreamName == "" || upstreamName == longName {
		t.Fatalf("history tool_use name must be rewritten upstream, got %q", upstreamName)
	}
	if !toolsetsToolNameRe.MatchString(upstreamName) || len(upstreamName) > 64 {
		t.Fatalf("rewritten name must be legal, got %q", upstreamName)
	}
}

// 流式 content_block_start 的 tool_use.name 回写原名。
func TestToolsetsStreamToolNameUnmapped(t *testing.T) {
	longName := strings.Repeat("a", 70) // 超长 → 必被改写
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		var tools []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body["tools"], &tools)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":%q}}\n\n",
			tools[0].Name)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	raw := fmt.Sprintf(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"tools":[{"name":%q}]}`, longName)
	var blockName string
	res := tp.StreamChat(context.Background(), toolsetsTestAccount(t), []byte(raw), "claude-opus-5", 0, func(event string, data []byte) error {
		if event == "content_block_start" {
			var ev struct {
				ContentBlock struct {
					Name string `json:"name"`
				} `json:"content_block"`
			}
			_ = json.Unmarshal(data, &ev)
			blockName = ev.ContentBlock.Name
		}
		return nil
	})
	if !res.Success {
		t.Fatalf("stream must succeed, err=%s", res.Error)
	}
	if blockName != longName {
		t.Fatalf("streamed tool name must be original (len %d), got %q", len(longName), blockName)
	}
}

// 新版客户端扩展字段（context_management/safeguards）与 messages[] 里的 system role 消息
// 都会被上游 400 拒绝（2026-09-29 实测）。出站必须剥字段、折 system。
func TestToolsetsUpstreamStripsExtendedFieldsAndSystemRole(t *testing.T) {
	var got map[string]json.RawMessage
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Errorf("upstream body not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)
	})
	raw := `{"model":"claude-opus-5","max_tokens":100,"stream":true,` +
		`"context_management":{"edits":[{"keep":"all","type":"clear_thinking_20251015"}]},` +
		`"safeguards":null,` +
		`"system":"base prompt",` +
		`"messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"text","text":"hook output"}]},` +
		`{"role":"assistant","content":"done"}]}`
	res, _ := tp.Chat(context.Background(), toolsetsTestAccount(t), []byte(raw), "claude-opus-5", 0)
	if !res.Success {
		t.Fatalf("chat must succeed, err=%s", res.Error)
	}
	for _, banned := range []string{"context_management", "safeguards"} {
		if _, ok := got[banned]; ok {
			t.Fatalf("upstream body must drop %s", banned)
		}
	}
	var roles []string
	var msgs []struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(got["messages"], &msgs); err != nil {
		t.Fatalf("messages not parseable: %v", err)
	}
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if len(roles) != 2 || roles[0] != "user" || roles[1] != "assistant" {
		t.Fatalf("system role must be folded out, got roles %v", roles)
	}
	var sys []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got["system"], &sys); err != nil || len(sys) != 2 {
		t.Fatalf("system must merge prompt+hook output as text blocks, got %s", got["system"])
	}
	if sys[0].Text != "base prompt" || sys[1].Text != "hook output" {
		t.Fatalf("system blocks wrong: %+v", sys)
	}
}

func TestToolsetsMessagesStripsPoisonFields(t *testing.T) {
	// messages 端点对称防御（2026-10-15 M2/M4 实测）：message 级 id /
	// internal_chat_message_metadata_passthrough / phase 上游逐字段点名 400。
	var gotBody string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Write([]byte(`{"id":"msg_o","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"output_tokens":1}}`))
	})
	raw := `{"model":"claude-sonnet-5","max_tokens":50,"stream":true,"messages":[
		{"role":"user","content":"hi","id":"msg_x","internal_chat_message_metadata_passthrough":{"turn_id":"t"},"phase":"final_answer"},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]}
	]}`
	acc := toolsetsTestAccount(t)
	res, _ := tp.Chat(t.Context(), acc, []byte(raw), "claude-sonnet-5", 0)
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gotBody), &m); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]json.RawMessage
	json.Unmarshal(m["messages"], &msgs)
	for i, msg := range msgs {
		for _, poison := range []string{"id", "internal_chat_message_metadata_passthrough", "phase"} {
			if _, ok := msg[poison]; ok {
				t.Fatalf("messages[%d] still carries %s", i, poison)
			}
		}
	}
}

func TestToolsetsHeadersMatchDesktopCapture(t *testing.T) {
	// 2026-10-15 三轮 Charles 抓包（1/2/3.chls，桌面 12.32.2 webview 形态全 200）逐字段对齐：
	// sec-fetch same-site/cors/empty、sec-ch-ua* 三件套、referer=desktop.postman.com、
	// UA 尾 Safari/605.1.15、x-app-version 纯版本号、无 Origin、无 x-multi-login-token。
	tp := NewToolsetsProvider(New())
	tokens := &Tokens{AccessToken: "tok", WorkspaceID: "23458429"}
	h := tp.buildHeaders(tokens)

	if got := h.Get("sec-fetch-site"); got != "same-site" {
		t.Fatalf("sec-fetch-site = %q, want same-site", got)
	}
	if got := h.Get("sec-fetch-mode"); got != "cors" {
		t.Fatalf("sec-fetch-mode = %q, want cors", got)
	}
	if got := h.Get("sec-fetch-dest"); got != "empty" {
		t.Fatalf("sec-fetch-dest = %q, want empty", got)
	}
	if got := h.Get("sec-ch-ua"); got == "" || !strings.Contains(got, "Chromium") {
		t.Fatalf("sec-ch-ua missing: %q", got)
	}
	if got := h.Get("referer"); !strings.HasPrefix(got, "https://desktop.postman.com/?desktopVersion=12.32.2&") {
		t.Fatalf("referer = %q", got)
	}
	if got := h.Get("x-app-version"); got != toolsetsDisplayVersion {
		t.Fatalf("x-app-version = %q, want display version %q (no -ui suffix)", got, toolsetsDisplayVersion)
	}
	ua := h.Get("User-Agent")
	if !strings.HasSuffix(ua, "Safari/605.1.15") {
		t.Fatalf("UA suffix = %q, want Safari/605.1.15", ua)
	}
	if !strings.Contains(ua, "Postman/"+toolsetsDisplayVersion+" ") {
		t.Fatalf("UA must embed display version, got %q", ua)
	}
	if h.Get("Origin") != "" {
		t.Fatalf("Origin must not be sent (capture proves desktop client omits it)")
	}
	if h.Get("x-multi-login-token") != "" {
		t.Fatalf("x-multi-login-token must not be sent (capture proves desktop client omits it)")
	}
	if h.Get("accept-language") != "en-US" {
		t.Fatalf("accept-language = %q", h.Get("accept-language"))
	}
	if h.Get("priority") != "u=1, i" {
		t.Fatalf("priority = %q", h.Get("priority"))
	}
	if got := h.Get("accept-encoding"); got != "gzip, deflate, br, zstd" {
		t.Fatalf("accept-encoding = %q", got)
	}
	if h.Get("x-pstmn-req-service") != "ai-toolsets" {
		t.Fatal("x-pstmn-req-service missing")
	}
	if h.Get("x-entity-team-id") != "23458429" {
		t.Fatal("x-entity-team-id missing")
	}

	// WEB（postman.sid）登录态：Cookie 前缀保留，其余头同形态。
	hw := tp.buildHeaders(&Tokens{PostmanSID: "sid123", WorkspaceID: "w"})
	if hw.Get("Cookie") != "postman.sid=sid123" {
		t.Fatalf("web cookie = %q", hw.Get("Cookie"))
	}
	if hw.Get("sec-fetch-site") != "same-site" {
		t.Fatalf("web form must also be webview-shaped, got %q", hw.Get("sec-fetch-site"))
	}
}

func TestToolsetsRemembersCookiesFromResponses2(t *testing.T) {
	// 变体：第一次 429 带 Set-Cookie（jar 记下），重试（attempt=1/2）时请求头即应回带。
	var cookies []string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		cookies = append(cookies, r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "cfv", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"rate_limited","message":"Too many requests."}}`))
	})
	raw := `{"model":"claude-sonnet-5","max_tokens":50,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	acc := toolsetsTestAccount(t)
	res, _ := tp.Chat(t.Context(), acc, []byte(raw), "claude-sonnet-5", 0)
	if res.Success {
		t.Fatal("all attempts are 429, must not succeed")
	}
	if len(cookies) < 2 {
		t.Fatalf("expected 3 attempts, got %d", len(cookies))
	}
	if strings.Contains(cookies[0], "__cf_bm") {
		t.Fatalf("first attempt cannot have the cookie yet: %q", cookies[0])
	}
	for i, c := range cookies[1:] {
		if !strings.Contains(c, "__cf_bm=cfv") {
			t.Fatalf("attempt %d must carry remembered __cf_bm cookie, got %q", i+2, c)
		}
	}
}
