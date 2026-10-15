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

// —— namespace/web_search 拍平 + input item 毒字段（id/passthrough）回归 ——
// 2026-10-15 Codex CLI 真实请求体触发："input" / "tools[7]" does not match any of the
// allowed types。实测矩阵：namespace、web_search 工具类型上游不认（拍平为 <ns>__<tool>
// function 后 200）；message item 的 id 与 internal_chat_message_metadata_passthrough
// 各自单独都能炸掉 input（剥掉后 200，developer role 与 id 之外的字段无碍）。

func TestResponsesFlattensNamespaceTools(t *testing.T) {
	var gotBody string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Write([]byte(`{"id":"resp_3","output":[],"usage":{"output_tokens":1}}`))
	})
	raw := `{"input":[{"content":[{"text":"hi","type":"input_text"}],"role":"user"}],"model":"gpt-6.1-sol",
		"tools":[
			{"type":"function","name":"exec_command","description":"run","strict":false,"parameters":{"type":"object"}},
			{"type":"namespace","name":"multi_agent_v1","description":"sub agents","tools":[
				{"type":"function","name":"spawn_agent","description":"spawn","strict":false,"parameters":{"type":"object"}}
			]},
			{"type":"web_search","external_web_access":false}
		]}`
	acc := toolsetsTestAccount(t)
	res, _ := tp.ChatResponses(t.Context(), acc, []byte(raw), "gpt-6.1-sol", 0)
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	var m map[string]json.RawMessage
	json.Unmarshal([]byte(gotBody), &m)
	var tools []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	json.Unmarshal(m["tools"], &tools)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (function + flattened namespace), got %d: %s", len(tools), gotBody)
	}
	for _, tl := range tools {
		if tl.Type != "function" {
			t.Fatalf("namespace/web_search must be flattened away, got type %s", tl.Type)
		}
	}
	if tools[1].Name != "multi_agent_v1__spawn_agent" {
		t.Fatalf("flattened name = %s, want multi_agent_v1__spawn_agent", tools[1].Name)
	}
}

func TestResponsesStripsMessageItemPoisonFields(t *testing.T) {
	var gotBody string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Write([]byte(`{"id":"resp_4","output":[],"usage":{"output_tokens":1}}`))
	})
	raw := `{"input":[
		{"type":"message","id":"msg_01a13e55","role":"developer","content":[{"type":"input_text","text":"sys"}],"internal_chat_message_metadata_passthrough":{"turn_id":"t1"}},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	],"model":"gpt-6.1-sol","stream":true}`
	acc := toolsetsTestAccount(t)
	res, _ := tp.ChatResponses(t.Context(), acc, []byte(raw), "gpt-6.1-sol", 0)
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	var m map[string]json.RawMessage
	json.Unmarshal([]byte(gotBody), &m)
	var items []map[string]json.RawMessage
	json.Unmarshal(m["input"], &items)
	for i, it := range items {
		for _, poison := range []string{"id", "internal_chat_message_metadata_passthrough", "status", "phase"} {
			if _, ok := it[poison]; ok {
				t.Fatalf("input[%d] still carries %s", i, poison)
			}
		}
	}
	// developer role 保留（实测可过，上游认）
	var roles []string
	for _, it := range items {
		var role string
		json.Unmarshal(it["role"], &role)
		roles = append(roles, role)
	}
	if roles[0] != "developer" {
		t.Fatalf("developer role must be preserved, got %v", roles)
	}
}

func TestResponsesStripsPhaseField(t *testing.T) {
	// phase="final_answer"（Codex 标记最终回答）是第四个 message item 毒字段
	// （2026-10-15 15:29 线上 8309-8311："input[7]" does not match any of the allowed types）。
	var gotBody string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		w.Write([]byte(`{"id":"resp_5","output":[],"usage":{"output_tokens":1}}`))
	})
	raw := `{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"你好"}],"phase":"final_answer"}
	],"model":"gpt-6.1-sol","stream":true}`
	acc := toolsetsTestAccount(t)
	res, _ := tp.ChatResponses(t.Context(), acc, []byte(raw), "gpt-6.1-sol", 0)
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	var m map[string]json.RawMessage
	json.Unmarshal([]byte(gotBody), &m)
	var items []map[string]json.RawMessage
	json.Unmarshal(m["input"], &items)
	if _, ok := items[1]["phase"]; ok {
		t.Fatalf("phase must be stripped, got %s", items[1])
	}
	if _, ok := items[1]["id"]; ok {
		t.Fatalf("id must be stripped, got %s", items[1])
	}
}
