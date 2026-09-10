package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func mustMsg(t *testing.T, role, text string) provider.ChatMessage {
	t.Helper()
	raw, _ := json.Marshal(text)
	return provider.ChatMessage{Role: role, Content: raw}
}

func mustRaw(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("invalid json: %s", s)
	}
	return json.RawMessage(s)
}

// 两个 active 账号。tokens 字段要能通过 provider.GetTokens 校验：
// 需要 user_id / workspace_id + access_token。
func newTestRouter(t *testing.T) *Router {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir + "\\router_test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	tok, _ := json.Marshal(provider.Tokens{AccessToken: "tok", UserID: "u", WorkspaceID: "w"})
	if _, err := s.UpsertAccount("a1@test.com", "", string(tok), "manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertAccount("a2@test.com", "", string(tok), "manual"); err != nil {
		t.Fatal(err)
	}
	return New(s)
}

// 首轮流到账号1，Agent A 续聊必须仍选中账号1（此前会轮询到账号2丢上下文）。
func TestStickyContinuationStaysOnOriginalAccount(t *testing.T) {
	r := newTestRouter(t)

	msgsFirst := []provider.ChatMessage{mustMsg(t, "user", "agent A hello")}
	acc1, poolUsed, err := r.pickAccount(nil, msgsFirst, false)
	if err != nil {
		t.Fatal(err)
	}
	if !poolUsed {
		t.Fatal("first turn should come from pool")
	}
	r.Pool.Done(acc1.ID)
	r.Provider.RememberConversation(acc1.ID, msgsFirst, &provider.Result{ConversationID: "conv-A1", Content: "hi"})

	// 模拟池子此时会轮到账号2：先让 pickAccount 无粘性走一次（吃掉账号2）。
	other := []provider.ChatMessage{mustMsg(t, "user", "some other agent new chat")}
	if acc, used, err := r.pickAccount(nil, other, false); err != nil || !used {
		t.Fatalf("other new chat should rotate: acc=%v used=%v err=%v", acc, used, err)
	}

	// Agent A 续聊：必须粘回账号1
	cont := []provider.ChatMessage{
		msgsFirst[0],
		{Role: "assistant", Content: mustRaw(t, `"hi"`)},
		mustMsg(t, "user", "continue please"),
	}
	stuck, used, err := r.pickAccount(nil, cont, false)
	if err != nil {
		t.Fatal(err)
	}
	if used {
		t.Fatalf("continuation should NOT come from pool rotation")
	}
	if stuck.ID != acc1.ID {
		t.Fatalf("continuation should stick to account %d, got %d", acc1.ID, stuck.ID)
	}
	if got := r.Provider.LookupConversation(acc1.ID, cont); got != "conv-A1" {
		t.Fatalf("should resume conv-A1 on original account, got %q", got)
	}
}

// 粘性账号被排除（额度耗尽/失败）后回退轮询，其余账号可接手。
func TestStickyFallsBackToPoolWhenExcluded(t *testing.T) {
	r := newTestRouter(t)
	msgsFirst := []provider.ChatMessage{mustMsg(t, "user", "agent B hello")}
	acc1, _, err := r.pickAccount(nil, msgsFirst, false)
	if err != nil {
		t.Fatal(err)
	}
	r.Pool.Done(acc1.ID)
	r.Provider.RememberConversation(acc1.ID, msgsFirst, &provider.Result{ConversationID: "conv-B1", Content: "hi"})

	cont := []provider.ChatMessage{
		msgsFirst[0],
		{Role: "assistant", Content: mustRaw(t, `"hi"`)},
		mustMsg(t, "user", "more"),
	}

	// 账号1被排除时，应回退轮询到账号2
	stuck, used, err := r.pickAccount(map[int64]bool{acc1.ID: true}, cont, false)
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("excluded sticky account must fall back to pool")
	}
	if stuck.ID == acc1.ID {
		t.Fatalf("fallback should not return excluded account %d", acc1.ID)
	}
}

