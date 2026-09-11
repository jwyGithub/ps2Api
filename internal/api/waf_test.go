package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"ps2api/internal/store"
)

// TestWafSignaturesEndpoint 签名表经 API 暴露给前端（单一事实源的对外出口）。
func TestWafSignaturesEndpoint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(st)
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf-signatures", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Probes []string `json:"probes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Probes) == 0 {
		t.Fatal("probes should not be empty")
	}
}

// TestWafBaselinesEndpoint 候选对照列表：有成功记录时返回候选，无则空数组。
func TestWafBaselinesEndpoint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	var acc int64 = 7
	st.LogRequest(&store.RequestLog{Status: "success", AccountID: &acc, RequestBytes: 100, UpstreamBody: `{}`, ConversationID: "c1", CreatedAt: time.Now()})
	st.LogRequest(&store.RequestLog{Status: "error", ErrorMessage: "(403, Cloudflare)", AccountID: &acc, RequestBytes: 120, UpstreamBody: `{}`, ConversationID: "c1", CreatedAt: time.Now()})
	s := New(st)
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/baselines?log_id=2", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) == 0 {
		t.Fatal("expected baseline candidates")
	}
	if got.Data[0]["id"].(float64) != 1 {
		t.Fatalf("first candidate should be id 1, got %v", got.Data[0]["id"])
	}
}
