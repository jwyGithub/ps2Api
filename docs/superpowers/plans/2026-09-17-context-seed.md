# 冷启动上下文补种（Context Seed）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 冷启动（换号/重启/指纹未命中）时先发一轮上下文补种请求在 Postman 服务端重建会话，第二轮带 conversationId 增量发送，消除 10000 rune 截断造成的答非所问。

**Architecture:** seed 复用 `splitMessages` 现有折叠逻辑（含 09-10/09-15/09-17 三条渲染契约），通过 `ChatRequest.ContextSeed` 控制字段把 tail 替换为摘要指令；接入点在 `streamInternal` 的 `buildBody` 之前；任何 seed 失败回落现有单发折叠路径。

**Tech Stack:** Go 标准库 + 现有 provider 包（net/http, context）。无新依赖。

**Spec:** `docs/superpowers/specs/2026-09-17-context-seed-design.md`

## Global Constraints

- 开关 `GATEWAY_CONTEXT_SEED`：默认开启，设 `"0"` 关闭（沿用 `GATEWAY_DISABLE_WAF_NEUTRALIZE != "1"` 的取反惯例：`os.Getenv("GATEWAY_CONTEXT_SEED") != "0"`）
- 触发条件四条全满足才 seed：冷启动（LookupConversation 落空）、非 tool-tail、历史消息数 > 6、开关开
- `req.WafProbe` 请求绝不 seed（探针必须复现可疑内容）
- 失败兜底契约：seed 失败静默回落折叠路径；seed 的 AuthFailed/QuotaExhausted/RateLimited 必须上抛到主 res（router 据此换号）
- seed 轮也走 `Trace`（复用 streamInternal 自带 Trace，无需额外埋点）
- 不改 `conversationFingerprint`、不改 `RememberConversation`、不改 router 任何代码

## 关键代码事实（实现者必读）