// 粘性账号被禁用后回退轮询。
func TestStickyFallsBackWhenAccountDisabled(t *testing.T) {
	r := newTestRouter(t)
	msgsFirst := []provider.ChatMessage{mustMsg(t, "user", "agent C hello")}
	acc1, _, err := r.pickAccount(nil, msgsFirst, false)
	if err != nil {
		t.Fatal(err)
	}
	r.Pool.Done(acc1.ID)
	r.Provider.RememberConversation(acc1.ID, msgsFirst, &provider.Result{ConversationID: "conv-C1", Content: "hi"})
	if err := r.Store.SetAccountEnabled(acc1.ID, false); err != nil {
		t.Fatal(err)
	}

	cont := []provider.ChatMessage{
		msgsFirst[0],
		{Role: "assistant", Content: mustRaw(t, `"hi"`)},
		mustMsg(t, "user", "more"),
	}
	if _, used, err := r.pickAccount(nil, cont, false); err != nil || !used {
		t.Fatalf("disabled sticky account must fall back to pool: used=%v err=%v", used, err)
	}
}

// Cloudflare 403(HTML 风控拦截)归类为 GatewayBlocked：零特征（概率型边缘拦截）时允许一次
// 重试后终止——共打两次上游即返回 GatewayBlocked 错误(HTTP 层映射为 529)，不向客户端 emit
// 任何增量，且写入一条告警。
func TestStreamCloudflare403RetriesOnceThenStops(t *testing.T) {
	r := newTestRouter(t)
	var calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		h := make(http.Header)
		h.Set("Server", "cloudflare")
		h.Set("Content-Type", "text/html; charset=UTF-8")
		h.Set("Cf-Ray", "a2d562e1bc2349d4-LAX")
		body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
		return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("gateway 403 should return an error with no result: res=%+v account=%+v err=%v", res, account, err)
	}
	re, ok := err.(*RouteError)
	if !ok || !re.GatewayBlocked {
		t.Fatalf("error should be *RouteError with GatewayBlocked=true, got %T %v", err, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("zero-signature gateway block should retry exactly once: expected 2 upstream calls, got %d", got)
	}
	if output.Len() != 0 {
		t.Fatalf("no output should be emitted to client on gateway block, got %q", output.String())
	}
	if alerts, err := r.Store.ListAlerts("", 10); err != nil || len(alerts) == 0 {
		t.Fatalf("expected a gateway-rejected alert to be recorded: alerts=%v err=%v", alerts, err)
	}
}

// 403 冷却判别：出站体命中 WAF 特征 = 内容型拦截（同一内容换号必然复现）→ 不冷却账号；
// 零特征 = 疑似风控型 → 保留冷却让号池降级让路。
func TestStreamGatewayBlockedContentSignatureSkipsCooldown(t *testing.T) {
	new403Router := func() *Router {
		r := newTestRouter(t)
		r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			h := make(http.Header)
			h.Set("Server", "cloudflare")
			h.Set("Content-Type", "text/html; charset=UTF-8")
			body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
			return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}
		return r
	}

	// 内容型：用户消息携带 <script> 标记 → 出站体特征计数 >0 → 不冷却任何账号。
	// 默认出站 query 会被 WAF 中和（provider/waf.go），此处关掉开关模拟「特征漏网」
	// （中和未覆盖的出口/kill-switch 场景）——判别逻辑正是为这种漏网兜底的。
	t.Setenv("GATEWAY_DISABLE_WAF_NEUTRALIZE", "1")
	r := new403Router()
	_, _, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "fix <script>alert(1)</script> in my page")},
	}, func(delta provider.Delta) error { return nil })
	if _, ok := err.(*RouteError); !ok {
		t.Fatalf("expected *RouteError, got %T %v", err, err)
	}
	accounts, aerr := r.Store.ListAccounts()
	if aerr != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, aerr)
	}
	for _, acc := range accounts {
		if r.Pool.GatewayCooled(acc.ID) {
			t.Fatalf("content-signature 403 must not cool account %s: content repeats on any account", acc.Email)
		}
	}

	// 风控型：普通消息零特征 → 一次重试(新对话换号)共 2 次 403，两个被拦账号都进入冷却。
	r = new403Router()
	_, _, err = r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error { return nil })
	if _, ok := err.(*RouteError); !ok {
		t.Fatalf("expected *RouteError, got %T %v", err, err)
	}
	accounts, aerr = r.Store.ListAccounts()
	if aerr != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, aerr)
	}
	cooled := 0
	for _, acc := range accounts {
		if r.Pool.GatewayCooled(acc.ID) {
			cooled++
		}
	}
	if cooled != 2 {
		t.Fatalf("risk-control 403 should cool both attempted accounts (1 retry), got %d", cooled)
	}
}

