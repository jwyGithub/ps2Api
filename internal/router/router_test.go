package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"postman2api-go/internal/provider"
	"postman2api-go/internal/store"
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