- `streamInternal` 签名（[stream.go:77](internal/provider/stream.go#L77)）：
  `func (p *Provider) streamInternal(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, postmanModel string, emit EmitFunc, res *Result) error`
  第 85 行 `body := p.buildBody(req, tokens, postmanModel, acc.ID)` —— seed 接入点在这行之前。
- `buildBody`（[request.go:10](internal/provider/request.go#L10)）第 12 行 `convID := p.LookupConversation(accountID, req.Messages)`；第 23 行 `split := p.splitMessages(req.Messages, convID, req.WafProbe)`；第 46 行 `query := capUpstreamQuery(upstreamQuery)`。
- `splitMessages`（[messages.go:124](internal/provider/messages.go#L124)）返回 `splitResult{Query string}`；折叠路径在 `convID == ""` 时走到；`queryIdx` 是最新一条非 tool 的 user 消息下标（[messages.go:171-181](internal/provider/messages.go#L171-L181)）；折叠 sections 顺序：普通续聊 `[User (task)]` 前置 → context → tail（[messages.go:273-300](internal/provider/messages.go#L273-L300)）。
- `LookupConversation`（[conversation.go:82](internal/provider/conversation.go#L82)）前缀循环：从 `len(messages)-1` 往回逐个试 `GetConversation(accountID, conversationFingerprint(messages[:i]))`。seed 存映射必须用 `conversationFingerprint(messages[:queryIdx])`（去掉最新 user 消息的前缀），这样第二轮 `LookupConversation(req.Messages)` 的循环里 `i == queryIdx` 那步会命中。
- `setConversationID`（[conversation.go:94](internal/provider/conversation.go#L94)）存的是「完整 messages 指纹」。seed 不能用它存（第二轮 messages 多了最新 user 消息，完整指纹不同）；需要直接调 `p.convStore.PutConversation` + `p.convStore.PutOwner`。`convStore` 是 `Provider` 的私有字段，`seed.go` 与 `Provider` 同包，可直接访问。
- 测试基建：`internal/provider` 现有测试用 `p := New()` + `p.buildBody(...)` 断言 query；mock 上游先例在 `internal/provider/vision_test.go`（httptest.Server）。`Provider.Client` 是导出字段，测试里可直接替换为指向 httptest server 的 client。
- `Trace` 在 streamInternal 内部已对每次出站调用记录 `upstream.request`（含 body），测试可从 httptest server 侧捕获请求体断言。

## File Structure

- Create: `internal/provider/seed.go` —— 补种核心（触发判断 + seedConversation + 指令常量 + 开关）
- Create: `internal/provider/seed_test.go` —— 四组测试
- Modify: `internal/provider/types.go` —— ChatRequest 加 `ContextSeed bool \`json:"-"\``
- Modify: `internal/provider/messages.go` —— splitMessages 折叠路径感知 ContextSeed（tail 替换为摘要指令）
- Modify: `internal/provider/stream.go` —— streamInternal 开头接入

---

### Task 1: ContextSeed 字段 + splitMessages 感知

**Files:**
- Modify: `internal/provider/types.go`（ChatRequest 结构体，WafProbe 字段后）
- Modify: `internal/provider/messages.go:273-300`（折叠 sections 组装段）
- Test: `internal/provider/seed_test.go`（新建）

**Interfaces:**
- Produces: `ChatRequest.ContextSeed bool`（`json:"-"`，控制字段）；splitMessages 在 `req` 带 ContextSeed 时 tail 渲染为摘要指令。注意 splitMessages 是方法且不接收 req——需要把 ContextSeed 作为新参数传入。

- [ ] **Step 1: 写失败测试**

```go
// internal/provider/seed_test.go
package provider

import (
	"encoding/json"
	"strings"
	"testing"
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/provider/ -run TestSplitMessagesContextSeed -count=1`
Expected: FAIL（`splitMessagesSeed` 未定义、`seedSummaryInstruction` 未定义）

- [ ] **Step 3: 最小实现**

`types.go` ChatRequest 末尾（WafProbe 字段后）追加：

```go
	// ContextSeed 标记本请求是「上下文补种」轮：冷启动折叠时把 tail（最新消息渲染）
	// 替换为一段摘要指令，让上游模型复述任务状态后在服务端建立带完整历史的会话。
	// 由 streamInternal 的补种逻辑注入，客户端不可见（json:"-"）。
	ContextSeed bool `json:"-"`
```

`messages.go`：`splitMessages` 改名保留原签名做薄包装，主逻辑加参数（保持现有 4 个调用点不动的最小 diff）：

```go
// splitMessages 保留原签名：非补种请求的入口。
func (p *Provider) splitMessages(messages []ChatMessage, convID string, wafProbe bool) splitResult {
	return p.splitMessagesSeed(messages, convID, wafProbe, false)
}

// splitMessagesSeed 带补种标志的折叠/切分主逻辑。
func (p *Provider) splitMessagesSeed(messages []ChatMessage, convID string, wafProbe, contextSeed bool) splitResult {
```

方法体内两处改动：

1. 折叠路径 tail 组装段（原 messages.go:285-296）：

```go
	tail := query
	if contextSeed {
		// 补种轮：tail 不渲染最新消息（服务端还不需要它），改为摘要指令——
		// 模型复述任务状态即建立会话，第二轮增量带上最新消息。
		tail = seedSummaryInstruction
	} else if isToolTail {
		tail = truncateMiddleRunes(query, FoldedTailToolResultRunes)
	} else if queryIdx >= 0 && query != "" {
		// 探针请求不加 “[User]\n” 角色标注：探针必须逐字复现可疑内容，任何前缀
		// 都改变字节形状（见 ChatRequest.WafProbe 注释）。
		if wafProbe {
			tail = query
		} else {
			tail = "[User]\n" + query
		}
	}
```

2. 文件末尾（capUpstreamQuery 之后）加指令常量：

```go
// seedSummaryInstruction 是补种轮的 tail 指令：让模型对折叠历史做一段话总结，
// 不执行操作。摘要会留在服务端会话里供第二轮参照（折叠原文 + 摘要都在）。
const seedSummaryInstruction = "[User]\n以上是此前对话的完整上下文。请用一段话总结当前任务状态与最近进展，不要执行任何操作、不要调用任何工具。"
```

注意：`buildBody` 的调用点（request.go:23）传 `req.ContextSeed`：

```go
	split := p.splitMessagesSeed(req.Messages, convID, req.WafProbe, req.ContextSeed)
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/provider/ -run "TestSplitMessagesContextSeed" -count=1 && go test ./internal/provider/ -count=1`
Expected: PASS（含全量回归——三条契约测试不受影响，因为 ContextSeed=false 路径字节不变）

- [ ] **Step 5: Commit**

```bash
git add internal/provider/types.go internal/provider/messages.go internal/provider/seed_test.go
git commit -m "provider: splitMessages supports ContextSeed tail replacement"
```

---

### Task 2: seedConversation + 触发判断 + 开关

**Files:**
- Create: `internal/provider/seed.go`
- Test: `internal/provider/seed_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `ContextSeed` 字段、`seedSummaryInstruction`
- Produces:
  - `func contextSeedEnabled() bool`
  - `func (p *Provider) shouldSeed(accID int64, req *ChatRequest) bool`（触发条件判断，供 streamInternal 调用）
  - `func (p *Provider) seedConversation(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, postmanModel string) *Result`（发起补种轮并按前缀指纹存映射）

- [ ] **Step 1: 写失败测试**

追加到 `seed_test.go`：

```go
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
	client := &http.Client{}
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
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/provider/ -run "TestShouldSeed|TestSeedConversation" -count=1`
Expected: FAIL（`shouldSeed` / `seedConversation` / `mockPostmanServer` / `seedTestAccount` 未定义）

- [ ] **Step 3: 写测试基建 + 最小实现**

`seed_test.go` 追加 mock 基建（httptest 上游，模拟 Postman SSE 流）：

```go
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
func seedTestAccount(t *testing.T, srv *httptest.Server) *store.Account {
	t.Helper()
	tokens, _ := json.Marshal(map[string]string{
		"access_token": "x", "user_id": "u", "workspace_id": "w",
	})
	return &store.Account{ID: 77, Tokens: string(tokens), Enabled: true}
}
```

注意：mock server 的 URL 需要生效——`chatURL` 用 `tokens.WorkspaceSubdomain + ".postman.co"`，测试无法改域名。**方案**：`seedTestAccount` 里给 tokens 设 `workspace_uuid` 走 desktop 分支也不行（URL 常量）。**正确做法**：`Provider.Client` 的 `Transport` 包装重写 URL。在测试里：

```go
// redirectTransport 把所有请求重定向到 mock server。
type redirectTransport struct{ base http.RoundTripper; target string }

func (rt redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := r.URL.Parse(rt.target)
	req := r.Clone(r.Context())
	req.URL = u
	return rt.base.RoundTrip(req)
}
```

`p.Client = &http.Client{Transport: redirectTransport{base: http.DefaultTransport, target: srv.URL}}`。

`internal/provider/seed.go` 实现：

```go
package provider

import (
	"context"
	"strings"

	"ps2api/internal/store"
)

// contextSeedEnabled 报告冷启动补种开关。默认开启；GATEWAY_CONTEXT_SEED=0 关闭。
func contextSeedEnabled() bool { return os.Getenv("GATEWAY_CONTEXT_SEED") != "0" }

// seedHistoryMinMessages 是触发补种的最小历史消息数（含最新消息）。
// 更短的会话折叠产物不超 10000 rune，补种白花一次上游配额。
const seedHistoryMinMessages = 7

// shouldSeed 报告本次请求是否应做上下文补种。四个条件全满足：
// 冷启动（无会话命中）、非 tool-tail 重放、历史足够长、开关开启。
// WafProbe 探针绝不补种（必须逐字复现可疑内容）。
func (p *Provider) shouldSeed(accID int64, req *ChatRequest) bool {
	if !contextSeedEnabled() || req.WafProbe {
		return false
	}
	if toolTail(req.Messages) {
		return false
	}
	if len(req.Messages) <= seedHistoryMinMessages {
		return false
	}
	return p.LookupConversation(accID, req.Messages) == ""
}

// seedConversation 发起补种轮：复用折叠路径把全部历史发往上游（conversationId=null），
// 模型按摘要指令复述任务状态后在服务端建立会话。成功后按「去掉最新消息的前缀指纹」
// 存映射——第二轮请求的 LookupConversation 前缀循环恰好命中它，恢复增量模式。
func (p *Provider) seedConversation(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, postmanModel string) *Result {
	seedReq := *req
	seedReq.ContextSeed = true
	seedRes := &Result{}
	_ = p.streamInternal(ctx, acc, &seedReq, tokens, postmanModel, func(Delta) error { return nil }, seedRes)
	if !seedRes.Success || seedRes.ConversationID == "" {
		return seedRes
	}
	// 前缀指纹：messages[:queryIdx]（去掉最新 user 消息）。第二轮完整 messages
	// 的 LookupConversation 循环在 i==queryIdx 时命中此键。
	queryIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && !isAnthropicToolResult(req.Messages[i]) {
			queryIdx = i
			break
		}
	}
	if queryIdx < 0 {
		return seedRes
	}
	fp := conversationFingerprint(req.Messages[:queryIdx])
	p.convStore.PutConversation(acc.ID, fp, seedRes.ConversationID)
	p.convStore.PutOwner(fp, acc.ID)
	return seedRes
}
```

注意 `os` 已在 seed.go import 列表里（`"os"`）。`streamInternal` 内部会自己走 buildBody → splitMessagesSeed（因 seedReq.ContextSeed=true 且无会话命中 → 折叠 + 摘要指令 tail）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/provider/ -run "TestShouldSeed|TestSeedConversation" -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/provider/seed.go internal/provider/seed_test.go
git commit -m "provider: add context-seed core (shouldSeed + seedConversation)"
```

---

### Task 3: streamInternal 接入 + 失败兜底

**Files:**
- Modify: `internal/provider/stream.go:77-90`（streamInternal 开头）
- Test: `internal/provider/seed_test.go`（追加）

**Interfaces:**
- Consumes: Task 2 的 `shouldSeed` / `seedConversation`
- Produces: 端到端行为——冷启动长会话第一跳自动补种，任何失败回落折叠路径

- [ ] **Step 1: 写失败测试（端到端两跳）**

追加到 `seed_test.go`：

```go
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

// TestStreamInternalSeedFallbackOnFailure: 补种轮失败（如 403）时回落
// 单发折叠路径——出站恰好 1 个请求，query 为折叠产物（含最新消息 tail）。
func TestStreamInternalSeedFallbackOnFailure(t *testing.T) {
	var bodies []map[string]interface{}
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if call == 1 { // 补种轮 → 403
			w.WriteHeader(403)
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
		t.Fatalf("fallback path = seed(403) + main(folded) = 2 calls, got %d", len(bodies))
	}
	mainQuery := bodies[1]["input"].(map[string]interface{})["query"].(string)
	if !strings.Contains(mainQuery, "TASK_MARKER") || !strings.Contains(mainQuery, "最新消息") {
		t.Fatal("fallback main query must be the folded product (history + latest)")
	}
}

// TestStreamInternalSeedAccountFailurePropagates: 补种轮 quota 耗尽时
// 主 res 带 QuotaExhausted（router 据此换号）。
func TestStreamInternalSeedAccountFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"data\":{\"data\":{\"usageState\":\"LIMIT_REACHED\"}},\"eventType\":\"usage\",\"postbotNative\":true}\n\n")
		fmt.Fprintf(w, "data: {\"eventType\":\"quotaExceeded\",\"postbotNative\":true}\n\n")
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
}
```

注意：quota SSE 事件的确切字段形状以 `internal/provider/sse.go` 的 StreamReader 解析逻辑为准——实现时先读 `QuotaExceeded` 的判定代码，按真实形状 mock；上面是占位形状。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/provider/ -run "TestStreamInternalSeed" -count=1`
Expected: FAIL（streamInternal 未接入 seed）

- [ ] **Step 3: 接入 streamInternal**

`stream.go` 在 `body := p.buildBody(...)`（第 85 行）之前插入：

```go
	// 冷启动补种：长会话在无会话命中时先发一轮上下文重建（折叠历史 + 摘要指令），
	// 在服务端建立带历史的会话，随后主轮恢复增量发送——消除 10000 rune 截断
	// 造成的答非所问（2026-09-17 设计，docs/superpowers/specs/2026-09-17-context-seed-design.md）。
	// 任何补种失败都回落单发折叠路径（绝不比无补种更差）；账号级失败
	// （AuthFailed/QuotaExhausted/RateLimited）上抛给 router 换号重试。
	if p.shouldSeed(acc.ID, req) {
		seedRes := p.seedConversation(ctx, acc, req, tokens, postmanModel)
		if seedRes.AuthFailed || seedRes.QuotaExhausted || seedRes.RateLimited {
			*res = *seedRes
			return fmt.Errorf("%s", res.Error)
		}
	}
```

同时确认 `stream.go` 顶部 import 已有 `fmt`。

- [ ] **Step 4: 跑测试确认通过 + 全量回归**

Run: `go test ./internal/provider/ -run "TestStreamInternalSeed" -count=1 -v && go test ./internal/provider/ -count=1`
Expected: 全 PASS（注意 `TestProxyTunnelRealProxy` 在 `internal/tlsfp`，真实网络 flaky，与本改动无关，不在本包）

- [ ] **Step 5: Commit**

```bash
git add internal/provider/stream.go internal/provider/seed_test.go
git commit -m "provider: wire context seed into streamInternal with fallback"
```

---

### Task 4: 全量验证 + 文档收尾

**Files:**
- Modify: `docs/superpowers/specs/2026-09-17-context-seed-design.md`（状态行改「已实现」）

- [ ] **Step 1: 全量构建与测试**

Run: `go build ./... && go test ./... -count=1`
Expected: 除 `internal/tlsfp` 的 `TestProxyTunnelRealProxy`（真实网络 flaky，干净树也失败）外全 PASS

- [ ] **Step 2: 用 trace 数据手动验证（可选，需要运行实例）**

启动服务后构造一次冷启动长会话请求，检查 `data/traces/anthropic/<today>/`：
第一跳 query 含折叠历史+「总结当前任务状态」、conversationId=null；第二跳 query 仅最新消息、conversationId 非空。

- [ ] **Step 3: 更新设计文档状态 + Commit**

设计文档首行 `状态：已确认` 改为 `状态：已实现（2026-09-17）`。

```bash
git add docs/superpowers/specs/2026-09-17-context-seed-design.md
git commit -m "docs: mark context-seed design as implemented"
```