// 网关(Cloudflare 403)零特征拦截只允许一次重试：重试(新对话换号)仍 403 即终止,共两次上游,
// 绝不切到第三个账号；被拦账号也不应被标记 error/exhausted(它健康,仅进入路由层冷却)。
func TestStreamGatewayBlockedDoesNotFailOver(t *testing.T) {
	r := newTestRouter(t)
	accounts, err := r.Store.ListAccounts()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	var calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		h := make(http.Header)
		h.Set("Server", "cloudflare")
		h.Set("Content-Type", "text/html; charset=UTF-8")
		h.Set("Cf-Ray", "a2d562e1bc2349d4-LAX")
		body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
		return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("gateway block should return an error, not fail over: res=%+v account=%+v err=%v", res, account, err)
	}
	re, ok := err.(*RouteError)
	if !ok || !re.GatewayBlocked {
		t.Fatalf("error should be *RouteError with GatewayBlocked=true, got %T %v", err, err)
	}
	// 关键：零特征 403 只允许一次重试(共 2 次上游)，重试仍 403 即终止,绝不逐账号空转。
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("gateway block must stop after one retry: expected 2 upstream calls, got %d", got)
	}
	if output.Len() != 0 {
		t.Fatalf("no output should be emitted to client on gateway block, got %q", output.String())
	}
	// 被拦账号不应被标记为 error/exhausted——它健康,只是被上游风控临时拦截。
	for _, a := range accounts {
		got, err := r.Store.GetAccount(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "error" || got.Status == "exhausted" {
			t.Fatalf("gateway-blocked account must not be marked %q (it is healthy, only cooled down)", got.Status)
		}
	}
}

// 所有账号都被网关拦截且尚未产出任何输出时,必须返回明确的错误(而非挂起或空流),
// 且不能向客户端 emit 任何增量,便于调用方(agent 终端)干净停止任务。
func TestStreamAllAccountsBlockedReturnsClearError(t *testing.T) {
	r := newTestRouter(t)
	var calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		h := make(http.Header)
		h.Set("Server", "cloudflare")
		h.Set("Content-Type", "text/html; charset=UTF-8")
		h.Set("Cf-Ray", "a2d562e1bc2349d4-LAX")
		body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
		return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("all-blocked should return an error with no result: res=%+v account=%+v err=%v", res, account, err)
	}
	if output.Len() != 0 {
		t.Fatalf("no output should be emitted to client on total block, got %q", output.String())
	}
	re, ok := err.(*RouteError)
	if !ok || !re.GatewayBlocked {
		t.Fatalf("error should be *RouteError with GatewayBlocked=true, got %T %v", err, err)
	}
	if !strings.Contains(re.Message, "403") {
		t.Fatalf("error message should clearly mention the 403 gateway block, got %q", re.Message)
	}
	// 零特征 403：允许一次重试（新对话换号），2 次均被拦后终止，不再空转。
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("zero-signature gateway block should retry exactly once: expected 2 upstream calls, got %d", got)
	}
}

