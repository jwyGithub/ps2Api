package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// TestProbePad 钉住等长 padding：不足补良性散文、已达标原样返回、绝不截断
// （截断会切掉待验证特征）。
func TestProbePad(t *testing.T) {
	short := probePad("abc", 200)
	if len(short) != 200 {
		t.Fatalf("probePad should pad to 200, got %d", len(short))
	}
	if !strings.HasPrefix(short, "abc") {
		t.Fatalf("padding must preserve original prefix, got %q", short[:10])
	}
	exact := probePad(strings.Repeat("x", 300), 200)
	if exact != strings.Repeat("x", 300) {
		t.Fatal("probePad must never truncate")
	}
}

// TestClassifyProbeOutcome 归类口径同 repro403 实验的 classify：成功=pass、
// 网关拦截=403、其余=other；Cf-Ray 从 RejectionDetail 行提取。
func TestClassifyProbeOutcome(t *testing.T) {
	if o := classifyProbeOutcome(&provider.Result{Success: true, RequestBytes: 42}); o.Label != "pass" || o.Bytes != 42 {
		t.Fatalf("success should be pass, got %+v", o)
	}
	o := classifyProbeOutcome(&provider.Result{GatewayBlocked: true, RequestBytes: 10,
		Error: "(403, Cloudflare)",
		RejectionDetail: "HTTP 状态: 403\nCf-Ray: 8b2c1d3e4f5a6b7c-SJC\n出站请求体: 10 字节"})
	if o.Label != "403" || o.Ray != "8b2c1d3e4f5a6b7c-SJC" {
		t.Fatalf("gateway blocked misclassified: %+v", o)
	}
	if o := classifyProbeOutcome(&provider.Result{Error: "boom"}); o.Label != "other" || o.Detail != "boom" {
		t.Fatalf("other misclassified: %+v", o)
	}
	if o := classifyProbeOutcome(nil); o.Label != "other" {
		t.Fatalf("nil should be other: %+v", o)
	}
}

// TestBisectDescend 行级二分收敛：左命中向左收、右命中向右收、两半都放行=歧义、
// 单行区间直接终止。
func TestBisectDescend(t *testing.T) {
	// 6 行，第 4 行（下标 3）触发：前几轮左半放行、右半命中后向左收
	lo, hi, amb, rounds := bisectDescend(6, func(a, b int) bool { return a <= 3 && 3 < b })
	if lo != 3 || hi != 4 || amb || rounds == 0 {
		t.Fatalf("want [3,4) not ambiguous, got [%d,%d) amb=%v rounds=%d", lo, hi, amb, rounds)
	}
	// 第 0 行触发：一直向左收
	lo, hi, amb, _ = bisectDescend(8, func(a, b int) bool { return a == 0 })
	if lo != 0 || hi != 1 || amb {
		t.Fatalf("want [0,1), got [%d,%d) amb=%v", lo, hi, amb)
	}
	// 全放行：歧义
	_, _, amb, _ = bisectDescend(4, func(a, b int) bool { return false })
	if !amb {
		t.Fatal("all-pass halves should be ambiguous")
	}
	// 单行：不测试直接返回
	lo, hi, amb, rounds = bisectDescend(1, func(a, b int) bool { t.Fatal("single line must not be tested"); return false })
	if lo != 0 || hi != 1 || amb || rounds != 0 {
		t.Fatalf("single line: got [%d,%d) amb=%v rounds=%d", lo, hi, amb, rounds)
	}
}

