package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ps2api/internal/router"
	"ps2api/internal/store"
)

// 回归锁定：codex 在 additional_tools 里声明的 custom(自由文本)工具，模型调用时必须以
// custom_tool_call 渲染回客户端，且事件序列完整(added → input.delta → input.done → item.done)。
// 上游回吐的名字若被 thirdParty 机制加了 namespace 前缀(functions.exec)，须剥回裸名——
// 客户端只认自己声明的名字，带前缀会被判 "unsupported call" 死循环。
func TestResponsesStreamCustomToolCall(t *testing.T) {
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
		// 上游回吐带前缀的 functions.exec(历史 trace 的真实形态)，arguments 是对象分片。
		body := `data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"function":{"arguments":"","name":"functions.exec"},"id":"call_X","toolCallGroupId":"g1"}],"metadata":{"model":"gpt-5.6-sol"}}}` + "\n" +
			`data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"function":{"arguments":{"input":"await tools.exec_command({\"cmd\":\"ls\"})"},"name":"functions.exec"},"id":"call_X","toolCallGroupId":"g1"}],"metadata":{"model":"gpt-5.6-sol"}}}` + "\n" +
			"data: [DONE]\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	server := &Server{Store: db, Router: rt}
	mux := http.NewServeMux()
	server.Register(mux)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol","stream":true,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"additional_tools","role":"developer","tools":[
				{"name":"functions","tools":[{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark"}}]}
			]}
		]
	}`))
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()

	// 完整事件序列：input.delta + input.done + 带 input 的 completed item。
	if !strings.Contains(body, "response.custom_tool_call_input.delta") {
		t.Fatalf("missing custom_tool_call_input.delta:\n%s", body)
	}
	if !strings.Contains(body, "response.custom_tool_call_input.done") {
		t.Fatalf("missing custom_tool_call_input.done:\n%s", body)
	}
	if !strings.Contains(body, `"type":"custom_tool_call"`) {
		t.Fatalf("missing custom_tool_call item:\n%s", body)
	}
	// 前缀必须被剥掉：渲染名是裸 exec，而非 functions.exec。
	if strings.Contains(body, `"name":"functions.exec"`) || strings.Contains(body, `"delta":"functions.exec"`) {
		t.Fatalf("upstream tool name prefix must be stripped to bare exec:\n%s", body)
	}
	if !strings.Contains(body, `"name":"exec"`) {
		t.Fatalf("custom_tool_call must render under bare name exec:\n%s", body)
	}
	// input 必须是 unwrap 后的自由文本，而非 {"input":"..."} 包装(SSE 里带 JSON 转义)。
	if !strings.Contains(body, `await tools.exec_command({\"cmd\":\"ls\"})`) {
		t.Fatalf("custom input must be unwrapped from arguments:\n%s", body)
	}
	// custom 工具不得走 function_call 的 arguments 增量事件。
	if strings.Contains(body, "response.function_call_arguments.delta") {
		t.Fatalf("custom tool must not emit function_call_arguments events:\n%s", body)
	}
}
