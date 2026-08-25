package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ps2api/internal/provider"
	"ps2api/internal/router"
	"ps2api/internal/store"
)

// 上游 Postman /chat 没有图片/附件通道（判定依据见 provider/imgprobe_test.go 的探针结论）。
// 带图请求此前被静默降级成 "[image attachment]" 占位符：模型看不到图却收到「这里有个附件」，
// 只能瞎猜，而调用方以为请求成功了。这些测试锁定「入站即明确 400」这条约束。

// mediaTestMux 返回一个不需要真实上游的 Server——媒体检查发生在触达 router 之前，
// 所以这些用例根本不会打到网络。
func mediaTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mux := http.NewServeMux()
	(&Server{Store: db, Router: router.New(db)}).Register(mux)
	return mux
}

// assertMediaRejected 断言响应是「因不支持媒体输入而 400」，并且文案可指导调用方改请求。
func assertMediaRejected(t *testing.T, response *httptest.ResponseRecorder, wantKind string) {
	t.Helper()
	if response.Code != 400 {
		t.Fatalf("status = %d, want 400（重试永远不会成功，必须是 4xx 让 SDK 立即停止）: %s",
			response.Code, response.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("缺少 error 对象: %s", response.Body.String())
	}
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type = %v, want invalid_request_error", errObj["type"])
	}
	message, _ := errObj["message"].(string)
	if !strings.Contains(message, wantKind) {
		t.Fatalf("消息未点明被拒的媒体类型 %q: %q", wantKind, message)
	}
	if !strings.Contains(message, "text only") {
		t.Fatalf("消息未说明上游只接受文本，调用方无从判断该怎么改: %q", message)
	}
}

// Anthropic 协议：content blocks 里的 image 块。
func TestAnthropicImageInputRejected(t *testing.T) {
	response := post(mediaTestMux(t), "/v1/messages", `{
		"model":"claude-sonnet-4-6","max_tokens":128,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}
		]}]}`)

	assertMediaRejected(t, response, "image")
	// Anthropic 协议的错误体还必须带顶层 type:error。
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["type"] != "error" {
		t.Fatalf("Anthropic 错误体缺少顶层 type:error: %s", response.Body.String())
	}
}

// Anthropic 协议：document(PDF) 块走同一条路——上游同样没有附件通道。
func TestAnthropicDocumentInputRejected(t *testing.T) {
	response := post(mediaTestMux(t), "/v1/messages", `{
		"model":"claude-sonnet-4-6","max_tokens":128,
		"messages":[{"role":"user","content":[
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}
		]}]}`)

	assertMediaRejected(t, response, "document")
}

// OpenAI 协议：image_url 块。
func TestOpenAIImageInputRejected(t *testing.T) {
	response := post(mediaTestMux(t), "/v1/chat/completions", `{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is this"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
		]}]}`)

	assertMediaRejected(t, response, "image")
	// OpenAI 错误体不应带 Anthropic 的顶层 type。
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["type"]; ok {
		t.Fatalf("OpenAI 错误体不应有顶层 type: %s", response.Body.String())
	}
}

// Responses 协议：input_image。这条最容易漏——responsesToOpenAI 会把 content 抽成纯文本，
// 图片块在转换那一步就丢了，所以检查必须发生在转换之前、扫原始 input。
func TestResponsesImageInputRejected(t *testing.T) {
	response := post(mediaTestMux(t), "/v1/responses", `{
		"model":"claude-sonnet-4-6",
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"what is this"},
			{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}
		]}]}`)

	assertMediaRejected(t, response, "image")
}

// 图片可能藏在 tool_result 的 content 里（客户端把截图作为工具结果回传）。
// 检测必须递归下探，否则这条路径会绕过拦截、继续静默降级。
func TestNestedToolResultImageRejected(t *testing.T) {
	response := post(mediaTestMux(t), "/v1/messages", `{
		"model":"claude-sonnet-4-6","max_tokens":128,
		"messages":[
			{"role":"user","content":"take a screenshot"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"screenshot","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}
			]}]}
		]}`)

	assertMediaRejected(t, response, "image")
}

// 不误伤：纯文本请求必须照常走到上游（这里没有可用账号，因此预期是上游侧错误而非 400）。
func TestPlainTextNotRejectedAsMedia(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/messages", `{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/messages", `{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`},
		{"/v1/chat/completions", `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/responses", `{"model":"claude-sonnet-4-6","input":"hello"}`},
	} {
		response := post(mediaTestMux(t), tc.path, tc.body)
		if response.Code == 400 {
			t.Errorf("%s 纯文本被误判为媒体输入: %s", tc.path, response.Body.String())
		}
	}
}

// text 块字符串值里出现 "type":"image" 这样的字面量不应触发拦截——JSON 里它是字符串，
// 不会被解析成对象。这条守住递归检测不产生误报。
func TestMediaLookalikeInTextNotRejected(t *testing.T) {
	body, err := json.Marshal(map[string]interface{}{
		"model": "claude-sonnet-4-6", "max_tokens": 128,
		"messages": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{map[string]interface{}{
				"type": "text",
				"text": `how do I send {"type":"image","source":{"type":"base64"}} to the API?`,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := post(mediaTestMux(t), "/v1/messages", string(body))
	if response.Code == 400 {
		t.Fatalf("提到 image 的纯文本被误拦: %s", response.Body.String())
	}
}

// 直接测 provider 层的判定，覆盖各协议的类型名映射。
func TestUnsupportedMediaKindMapping(t *testing.T) {
	cases := []struct {
		block string
		want  string
	}{
		{`{"type":"image","source":{}}`, "image"},
		{`{"type":"image_url","image_url":{}}`, "image"},
		{`{"type":"input_image","image_url":"x"}`, "image"},
		{`{"type":"document","source":{}}`, "document"},
		{`{"type":"file","file":{}}`, "document"},
		{`{"type":"input_file","file_id":"x"}`, "document"},
	}
	for _, tc := range cases {
		content := json.RawMessage(`[` + tc.block + `]`)
		kind, ok := provider.UnsupportedMediaContent([]provider.ChatMessage{{Role: "user", Content: content}})
		if !ok || kind != tc.want {
			t.Errorf("%s → (%q, %v), want (%q, true)", tc.block, kind, ok, tc.want)
		}
	}
	// 文本与工具块不该被判为媒体。
	for _, block := range []string{
		`{"type":"text","text":"hi"}`,
		`{"type":"tool_use","id":"t","name":"n","input":{}}`,
		`{"type":"tool_result","tool_use_id":"t","content":"ok"}`,
	} {
		content := json.RawMessage(`[` + block + `]`)
		if kind, ok := provider.UnsupportedMediaContent([]provider.ChatMessage{{Role: "user", Content: content}}); ok {
			t.Errorf("%s 被误判为媒体 %q", block, kind)
		}
	}
}