// newFakeProbeTestServer 构造带假发送器的测试服务：blocked(text) 决定一条探针文本
// 是否「触发 403」，其余一律 pass。零网络。
func newFakeProbeTestServer(t *testing.T, blocked func(text string) bool) (*http.ServeMux, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// 简报原版测试没插入账号，ActiveAccounts() 为空会让所有 POST 卡在
	// 「没有活跃账号」400；这里补一个活跃账号，POST 默认选中它。
	if _, err := st.ImportAccount("probe@test", "", "{}", "manual", true); err != nil {
		t.Fatal(err)
	}
	var acc int64 = 7
	st.LogRequest(&store.RequestLog{Status: "success", AccountID: &acc, RequestBytes: 10,
		UpstreamBody: `{"input":{"query":"clean"}}`, ConversationID: "c1", CreatedAt: time.Now()})
	st.LogRequest(&store.RequestLog{Status: "error", ErrorMessage: "(403, Cloudflare)", AccountID: &acc,
		RequestBytes: 20, UpstreamBody: `{"input":{"query":"padding one\n./bin/catpaw2api -config config.json\npadding two\npadding three\npadding four\npadding five\npadding six"}}`,
		ConversationID: "c1", CreatedAt: time.Now()})
	s := New(st)
	s.probe.newSender = func(acc *store.Account, model string) probeSender {
		return func(ctx context.Context, text string) probeOutcome {
			if blocked(text) {
				return probeOutcome{Label: "403", Ray: "fake-ray", Bytes: len(text)}
			}
			return probeOutcome{Label: "pass", Bytes: len(text)}
		}
	}
	mux := http.NewServeMux()
	s.Register(mux)
	return mux, s
}

