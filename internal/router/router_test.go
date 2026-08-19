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
	acc1, poolUsed, err := r.pickAccount(nil, msgsFirst)
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
	if acc, used, err := r.pickAccount(nil, other); err != nil || !used {
		t.Fatalf("other new chat should rotate: acc=%v used=%v err=%v", acc, used, err)
	}

	// Agent A 续聊：必须粘回账号1
	cont := []provider.ChatMessage{
		msgsFirst[0],
		{Role: "assistant", Content: mustRaw(t, `"hi"`)},
		mustMsg(t, "user", "continue please"),
	}
	stuck, used, err := r.pickAccount(nil, cont)
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
	acc1, _, err := r.pickAccount(nil, msgsFirst)
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
	stuck, used, err := r.pickAccount(map[int64]bool{acc1.ID: true}, cont)
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
	acc1, _, err := r.pickAccount(nil, msgsFirst)
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
	if _, used, err := r.pickAccount(nil, cont); err != nil || !used {
		t.Fatalf("disabled sticky account must fall back to pool: used=%v err=%v", used, err)
	}
}

// Cloudflare 403(HTML 风控拦截)以前被当成 RequestRejected 直接返回,任务卡死。
// 现在归类为 GatewayBlocked:排除被拦账号并 failover 到其他账号,第二次即恢复成功——且写入一条告警。
func TestStreamCloudflare403RetriesAndRecovers(t *testing.T) {
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
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err != nil || res == nil || !res.Success || account == nil {
		t.Fatalf("Cloudflare 403 should retry and recover: res=%+v account=%+v err=%v", res, account, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly 2 upstream calls (403 then success), got %d", got)
	}
	if output.String() != "ok" {
		t.Fatalf("client should see recovered output, got %q", output.String())
	}
	if alerts, err := r.Store.ListAlerts("", 10); err != nil || len(alerts) == 0 {
		t.Fatalf("expected a gateway-rejected alert to be recorded: alerts=%v err=%v", alerts, err)
	}
}

// 403 集中在被烧账号(token-1)时,应 failover 到健康账号(token-2)成功,且不把被拦账号
// 标记成 error/exhausted(账号本身健康,仅进入路由层冷却)。验证「换账号」这一核心缓解手段。
func TestStreamGatewayBlockedFailsOverToHealthyAccount(t *testing.T) {
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
	var blocked int32
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("x-access-token") == "token-1" {
			atomic.AddInt32(&blocked, 1)
			h := make(http.Header)
			h.Set("Server", "cloudflare")
			h.Set("Content-Type", "text/html; charset=UTF-8")
			h.Set("Cf-Ray", "a2d562e1bc2349d4-LAX")
			body := "<!doctype html><html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>"
			return &http.Response{StatusCode: 403, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}
		body := "data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-2\"}}\n\n" +
			"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"ok\"}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	var output strings.Builder
	res, account, err := r.Stream(context.Background(), &provider.ChatRequest{
		Model: "claude-opus-4-8", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")},
	}, func(delta provider.Delta) error {
		output.WriteString(delta.Content)
		return nil
	})
	if err != nil || res == nil || !res.Success || account == nil {
		t.Fatalf("gateway block should fail over and recover: res=%+v account=%+v err=%v", res, account, err)
	}
	if output.String() != "ok" {
		t.Fatalf("client should see recovered output, got %q", output.String())
	}
	// 被拦账号(token-1)不应被标记为 error/exhausted——它健康,只是被上游风控临时拦截。
	blockedAcc, err := r.Store.GetAccount(accounts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if blockedAcc.Status == "error" || blockedAcc.Status == "exhausted" {
		t.Fatalf("gateway-blocked account must not be marked %q (it is healthy, only cooled down)", blockedAcc.Status)
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
	// failover 逐个排除被拦账号：2 个账号各被试一次(共 2 次 403)后账号耗尽,不再空转。
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("should try each of the 2 accounts once before giving up, got %d upstream calls", got)
	}
}

func TestStreamQuotaExhaustedSwitchesAccountBeforeOutput(t *testing.T) {
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
	exhausted, err := r.Store.GetAccount(accounts[0].ID)
	if err != nil || exhausted.Status != "exhausted" || exhausted.QuotaRemaining != 0 {
		t.Fatalf("first account was not marked exhausted: %+v err=%v", exhausted, err)
	}
}
