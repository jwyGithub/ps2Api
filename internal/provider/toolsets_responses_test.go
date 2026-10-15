// toolsets_responses_test.go —— Responses 端关键回归：
// 1) assistant 消息 content 的 input_text 必须出站前改写为 output_text（上游对
//    assistant+input_text 回 503+upstream_unavailable 伪装错误，2026-10-15 实测矩阵）；
// 2) 503+upstream_unavailable 必须分类为 RequestRejected（内容错误换号无用），
//    不得 RateLimited 进限频重试把号池打挂。
package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesUpstreamRewritesAssistantInputText(t *testing.T) {
	var gotBody string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","output":[],"usage":{"output_tokens":1}}`))
	})
	raw := `{"input":[
		{"content":[{"text":"q","type":"input_text"}],"role":"user","type":"message"},
		{"content":[{"text":"","type":"input_text"}],"role":"assistant","type":"message"},
		{"content":[{"text":"q2","type":"input_text"}],"role":"user","type":"message"}
	],"model":"gpt-6.1-sol","reasoning":{"effort":"high"},"stream":true,"status":"completed"}`

	acc := toolsetsTestAccount(t)
	res, _ := tp.ChatResponses(t.Context(), acc, []byte(raw), "gpt-6.1-sol", 0)
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gotBody), &m); err != nil {
		t.Fatalf("bad upstream body: %v", err)
	}
	if _, ok := m["status"]; ok {
		t.Fatalf("item-level status passthrough must not leak to top level; got %s", gotBody)
	}
	var items []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(m["input"], &items); err != nil {
		t.Fatalf("input not parseable: %v", err)
	}
	if items[1].Role != "assistant" {
		t.Fatalf("assistant item missing, got %+v", items)
	}
	for _, c := range items[1].Content {
		if c.Type != "output_text" {
			t.Fatalf("assistant content type must be rewritten input_text->output_text, got %s", c.Type)
		}
	}
	// user 的 input_text 不能被动到
	if items[0].Content[0].Type != "input_text" {
		t.Fatalf("user content must stay input_text, got %s", items[0].Content[0].Type)
	}
}

func TestResponsesUpstreamUnavailableClassifiedRejected(t *testing.T) {
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"code":"upstream_unavailable","message":"The model provider is unavailable. Please try again."}}`))
	})
	raw := `{"input":[{"content":[{"text":"hi","type":"input_text"}],"role":"user"}],"model":"gpt-6.1-sol","stream":true}`
	acc := toolsetsTestAccount(t)
	res := tp.StreamChatResponses(t.Context(), acc, []byte(raw), "gpt-6.1-sol", 0, func(string, []byte) error { return nil })
	if res.Success {
		t.Fatalf("must not be success")
	}
	if !res.RequestRejected {
		t.Fatalf("503+upstream_unavailable must classify RequestRejected (content error), got RateLimited=%v err=%s", res.RateLimited, res.Error)
	}
	if res.RateLimited {
		t.Fatalf("must not classify RateLimited — that drives pointless account rotation")
	}
	if !strings.Contains(res.Error, "upstream_unavailable") {
		t.Fatalf("error should carry upstream code, got %s", res.Error)
	}
}

func TestResponsesRateLimitedStillClassifiedRateLimited(t *testing.T) {
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"rate_limited","message":"Too many requests."}}`))
	})
	raw := `{"input":[{"content":[{"text":"hi","type":"input_text"}],"role":"user"}],"model":"gpt-6.1-sol","stream":true}`
	acc := toolsetsTestAccount(t)
	res := tp.StreamChatResponses(t.Context(), acc, []byte(raw), "gpt-6.1-sol", 0, func(string, []byte) error { return nil })
	if !res.RateLimited || res.RequestRejected {
		t.Fatalf("429 must stay RateLimited, got RL=%v RR=%v", res.RateLimited, res.RequestRejected)
	}
}

func TestResponsesSanitizeSkipsStringInput(t *testing.T) {
	// input 为纯字符串（easy-input 形态）时改写必须 no-op 不报错。
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"resp_2","output":[],"usage":{"output_tokens":1}}`))
	})
	raw := `{"input":"hi","model":"gpt-6.1-sol","stream":false}`
	acc := toolsetsTestAccount(t)
	res, _ := tp.ChatResponses(t.Context(), acc, []byte(raw), "gpt-6.1-sol", 0)
	if !res.Success {
		t.Fatalf("string input must pass through, got error: %s", res.Error)
	}
}
