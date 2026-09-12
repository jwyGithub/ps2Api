package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// TestLeafDiff 钉住逐叶 diff 语义：只报差异叶子；字符串/标量/数组/删除/新增全覆盖；
// JSON 解析失败退化为整串单叶子。
func TestLeafDiff(t *testing.T) {
	base := `{"input":{"query":"hello","max":1},"arr":["a","b"],"keep":"same"}`
	// changed + added + removed 三类
	target := `{"input":{"query":"hello world","max":1},"arr":["a","c"],"keep":"same","new":"x"}`
	diffs := leafDiff(base, target)
	byPath := map[string]LeafDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	if len(diffs) != 3 {
		t.Fatalf("want 3 diffs, got %d: %+v", len(diffs), diffs)
	}
	q := byPath["input.query"]
	if q.Kind != "changed" || q.BaselineLen != 5 || q.TargetLen != 11 {
		t.Fatalf("input.query diff wrong: %+v", q)
	}
	if q.TargetPreview != "hello world" {
		t.Fatalf("preview = %q", q.TargetPreview)
	}
	if byPath["new"].Kind != "added" {
		t.Fatalf("new leaf should be added: %+v", byPath["new"])
	}
	if byPath["arr[1]"].Kind != "changed" {
		t.Fatalf("arr[1] should be changed: %+v", byPath["arr[1]"])
	}
	// removed：target 删掉 keep
	diffs = leafDiff(base, `{"input":{"query":"hello","max":1},"arr":["a","b"]}`)
	if len(diffs) != 1 || diffs[0].Path != "keep" || diffs[0].Kind != "removed" {
		t.Fatalf("removed leaf wrong: %+v", diffs)
	}
	// 相同 body 零差异
	if got := leafDiff(base, base); len(got) != 0 {
		t.Fatalf("identical bodies should give no diffs, got %+v", got)
	}
	// 非法 JSON：退化整串
	diffs = leafDiff("not-json{", "not-json{2")
	if len(diffs) != 1 || diffs[0].Path != "(body)" || diffs[0].Kind != "changed" {
		t.Fatalf("malformed bodies should degrade to single leaf: %+v", diffs)
	}
	// 长文本截断到 500
	long := strings.Repeat("x", 800)
	diffs = leafDiff(`{"a":"`+long+`"}`, `{"a":"`+long+`y"}`)
	if len(diffs[0].TargetPreview) != 500 {
		t.Fatalf("preview should cap at 500, got %d", len(diffs[0].TargetPreview))
	}
}

// TestWafAnalyzeEndpoint 端到端：403 行 + 同会话成功行 → 签名计数、diff、
// baseline tier、分桶齐返回；无 baseline_id 时自动挑选；显式 baseline_id 覆盖自动挑选。
func TestWafAnalyzeEndpoint(t *testing.T) {
	st, _, mux := newWafTestServer(t)
	var acc int64 = 7
	st.LogRequest(&store.RequestLog{Status: "success", AccountID: &acc, RequestBytes: 100,
		UpstreamBody: `{"input":{"query":"./bin/cat ok"}}`, ConversationID: "c1", CreatedAt: time.Now()})
	st.LogRequest(&store.RequestLog{Status: "error", ErrorMessage: "(403, Cloudflare)", AccountID: &acc,
		RequestBytes: 120, UpstreamBody: `{"input":{"query":"./bin/catpaw2api -config x"}}`,
		ConversationID: "c1", CreatedAt: time.Now()})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/analyze?log_id=2", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Log             map[string]interface{}   `json:"log"`
		SignatureCounts map[string]int           `json:"signatureCounts"`
		Baseline        map[string]interface{}   `json:"baseline"`
		Diff            []LeafDiff               `json:"diff"`
		SizeBuckets     []map[string]interface{} `json:"sizeBuckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SignatureCounts["bin/cat"] != 1 {
		t.Fatalf("signatureCounts = %v", got.SignatureCounts)
	}
	if got.Baseline["id"].(float64) != 1 || got.Baseline["tier"] != "conversation" {
		t.Fatalf("baseline = %v", got.Baseline)
	}
	if len(got.Diff) != 1 || got.Diff[0].Path != "input.query" || got.Diff[0].Kind != "changed" {
		t.Fatalf("diff = %+v", got.Diff)
	}
	if len(got.SizeBuckets) == 0 {
		t.Fatal("sizeBuckets should not be empty")
	}
	// 显式 baseline_id：无对照可选时 404
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/analyze?log_id=2&baseline_id=999", nil))
	if rec.Code != 404 {
		t.Fatalf("missing baseline should 404, got %d", rec.Code)
	}
	// log_id 非法 400
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/analyze?log_id=abc", nil))
	if rec.Code != 400 {
		t.Fatalf("bad log_id should 400, got %d", rec.Code)
	}
}

// newWafTestServer 构造带临时库的 Server + mux（Task 4 的测试可改用这个）。
func newWafTestServer(t *testing.T) (*store.Store, *Server, *http.ServeMux) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st)
	mux := http.NewServeMux()
	s.Register(mux)
	return st, s, mux
}

// TestParseLeafPathAndLeafValue 钉住路径解析与叶子全文提取：对象键/数组下标/
// 混合路径、路径不存在、畸形路径、非 JSON body。
func TestParseLeafPathAndLeafValue(t *testing.T) {
	body := `{"input":{"query":"./bin/catpaw2api -config x"},"arr":["a","b"],"nested":[{"msg":"hello"}]}`
	cases := []struct{ path, want string }{
		{"input.query", "./bin/catpaw2api -config x"},
		{"arr[1]", "b"},
		{"nested[0].msg", "hello"},
		{"input", `{"query":"./bin/catpaw2api -config x"}`}, // 中间节点：leafString 走 json.Marshal
	}
	for _, c := range cases {
		got, ok := leafValue(body, c.path)
		if !ok || got != c.want {
			t.Fatalf("leafValue(%q) = %q, %v; want %q", c.path, got, ok, c.want)
		}
	}
	for _, bad := range []string{"input.missing", "arr[5]", "nested[0].gone", "arr[x]", "input["} {
		if got, ok := leafValue(body, bad); ok {
			t.Fatalf("leafValue(%q) should miss, got %q", bad, got)
		}
	}
	if _, ok := leafValue("not-json{", "input.query"); ok {
		t.Fatal("malformed body should miss")
	}
}