func TestStreamBlockedAccountSwitchesAndDisablesBeforeOutput(t *testing.T) {
	r := newTestRouter(t)
	accounts, err := r.Store.ListAccounts()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	for i, account := range accounts {
		tokens, _ := json.Marshal(provider.Tokens{AccessToken: "token-" + string(rune('1'+i)), UserID: "u", WorkspaceID: "w"})
		if err := r.Store.UpdateTokens(account.ID, string(tokens)); err != nil {
			t.Fatal(err)
		}
	}
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := "data: {\"eventType\":\"usage\",\"data\":{\"limit\":50000,\"usage\":50000,\"usageState\":\"BLOCKED\"}}\n\n"
		if req.Header.Get("x-access-token") == "token-2" {
			body = "data: {\"eventType\":\"usage\",\"data\":{\"limit\":50000,\"usage\":1000,\"usageState\":\"AVAILABLE\"}}\n\n" +
				"data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-2\"}}\n\n" +
				"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"ok\"}}\n\n" +
				"data: [DONE]\n\n"
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err != nil || res == nil || !res.Success || account == nil || account.ID != accounts[1].ID {
		t.Fatalf("stream did not fail over: res=%+v account=%+v err=%v", res, account, err)
	}
	if output.String() != "ok" {
		t.Fatalf("client saw intermediate quota error: %q", output.String())
	}
	// 实时聊天收到 BLOCKED：账号被网关封锁属账号异常，须标 error 并停用（从选号池摘除），
	// 立即 failover 到下一个账号——而不是按额度耗尽（exhausted）处理，即使余量恰好算到 0。
	blocked, err := r.Store.GetAccount(accounts[0].ID)
	if err != nil || blocked.Status != "error" || blocked.Enabled {
		t.Fatalf("first account was not marked error+disabled on BLOCKED: %+v err=%v", blocked, err)
	}
}

// 网关拦截(Cloudflare 403)诱因是有状态的 WAF/Bot 风控而非账号身份。契约:零特征拦截允许
// 一次重试后终止(共两次上游),不逐号空转;被拦账号仅进入网关冷却窗口,不判为异常。
func TestStreamGatewayBlockedReturnsImmediatelyNoRetry(t *testing.T) {
	r := newTestRouter(t)
	accounts, err := r.Store.ListAccounts()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}

	var calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		// 请求体带巨型 tool_result → Cloudflare 403(HTML 风控页)。
		h := make(http.Header)
		h.Set("Server", "cloudflare")
		h.Set("Content-Type", "text/html; charset=UTF-8")
		h.Set("Cf-Ray", "a2d562e1bc2349d4-LAX")
		body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
		return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	big := strings.Repeat("<div>dump</div>", 4000) // ~60KB 类 HTML 的巨型 tool_result
	toolResult := `[{"type":"tool_result","tool_use_id":"toolu_1","content":` + jsonStringT(t, big) + `}]`
	msgs := []provider.ChatMessage{
		mustMsg(t, "user", "read the file"),
		{Role: "assistant", Content: mustRaw(t, `"reading"`)},
		{Role: "user", Content: mustRaw(t, toolResult)},
	}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: msgs,
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("gateway block must return an error with no result/account: res=%+v account=%+v err=%v", res, account, err)
	}
	re, ok := err.(*RouteError)
	if !ok || !re.GatewayBlocked {
		t.Fatalf("error should be *RouteError with GatewayBlocked=true, got %T %v", err, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("gateway block must stop after one retry: expected 2 upstream calls, got %d", got)
	}
	if output.Len() != 0 {
		t.Fatalf("no output should be emitted to client on gateway block, got %q", output.String())
	}
	// 诱因是请求体而非账号:被拦账号不应被标记异常/耗尽,仍健康(仅进入网关冷却窗口)。
	same, err := r.Store.GetAccount(accounts[0].ID)
	if err != nil || same.Status == "error" || same.Status == "exhausted" {
		t.Fatalf("blocked account must stay healthy (request-triggered, not account fault): %+v err=%v", same, err)
	}
}

