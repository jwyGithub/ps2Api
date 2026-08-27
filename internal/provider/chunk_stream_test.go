package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"ps2api/internal/store"
)

type chunkRoundTrip func(*http.Request) (*http.Response, error)

func (f chunkRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestStreamChunkedOrchestration 验证分片续传：超长 USER_QUERY 被切成 N 片，前 N-1 片作为
// 前置片顺序发出（带 ACK 包裹、conversationId 链式续接、回复不 emit），最后一片带末片包裹并
// 正常 emit；res.ConversationID 落在最后一片返回的会话 ID 上。
func TestStreamChunkedOrchestration(t *testing.T) {
	p := New()

	var mu sync.Mutex
	type capture struct {
		query  string
		convID interface{}
	}
	var calls []capture

	p.Client = &http.Client{Transport: chunkRoundTrip(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		var body struct {
			Input struct {
				Query          string      `json:"query"`
				ConversationID interface{} `json:"conversationId"`
			} `json:"input"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		n := len(calls)
		calls = append(calls, capture{query: body.Input.Query, convID: body.Input.ConversationID})
		mu.Unlock()

		sse := fmt.Sprintf("data: {\"eventType\":\"conversation\",\"data\":{\"id\":\"conv-%d\"}}\n\n"+
			"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"OUT%d\"}}\n\n"+
			"data: [DONE]\n\n", n, n)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse)), Request: req}, nil
	})}

	// ~27000 个无换行字符 → 预算 9644 下硬切成 3 片。
	huge := strings.Repeat("a", 27000)
	hugeJSON, _ := json.Marshal(huge)
	req := &ChatRequest{
		Model:    "test",
		Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(hugeJSON)}},
	}
	tokens := &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}

	var emitted strings.Builder
	res := &Result{}
	err := p.streamInternal(context.Background(), &store.Account{ID: 1}, req, tokens, "test", func(d Delta) error {
		emitted.WriteString(d.Content)
		return nil
	}, res)
	if err != nil || !res.Success {
		t.Fatalf("chunked stream should succeed: err=%v res=%+v", err, res)
	}

	if len(calls) != 3 {
		t.Fatalf("expected 3 upstream calls (2 priming + 1 final), got %d", len(calls))
	}
	// 前置片 1/3：conversationId 为 null（冷启动），带 ACK 包裹。
	if !strings.HasPrefix(calls[0].query, "[大输入分片 1/3]") {
		t.Fatalf("call 0 should be priming chunk 1/3, got prefix %q", head(calls[0].query))
	}
	if calls[0].convID != nil {
		t.Fatalf("call 0 conversationId should be null on cold start, got %v", calls[0].convID)
	}
	// 前置片 2/3：conversationId 续接上一片返回的 conv-0。
	if !strings.HasPrefix(calls[1].query, "[大输入分片 2/3]") {
		t.Fatalf("call 1 should be priming chunk 2/3, got prefix %q", head(calls[1].query))
	}
	if calls[1].convID != "conv-0" {
		t.Fatalf("call 1 should chain conversationId conv-0, got %v", calls[1].convID)
	}
	// 最后一片：末片包裹，conversationId 续接 conv-1。
	if !strings.HasPrefix(calls[2].query, "[大输入分片 3/3，最后一部分]") {
		t.Fatalf("call 2 should be final chunk 3/3, got prefix %q", head(calls[2].query))
	}
	if calls[2].convID != "conv-1" {
		t.Fatalf("call 2 should chain conversationId conv-1, got %v", calls[2].convID)
	}
	// 只有最后一片的输出被 emit 给客户端；前置片的 ACK 回复被丢弃。
	if emitted.String() != "OUT2" {
		t.Fatalf("client should only see final chunk output OUT2, got %q", emitted.String())
	}
	// res 会话 ID 落在最后一片返回值，供续聊沉淀。
	if res.ConversationID != "conv-2" {
		t.Fatalf("res.ConversationID should be conv-2 (final round), got %q", res.ConversationID)
	}
}

func head(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40])
	}
	return s
}
