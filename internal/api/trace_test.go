package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceChatConnectsRequestAndResponse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GATEWAY_TRACE_LOG", "1")
	t.Setenv("GATEWAY_TRACE_DIR", dir)
	handler := traceChat(func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(w, 200, map[string]bool{"ok": true})
	})
	request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"test"}`))
	response := httptest.NewRecorder()
	handler(response, request)
	traceID := response.Header().Get("X-Postman2API-Trace-ID")
	if response.Code != 200 || traceID == "" {
		t.Fatalf("response status=%d trace_id=%q", response.Code, traceID)
	}
	files, err := filepath.Glob(filepath.Join(dir, "anthropic", "*", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("trace files=%v err=%v", files, err)
	}
	if filepath.Base(files[0]) != traceID+".jsonl" {
		t.Fatalf("trace file does not match response trace id: %s", files[0])
	}
	content, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, traceID) || !strings.Contains(text, `"event":"client.request"`) || !strings.Contains(text, `"event":"client.response.body"`) {
		t.Fatalf("incomplete trace: %s", text)
	}
}