// 回归：续聊(有可复用历史)遇网关 403 时，绝不换号——换号会丢失 Postman 服务端会话上下文
// （请求被降级为 USER_QUERY 且历史被截断），并把同一错误传染给其它健康账号。零特征 403 的
// 那一次重试经会话粘性回原号。同时断言 req.Messages 未被就地改写（保住指纹 → 维持
// TOOL_RESPONSE + conversationId），即从源头杜绝「压缩改写 → 破坏指纹 → 换号 → 降级失忆」。
func TestStreamGatewayBlockedContinuationNoFailoverNoDowngrade(t *testing.T) {
	r := newTestRouter(t)
	accounts, err := r.Store.ListAccounts()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	for i, account := range accounts {
		tokens, _ := json.Marshal(provider.Tokens{AccessToken: "token-" + string(rune('1'+i)), UserID: "u", WorkspaceID: "w"})
		if err := r.Store.UpdateTokens(account.ID, string(tokens)); err != nil {
			t.Fatal(err)
		}
	}
	acc1 := accounts[0] // token-1：会话粘性绑定的原账号

	// 建立会话粘性：记住 token-1 账号上「首轮 + 助手回复」这段前缀的会话。
	first := []provider.ChatMessage{mustMsg(t, "user", "read the file")}
	r.Provider.RememberConversation(acc1.ID, first, &provider.Result{ConversationID: "conv-A", Content: "reading"})

	big := strings.Repeat("<div>dump</div>", 4000) // ~60KB 类 HTML 的巨型 tool_result
	toolResult := `[{"type":"tool_result","tool_use_id":"toolu_1","content":` + jsonStringT(t, big) + `}]`
	cont := []provider.ChatMessage{
		first[0],
		{Role: "assistant", Content: mustRaw(t, `"reading"`)},
		{Role: "user", Content: mustRaw(t, toolResult)}, // Anthropic tool_result → 可复用历史
	}

	var token1Calls, token2Calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("x-access-token") == "token-2" {
			atomic.AddInt32(&token2Calls, 1) // 若发生此调用即说明错误地换了号
			body := "data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-B\"}}\n\n" +
				"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"WRONG\"}}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		// 原账号(token-1)始终被 Cloudflare 拦截:零特征 403 原号重试一次,仍拦即终止、绝不换号。
		atomic.AddInt32(&token1Calls, 1)
		h := make(http.Header)
		h.Set("Server", "cloudflare")
		h.Set("Content-Type", "text/html; charset=UTF-8")
		h.Set("Cf-Ray", "a2d562e1bc2349d4-LAX")
		body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
		return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	req := &provider.ChatRequest{Model: "claude-opus-4-8", Messages: cont}
	var output strings.Builder
	res, account, err := r.Stream(context.Background(), req, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("continuation gateway block must return an error with no result/account: res=%+v account=%+v err=%v", res, account, err)
	}
	re, ok := err.(*RouteError)
	if !ok || !re.GatewayBlocked {
		t.Fatalf("error should be *RouteError with GatewayBlocked=true, got %T %v", err, err)
	}
	if n := atomic.LoadInt32(&token2Calls); n != 0 {
		t.Fatalf("must NOT fail over to the other account on a continuation (would lose server-side session), but token-2 was called %d time(s)", n)
	}
	if n := atomic.LoadInt32(&token1Calls); n != 2 {
		t.Fatalf("expected exactly 2 calls to the original account (zero-signature 403 retries once on the same account), got %d", n)
	}
	if output.Len() != 0 {
		t.Fatalf("no output should be emitted to client on gateway block, got %q", output.String())
	}
	// 关键：req.Messages 不得被就地改写——改写会破坏会话指纹并触发降级为 USER_QUERY。
	if len(req.Messages) != 3 {
		t.Fatalf("req.Messages must not be mutated, got %d messages", len(req.Messages))
	}
	if string(req.Messages[2].Content) != toolResult {
		t.Fatalf("tool_result content must stay intact (no compaction/mutation), got %q", string(req.Messages[2].Content))
	}
}

// 网关拦截(Cloudflare 403)不逐账号 failover:换号既大概率同样被 WAF 拦、又会把错误传染给
// 一批健康账号并丢失会话上下文。无论号池多大,零特征 403 只重试一次(共两次上游)后立即终止。
func TestStreamGatewayBlockedNoFailoverAcrossAccounts(t *testing.T) {
	r := newTestRouter(t)
	// 扩充到 5 个账号:即便有大量可用号,网关拦截也不得逐个换号兜底。
	tok, _ := json.Marshal(provider.Tokens{AccessToken: "tok", UserID: "u", WorkspaceID: "w"})
	for _, email := range []string{"a3@test.com", "a4@test.com", "a5@test.com"} {
		if _, err := r.Store.UpsertAccount(email, "", string(tok), "manual"); err != nil {
			t.Fatal(err)
		}
	}

	var calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		h := make(http.Header)
		h.Set("Server", "cloudflare")
		h.Set("Content-Type", "text/html; charset=UTF-8")
		h.Set("Cf-Ray", "a2d562e1bc2349d4-LAX")
		body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
		return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("gateway block should return an error with no result: res=%+v account=%+v err=%v", res, account, err)
	}
	re, ok := err.(*RouteError)
	if !ok || !re.GatewayBlocked {
		t.Fatalf("error should be *RouteError with GatewayBlocked=true, got %T %v", err, err)
	}
	// 关键断言:无论号池多大,零特征 403 只重试一次(共 2 次上游)、绝不逐号空转。
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("gateway block must stop after one retry regardless of pool size: expected 2 upstream calls, got %d", got)
	}
	if output.Len() != 0 {
		t.Fatalf("no output should be emitted to client on gateway block, got %q", output.String())
	}
}

func jsonStringT(t *testing.T, s string) string {
	t.Helper()
	b, _ := json.Marshal(s)
	return string(b)
}

// upstreamPolicyErrorSSE 是线上 trace 里那条上游自身模型故障的原文（Postman → Bedrock）：
// 服务端先确认会话存在，随后 failure 事件报 Policy Error。account/请求都没问题。
const upstreamPolicyErrorSSE = "data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-A\",\"interactionCount\":21}}\n\n" +
	"data: {\"eventType\":\"failure\",\"data\":{\"errorType\":\"LLM_STREAM_ERROR\"," +
	"\"message\":\"LLM stream error: Failed after 3 attempts. Last error: AI_APICallError: Policy Error\"," +
	"\"userMessage\":\"That was unexpected :(. Try starting a new chat, or remove any configured MCP servers.\"}}\n\n" +
	"data: [DONE]\n\n"

