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

// 回归锁定：function_call 的 arguments 必须始终作为 JSON 字符串发到客户端，
// 绝不能透传成裸对象。背景：上游 Postman(gpt-5.6-sol) 会把同一个调用的 arguments
// 先以空字符串、再以对象 {} 的形式分片下发。若直接透传对象，产生
// `"delta":{}` / `"arguments":{}`，违反 OpenAI Responses 规范(arguments 必须是字符串)，
// 导致 Codex 侧解析工具参数失败。normalizeArguments 负责把对象规整成字符串。
func TestResponsesToolArgsAlwaysString(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UpsertAccount("test@example.com", "", `{"access_token":"token","user_id":"user","workspace_id":"workspace"}`, "manual"); err != nil {
		t.Fatal(err)
	}
	rt := router.New(db)
	// 上游把 arguments 先发空串、再发对象 {}(与真实 gpt-5.6-sol agent-mode 抓包一致)。
	rt.Provider.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"function":{"arguments":"","name":"listCollections"},"id":"call_X","toolCallGroupId":null}],"metadata":{"model":"gpt-5.6-sol"}}}` + "\n" +
			`data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"function":{"arguments":{},"name":"listCollections"},"id":"call_X","toolCallGroupId":null}],"metadata":{"model":"gpt-5.6-sol"}}}` + "\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	server := &Server{Store: db, Router: rt}
	mux := http.NewServeMux()
	server.Register(mux)

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"input":"hi"}`))
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()

	// 反例：绝不能出现裸对象形式。
	if strings.Contains(body, `"delta":{}`) || strings.Contains(body, `"arguments":{}`) {
		t.Fatalf("arguments 被透传成裸对象，违反 Responses 规范:\n%s", body)
	}
	// 正例：arguments 增量与收尾都应是字符串 "{}"。
	if !strings.Contains(body, `"delta":"{}"`) {
		t.Fatalf("缺少字符串形式的 arguments 增量:\n%s", body)
	}
	if !strings.Contains(body, `"arguments":"{}"`) {
		t.Fatalf("缺少字符串形式的 arguments 收尾:\n%s", body)
	}
}
