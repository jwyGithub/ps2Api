package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ps2api/internal/provider"
	"ps2api/internal/router"
	"ps2api/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnthropicToolUseIsPreservedForConversationReuse(t *testing.T) {
	content, _ := json.Marshal([]map[string]interface{}{
		{"type": "text", "text": "I will check that."},
		{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": map[string]interface{}{"city": "Tokyo"}},
	})

	msg := anthropicMessageToOpenAI(AnthropicMsg{Role: "assistant", Content: content})
	if string(msg.Content) != `"I will check that."` {
		t.Fatalf("text content = %s", msg.Content)
	}
	var calls []map[string]interface{}
	if err := json.Unmarshal(msg.ToolCalls, &calls); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0]["id"] != "call_1" {
		t.Fatalf("tool calls = %s", msg.ToolCalls)
	}
	fn, _ := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Tokyo"}` {
		t.Fatalf("function = %#v", fn)
	}
}

func TestOpenAIToAnthropicPreservesThinking(t *testing.T) {
	res := &provider.Result{ReasoningContent: "checking files", Content: "done"}

	out := openAIToAnthropic(res, "claude-sonnet-4-6")
	blocks := out["content"].([]map[string]interface{})
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %#v", blocks)
	}
	if blocks[0]["type"] != "thinking" || blocks[0]["thinking"] != "checking files" || blocks[0]["signature"] != "" {
		t.Fatalf("thinking block = %#v", blocks[0])
	}
	if blocks[1]["type"] != "text" || blocks[1]["text"] != "done" {
		t.Fatalf("text block = %#v", blocks[1])
	}
}

func TestAnthropicStreamPreservesThinking(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UpsertAccount("test@example.com", "", `{"access_token":"token","user_id":"user","workspace_id":"workspace"}`, "manual"); err != nil {
		t.Fatal(err)
	}

	rt := router.New(db)
	rt.Provider.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := "data: {\"eventType\":\"thinkingChunk\",\"data\":{\"thinkingContent\":\"checking\"}}\n" +
			"data: {\"eventType\":\"textChunk\",\"data\":{\"textContent\":\"done\"}}\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	server := &Server{Store: db, Router: rt}
	mux := http.NewServeMux()
	server.Register(mux)

	request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"test"}]}`))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	body := response.Body.String()
	thinking := `"delta":{"thinking":"checking","type":"thinking_delta"}`
	signature := `"delta":{"signature":"","type":"signature_delta"}`
	text := `"delta":{"text":"done","type":"text_delta"}`
	if response.Code != http.StatusOK || !strings.Contains(body, thinking) || !strings.Contains(body, signature) || !strings.Contains(body, text) || strings.Index(body, thinking) > strings.Index(body, signature) || strings.Index(body, signature) > strings.Index(body, text) {
		t.Fatalf("unexpected anthropic stream (%d): %s", response.Code, body)
	}
}