// continuationMsgs 构造一段「有可复用历史」的续聊（Anthropic tool_result），
// 即 Postman 服务端已有会话、换号必然丢上下文的那一类请求。
func continuationMsgs(t *testing.T) []provider.ChatMessage {
	t.Helper()
	return []provider.ChatMessage{
		mustMsg(t, "user", "read the file"),
		{Role: "assistant", Content: mustRaw(t, `"reading"`)},
		{Role: "user", Content: mustRaw(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"File created successfully"}]`)},
	}
}

// 回归（线上事故 trace 179ede71）：上游自己调模型失败（LLM_STREAM_ERROR / Policy Error）时，
// 续聊必须钉住原账号原地重试——绝不换号。换号会让服务端 conversationId 失效，请求降级为
// 只剩几百字节的 USER_QUERY（失忆），失忆后必然再次失败并把同一个错误传染给下一个账号。
// 同时：账号绝不能被标记为 error（那会把它踢出 ActiveAccounts，一次上游抖动毁一批号）。
func TestStreamUpstreamModelFailureContinuationStaysOnOriginalAccount(t *testing.T) {
	r := newTestRouter(t)
	accounts, err := r.Store.ListAccounts()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	for i, account := range accounts {
		tokens, _ := json.Marshal(provider.Tokens{AccessToken: "token-" + string(rune('1'+i)), UserID: "u", WorkspaceID: "w"})
		if err := r.Store.UpdateTokens(account.ID, string(tokens)); err != nil {
			t.Fatal(err)
		}
	}
	acc1 := accounts[0] // 会话粘性绑定的原账号

	cont := continuationMsgs(t)
	// 建立粘性：token-1 上已有「首轮 + 助手回复」这段前缀的服务端会话。
	r.Provider.RememberConversation(acc1.ID, cont[:1], &provider.Result{ConversationID: "conv-A", Content: "reading"})

	var token1Calls, token2Calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("x-access-token") == "token-2" {
			atomic.AddInt32(&token2Calls, 1) // 发生即说明错误地换了号 → 上下文已丢
			body := "data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-B\"}}\n\n" +
				"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"AMNESIAC\"}}\n\n" +
				"data: [DONE]\n\n"
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		if atomic.AddInt32(&token1Calls, 1) == 1 {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(upstreamPolicyErrorSSE)), Request: req}, nil
		}
		body := "data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-A\"}}\n\n" +
			"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"ok\"}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: cont,
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err != nil || res == nil || !res.Success || account == nil {
		t.Fatalf("upstream failure should retry on the same account and recover: res=%+v account=%+v err=%v", res, account, err)
	}
	if n := atomic.LoadInt32(&token2Calls); n != 0 {
		t.Fatalf("must NOT fail over on a continuation (would drop the server-side session and answer amnesiac), but the other account was called %d time(s)", n)
	}
	if account.ID != acc1.ID {
		t.Fatalf("must recover on the ORIGINAL account %d, got %d", acc1.ID, account.ID)
	}
	if n := atomic.LoadInt32(&token1Calls); n != 2 {
		t.Fatalf("expected exactly 2 calls to the original account (policy error, then retry success), got %d", n)
	}
	if output.String() != "ok" {
		t.Fatalf("client should see the original account's output, got %q", output.String())
	}
	// 上游模型故障与账号健康无关：绝不能写成 error（会被 ActiveAccounts 过滤掉，并打断会话粘性）。
	same, err := r.Store.GetAccount(acc1.ID)
	if err != nil || same.Status != "active" {
		t.Fatalf("account must stay active after an upstream-side model failure, got %+v err=%v", same, err)
	}
	if alerts, err := r.Store.ListAlerts("", 10); err != nil || len(alerts) != 0 {
		t.Fatalf("upstream model failure must not raise an account alert, got %d alert(s) err=%v", len(alerts), err)
	}
}

// 上游持续报 Policy Error（换号也没用，因为故障在上游侧）：必须把重试全部消耗在原账号上、
// 一个都不传染给其他账号，返回的错误里要带上真正的根因，且没有任何账号被打成 error。
func TestStreamPersistentUpstreamFailureDoesNotSpreadAcrossAccounts(t *testing.T) {
	r := newTestRouter(t)
	accounts, err := r.Store.ListAccounts()
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts=%v err=%v", accounts, err)
	}
	for i, account := range accounts {
		tokens, _ := json.Marshal(provider.Tokens{AccessToken: "token-" + string(rune('1'+i)), UserID: "u", WorkspaceID: "w"})
		if err := r.Store.UpdateTokens(account.ID, string(tokens)); err != nil {
			t.Fatal(err)
		}
	}
	acc1 := accounts[0]
	cont := continuationMsgs(t)
	r.Provider.RememberConversation(acc1.ID, cont[:1], &provider.Result{ConversationID: "conv-A", Content: "reading"})

	var token1Calls, token2Calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("x-access-token") == "token-2" {
			atomic.AddInt32(&token2Calls, 1)
		} else {
			atomic.AddInt32(&token1Calls, 1)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(upstreamPolicyErrorSSE)), Request: req}, nil
	})}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: cont,
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("persistent upstream failure should return an error: res=%+v account=%+v err=%v", res, account, err)
	}
	if n := atomic.LoadInt32(&token2Calls); n != 0 {
		t.Fatalf("a continuation must never be retried on another account, but it was called %d time(s)", n)
	}
	if n := atomic.LoadInt32(&token1Calls); n < 2 {
		t.Fatalf("retries should be spent on the sticky account, got only %d call(s)", n)
	}
	if !strings.Contains(err.Error(), "Policy Error") {
		t.Fatalf("returned error must carry the upstream root cause, got %q", err.Error())
	}
	// 两个账号都必须保持可用：故障在上游侧，不该有任何号被踢出池。
	for _, a := range accounts {
		got, err := r.Store.GetAccount(a.ID)
		if err != nil || got.Status != "active" {
			t.Fatalf("account %d must stay active after upstream-side failures, got %+v err=%v", a.ID, got, err)
		}
	}
}

// 回归（线上事故 trace 3898bd74）：客户端断开后必须立刻停。那次非流式请求的 attempt 1 失败时
// 客户端已经走了，可循环还继续换了 3 个账号——每次都在 ~240ms 内报 "Client disconnected"，
// 白建 3 个 Postman 会话、把 3 个账号记成异常，最后还用 "Client disconnected" 覆盖掉真正的
// 首因（Policy Error），对外成了 "All accounts failed. Last error: Client disconnected"。
// 这里走非流式 Chat（emitted 恒 false，已有的 abort() 守卫不生效），才真正验证 ctx 守卫。
func TestChatStopsImmediatelyWhenClientIsGone(t *testing.T) {
	r := newTestRouter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		cancel() // 客户端在本次尝试进行中断开
		return nil, context.Canceled
	})}

	res, account, err := r.Chat(ctx, &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("a gone client should end the request: res=%+v account=%+v err=%v", res, account, err)
	}
	// 关键断言：不得在客户端走后继续换号空转（此前会一路耗尽 retry_count 个账号）。
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("must not retry on other accounts after the client is gone, got %d upstream calls", got)
	}
	accounts, err := r.Store.ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a.Status == "error" {
			t.Fatalf("a client disconnect must not be blamed on account %d", a.ID)
		}
	}
	if alerts, err := r.Store.ListAlerts("", 10); err != nil || len(alerts) != 0 {
		t.Fatalf("a client disconnect must not raise account alerts, got %d err=%v", len(alerts), err)
	}
}

// 流式路径下客户端写回失败（emit 报错）同样必须一次就停——此处由已有的 abort() 守卫兜住，
// 与上面的 ctx 守卫互补。
func TestStreamClientDisconnectStopsImmediately(t *testing.T) {
	r := newTestRouter(t)
	var calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		body := "data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-1\"}}\n\n" +
			"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"ok\"}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(provider.Delta) error {
		return io.ErrClosedPipe // 客户端已走：写回失败
	})
	if err == nil || res != nil || account != nil {
		t.Fatalf("client disconnect should end the request: res=%+v account=%+v err=%v", res, account, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("must not retry after the client is gone, got %d upstream calls", got)
	}
	if accounts, err := r.Store.ListAccounts(); err == nil {
		for _, a := range accounts {
			if a.Status == "error" {
				t.Fatalf("a client disconnect must not be blamed on account %d", a.ID)
			}
		}
	}
}

// 会话粘性必须容忍 status=="error"：续聊的服务端会话只在这个账号上，宁可原号失败也不要
// 静默换号交付失忆答案。（曾经的级联根因：一次上游错误把号写成 error → 粘性失效 → 换号降级。）
func TestStickyToleratesErrorStatusButNotExhausted(t *testing.T) {
	r := newTestRouter(t)
	msgsFirst := []provider.ChatMessage{mustMsg(t, "user", "agent D hello")}
	acc1, _, err := r.pickAccount(nil, msgsFirst, false)
	if err != nil {
		t.Fatal(err)
	}
	r.Pool.Done(acc1.ID)
	r.Provider.RememberConversation(acc1.ID, msgsFirst, &provider.Result{ConversationID: "conv-D1", Content: "hi"})

	cont := []provider.ChatMessage{
		msgsFirst[0],
		{Role: "assistant", Content: mustRaw(t, `"hi"`)},
		mustMsg(t, "user", "more"),
	}

	if err := r.Store.SetAccountStatus(acc1.ID, "error", "some earlier failure"); err != nil {
		t.Fatal(err)
	}
	stuck, used, err := r.pickAccount(nil, cont, false)
	if err != nil {
		t.Fatal(err)
	}
	if used || stuck.ID != acc1.ID {
		t.Fatalf("continuation must still stick to account %d despite status=error (got %d, fromPool=%v)", acc1.ID, stuck.ID, used)
	}

	// 额度确定为 0 则相反：那种号发出去必然拿不到结果，必须回退轮询。
	if err := r.Store.SetAccountStatus(acc1.ID, "exhausted", "quota exceeded"); err != nil {
		t.Fatal(err)
	}
	if _, used, err := r.pickAccount(nil, cont, false); err != nil || !used {
		t.Fatalf("exhausted sticky account must fall back to the pool: used=%v err=%v", used, err)
	}
}

// 出口序号必须与全局 attempt 解耦：同账号重试递增以轮换代理出口 IP，
// 而一旦跨账号 failover 换号就归零——保证换号后新账号仍从自身粘性出口走代理池，
// 绝不因全局重试数堆高使 seq>=N 而在 selectFor 里回退本机直连（换号多因 403，直连必再被拦）。
func TestNextEgressSeqResetsOnAccountSwitch(t *testing.T) {
	// 首次尝试：始终从 0（账号粘性出口）开始。
	if got := nextEgressSeq(0, 0, 1, true); got != 0 {
		t.Fatalf("first attempt must start at egress seq 0, got %d", got)
	}
	// 同账号连续重试：递增，轮换到下一个出口 IP。
	if got := nextEgressSeq(0, 1, 1, false); got != 1 {
		t.Fatalf("same-account retry must rotate egress (0->1), got %d", got)
	}
	if got := nextEgressSeq(4, 1, 1, false); got != 5 {
		t.Fatalf("same-account retry must keep incrementing (4->5), got %d", got)
	}
	// 换号：无论上一账号的出口序号堆到多高，新账号都必须归零，
	// 这样 selectFor 会走 (stickyBase(newAcc)+0)%N 命中代理池，而非直连。
	if got := nextEgressSeq(9, 1, 2, false); got != 0 {
		t.Fatalf("account switch must reset egress seq to 0 so the new account still uses the proxy pool, got %d", got)
	}
	if got := nextEgressSeq(100, 7, 3, false); got != 0 {
		t.Fatalf("account switch must reset egress seq regardless of how high the previous seq was, got %d", got)
	}
}

// 零特征(概率型)403 的一次重试应能恢复：第一次上游 403(Cloudflare 风控页)、第二次成功，
// 客户端拿到正常输出且不感知中间的 403。这是「网关内一次重试」修复的核心价值路径
// （实测 2026-09-10：同账号+同出口+同 body 秒级重发即成功，单发拦截率 ~8%）。
func TestStreamZeroSignature403RetryRecovers(t *testing.T) {
	r := newTestRouter(t)
	var calls int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			h := make(http.Header)
			h.Set("Server", "cloudflare")
			h.Set("Content-Type", "text/html; charset=UTF-8")
			h.Set("Cf-Ray", "a2d562e1bc2349d4-LAX")
			body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
			return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		body := "data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-1\"}}\n\n" +
			"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"ok\"}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	var output strings.Builder
	res, _, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("retry after zero-signature 403 should recover: res=%+v err=%v", res, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 upstream calls (403 then success), got %d", got)
	}
	if output.String() != "ok" {
		t.Fatalf("client should see the retried output, got %q", output.String())
	}
}
