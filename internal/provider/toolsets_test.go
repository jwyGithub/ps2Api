// toolsets_test.go —— ToolsetsProvider 透传的关键路径回归：
// 200 + error JSON（非 SSE）必须分类为 RateLimited 供 api 层换号重试，
// 而不是当成 Success 让客户端收到空流（2026-09-24 修复引入）。
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
