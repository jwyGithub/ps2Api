package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ps2api/internal/router"
	"ps2api/internal/store"
)

// 这些测试锁定「上游失败时，错误必须按各自协议规范表达」这条约束。
// 背景：此前三个对话端点统一返回 503 + {"error":{...}}，且流式失败后仍发送正常终止
// 标记，导致 agent 客户端把失败当成「本轮成功但没内容」，不停止、继续下一轮。

// failingUpstream 返回一个所有上游请求都失败的 Server，用于驱动错误分支。
// 账号存在(否则 router 走的是「无可用账号」而非「上游失败」路径)，但传输层必定报错。
func failingUpstream(t *testing.T) *http.ServeMux {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.UpsertAccount("test@example.com", "", `{"access_token":"token","user_id":"user","workspace_id":"workspace"}`, "manual"); err != nil {
		t.Fatal(err)
	}
	// retry_count=1：避免默认的 3 次重试各自退避，让测试快速跑完。
	if err := db.SetSetting("retry_count", "1"); err != nil {
		t.Fatal(err)
	}
	rt := router.New(db)
	rt.Provider.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	mux := http.NewServeMux()
	(&Server{Store: db, Router: rt}).Register(mux)
	return mux
}

func post(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return response
}

// Anthropic 错误体必须带顶层 type:"error"——缺了它客户端 SDK 认不出这是错误对象。
// 状态码用 529 overloaded_error 表达上游过载：503 不在 Anthropic 的状态码枚举里。
func TestAnthropicErrorShapeAndStatus(t *testing.T) {
	response := post(failingUpstream(t), "/v1/messages",
		`{"model":"claude-sonnet-4-6","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)

	if response.Code != 529 {
		t.Fatalf("status = %d, want 529 (503 不在 Anthropic 枚举内): %s", response.Code, response.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" {
		t.Fatalf("顶层 type = %q, want \"error\": %s", body.Type, response.Body.String())
	}
	if body.Error.Type != "overloaded_error" {
		t.Fatalf("error.type = %q, want overloaded_error", body.Error.Type)
	}
	if body.Error.Message == "" {
		t.Fatal("error.message 为空")
	}
}

// 400 类错误同样要带顶层 type:"error"，且 error.type 取自 Anthropic 枚举。
func TestAnthropicBadRequestShape(t *testing.T) {
	response := post(failingUpstream(t), "/v1/messages", `{"model":"","messages":[]}`)

	if response.Code != 400 {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["type"] != "error" {
		t.Fatalf("顶层 type = %v, want \"error\": %s", body["type"], response.Body.String())
	}
	errObj, _ := body["error"].(map[string]interface{})
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type = %v, want invalid_request_error", errObj["type"])
	}
}

// 流式失败时的终止序列：必须有 error 事件 + message_stop，且不得出现
// stop_reason:"error"——那不是 Anthropic 的合法枚举值，类型化 SDK 会解析失败。
func TestAnthropicStreamErrorTermination(t *testing.T) {
	response := post(failingUpstream(t), "/v1/messages",
		`{"model":"claude-sonnet-4-6","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := response.Body.String()

	if !strings.Contains(body, "event: error") {
		t.Fatalf("缺少 error 事件: %s", body)
	}
	if !strings.Contains(body, `"type":"message_stop"`) {
		t.Fatalf("缺少 message_stop，客户端会永久挂起: %s", body)
	}
	if strings.Contains(body, `"stop_reason":"error"`) {
		t.Fatalf("stop_reason:\"error\" 不是合法枚举值: %s", body)
	}
	// 失败流不能谎报成功完成。
	if strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("失败流不应带 stop_reason:end_turn: %s", body)
	}
}

// OpenAI 流式失败后**不能**发 data: [DONE]——它的语义是「流正常完成」，
// SDK 见到它会忽略前面的 error 帧，认定本轮成功但内容为空，于是 agent 不停止。
func TestOpenAIStreamErrorHasNoDone(t *testing.T) {
	response := post(failingUpstream(t), "/v1/chat/completions",
		`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := response.Body.String()

	// 首个增量前就失败：此时应回退为干净的 HTTP 503 JSON，而不是半截流。
	if response.Code == 503 {
		if strings.Contains(body, "[DONE]") {
			t.Fatalf("503 回退路径不应含 [DONE]: %s", body)
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("503 body 不是合法 JSON: %s", body)
		}
		errObj, _ := parsed["error"].(map[string]interface{})
		if errObj["type"] != "service_unavailable" {
			t.Fatalf("error.type = %v, want service_unavailable", errObj["type"])
		}
		return
	}
	// 已开流后失败：error 帧之后必须直接断流。
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("流式失败后不得发 [DONE]，否则 SDK 认定本轮成功: %s", body)
	}
}

// 非流式 OpenAI 错误体带 param/code 两个键（OpenAI SDK 期望的形状）。
func TestOpenAIErrorShape(t *testing.T) {
	response := post(failingUpstream(t), "/v1/chat/completions",
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	if response.Code != 503 {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// 不应带 Anthropic 的顶层 type。
	if _, ok := body["type"]; ok {
		t.Fatalf("OpenAI 错误体不应有顶层 type: %s", response.Body.String())
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("缺少 error 对象: %s", response.Body.String())
	}
	if errObj["type"] != "service_unavailable" {
		t.Fatalf("error.type = %v, want service_unavailable", errObj["type"])
	}
	for _, key := range []string{"param", "code"} {
		if _, ok := errObj[key]; !ok {
			t.Fatalf("OpenAI 错误体缺少 %q 键: %s", key, response.Body.String())
		}
	}
}

// 鉴权失败要按调用方协议分流：同一个 401，两种协议的错误体结构不同。
func TestAuthErrorFollowsCallerProtocol(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSetting("api_key", "secret"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Server{Store: db, Router: router.New(db)}).Register(mux)

	anthropic := post(mux, "/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if anthropic.Code != 401 {
		t.Fatalf("anthropic status = %d, want 401", anthropic.Code)
	}
	var aBody map[string]interface{}
	if err := json.Unmarshal(anthropic.Body.Bytes(), &aBody); err != nil {
		t.Fatal(err)
	}
	if aBody["type"] != "error" {
		t.Fatalf("anthropic 401 缺少顶层 type:error: %s", anthropic.Body.String())
	}
	if errObj, _ := aBody["error"].(map[string]interface{}); errObj["type"] != "authentication_error" {
		t.Fatalf("anthropic error.type = %v, want authentication_error", errObj["type"])
	}

	openai := post(mux, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if openai.Code != 401 {
		t.Fatalf("openai status = %d, want 401", openai.Code)
	}
	var oBody map[string]interface{}
	if err := json.Unmarshal(openai.Body.Bytes(), &oBody); err != nil {
		t.Fatal(err)
	}
	if _, ok := oBody["type"]; ok {
		t.Fatalf("openai 401 不应有顶层 type: %s", openai.Body.String())
	}
	errObj, _ := oBody["error"].(map[string]interface{})
	if errObj["type"] != "invalid_request_error" || errObj["code"] != "invalid_api_key" {
		t.Fatalf("openai error = %#v", errObj)
	}
}