// waitProbeDone 轮询 GET 直到 job 结束（假发送器零延迟，5s 上限足够）。
func waitProbeDone(t *testing.T, mux *http.ServeMux, jobID string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/probe/"+jobID, nil))
		if rec.Code != 200 {
			t.Fatalf("probe status GET = %d: %s", rec.Code, rec.Body.String())
		}
		var j map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		if j["status"] != "running" {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("probe job did not finish in 5s")
	return nil
}

func postProbe(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/waf/probe", strings.NewReader(body)))
	return rec
}

// TestWafProbeBisectEndToEnd 端到端（假发送器）：叶子轮命中 → 行级二分收敛到
// bin/cat 行；对照变体放行；结论与证据齐全。
func TestWafProbeBisectEndToEnd(t *testing.T) {
	probePause = 0 // 测试不发真实请求，退避归零
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, _ := newFakeProbeTestServer(t, func(text string) bool { return strings.Contains(text, "bin/cat") })

	rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	j := waitProbeDone(t, mux, started.JobID)
	if j["status"] != "done" {
		t.Fatalf("status = %v, summary = %v", j["status"], j["summary"])
	}
	// 命中叶子与收敛行
	hits := j["hits"].([]interface{})
	if len(hits) != 1 || hits[0] != "input.query" {
		t.Fatalf("hits = %v", hits)
	}
	cs := j["conclusions"].([]interface{})
	if len(cs) != 1 {
		t.Fatalf("conclusions = %v", cs)
	}
	c := cs[0].(map[string]interface{})
	lines := c["lines"].([]interface{})
	if len(lines) != 1 || lines[0] != "./bin/catpaw2api -config config.json" {
		t.Fatalf("converged lines = %v", lines)
	}
	if c["ambiguous"] != false {
		t.Fatalf("should not be ambiguous: %v", c)
	}
	// 变体序列：对照在前且 pass；叶子轮 403；二分轮 ≥1
	variants := j["variants"].([]interface{})
	if len(variants) < 3 {
		t.Fatalf("variants too few: %v", variants)
	}
	if variants[0].(map[string]interface{})["outcome"] != "pass" {
		t.Fatalf("control variant should pass: %v", variants[0])
	}
	saw403 := false
	for _, v := range variants[1:] {
		if v.(map[string]interface{})["outcome"] == "403" {
			saw403 = true
		}
	}
	if !saw403 {
		t.Fatalf("no 403 variant recorded: %v", variants)
	}
}

// TestWafProbeAllPass 全放行：结论提示组合特征/换对照（spec 原文案）。
func TestWafProbeAllPass(t *testing.T) {
	probePause = 0
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, _ := newFakeProbeTestServer(t, func(string) bool { return false })
	rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	j := waitProbeDone(t, mux, started.JobID)
	if j["status"] != "done" || !strings.Contains(fmt.Sprint(j["summary"]), "组合特征") {
		t.Fatalf("all-pass summary wrong: %v", j["summary"])
	}
}

// TestWafProbeControlBlocked 对照被拦：中止并提示风控窗口。
func TestWafProbeControlBlocked(t *testing.T) {
	probePause = 0
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, _ := newFakeProbeTestServer(t, func(string) bool { return true })
	rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`)
	var started struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	j := waitProbeDone(t, mux, started.JobID)
	if j["status"] != "aborted" || !strings.Contains(fmt.Sprint(j["summary"]), "风控窗口") {
		t.Fatalf("control-blocked should abort with hint: %v / %v", j["status"], j["summary"])
	}
}

// TestWafProbeAbortPropagation 中止传播：叶子轮中途 abort 后主流程必须停，
// 最终 status 是 aborted，不产出「全部叶子放行」假结论，conclusions 为空。
// 全放行假发送器：若中止不传播，job 会以 done + 假结论收场，此测试即失败。
func TestWafProbeAbortPropagation(t *testing.T) {
	probePause = 0
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, s := newFakeProbeTestServer(t, func(string) bool { return false })
	// 覆写发送器：第 3 次调用（对照 2 次后的叶子轮第 1 次）挂起，
	// 等测试走真实 abort 端点后再放行，保证取消发生在 job 运行中。
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	s.probe.newSender = func(*store.Account, string) probeSender {
		return func(ctx context.Context, text string) probeOutcome {
			calls++
			if calls == 3 {
				close(entered)
				<-release
			}
			return probeOutcome{Label: "pass", Bytes: len(text)}
		}
	}
	rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	go func() {
		<-entered
		// 走真实 DELETE abort 端点（内部做 isRunning 检查后调 cancel）
		abortRec := httptest.NewRecorder()
		mux.ServeHTTP(abortRec, httptest.NewRequest("DELETE", "/api/waf/probe/"+started.JobID, nil))
		close(release)
	}()
	j := waitProbeDone(t, mux, started.JobID)
	if j["status"] != "aborted" {
		t.Fatalf("status = %v, summary = %v", j["status"], j["summary"])
	}
	if summary := fmt.Sprint(j["summary"]); strings.Contains(summary, "全部叶子放行") {
		t.Fatalf("abort must not be overwritten by fake all-pass conclusion: %q", summary)
	}
	if cs, ok := j["conclusions"].([]interface{}); !ok || len(cs) != 0 {
		t.Fatalf("aborted job must have no conclusions: %v", j["conclusions"])
	}
}

// TestWafProbeValidation 校验分支：未知路径 400、单飞 409、缺参 400、日志不存在 404。
func TestWafProbeValidation(t *testing.T) {
	probePause = 0
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, s := newFakeProbeTestServer(t, func(string) bool { return false })

	if rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.absent"]}`); rec.Code != 400 {
		t.Fatalf("unknown path should 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postProbe(t, mux, `{"log_id":0,"baseline_id":1,"paths":["input.query"]}`); rec.Code != 400 {
		t.Fatalf("bad log_id should 400, got %d", rec.Code)
	}
	if rec := postProbe(t, mux, `{"log_id":999,"baseline_id":1,"paths":["input.query"]}`); rec.Code != 404 {
		t.Fatalf("missing log should 404, got %d", rec.Code)
	}
	// 单飞：手工塞一个 running job（无需真跑）
	s.probe.mu.Lock()
	s.probe.current = &probeJob{id: "probe-x", status: "running"}
	s.probe.mu.Unlock()
	if rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`); rec.Code != 409 {
		t.Fatalf("second job while running should 409, got %d: %s", rec.Code, rec.Body.String())
	}
	// 超长叶子：全文 + 前缀超出上游 10000 rune query 上限，必须 400 拒绝
	// （否则所有变体变 other_error，产出「全部叶子放行」的误导性假阴性）。
	var bigAcc int64 = 7
	if err := s.Store.LogRequest(&store.RequestLog{Status: "error", ErrorMessage: "(403, Cloudflare)",
		AccountID: &bigAcc, RequestBytes: 13000, UpstreamBody: `{"input":{"query":"` + strings.Repeat("a", 12000) + `"}}`,
		ConversationID: "c1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	big := postProbe(t, mux, `{"log_id":3,"baseline_id":1,"paths":["input.query"]}`)
	if big.Code != 400 || !strings.Contains(big.Body.String(), "超出上游") {
		t.Fatalf("oversized leaf should 400 with hint, got %d: %s", big.Code, big.Body.String())
	}
	// 未知 job id
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/probe/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown job should 404, got %d", rec.Code)
	}
}
