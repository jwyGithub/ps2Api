// quota_cycle_backfill_test.go —— 周期字段补采回归（2026-10-15）：
// billing 直查成功但 quotaCycleStart/End 为 NULL 时，补一次最小烧 token 探测把
// usageCycle 落库（RefreshDueQuotas 对 NULL 周期账号永久跳过的盲区修复）。
// 周期已有值时不补采（每次对话都会覆盖更新，上游权威值）。
package router

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// usageCycle SSE usage 事件（探测流里唯一关心的东西）——Postman SSE 是单行 data JSON，
// eventType 在 JSON 内（sseEvent 结构），另有 [DONE] 收尾。
const probeUsageSSE = "data: {\"eventType\":\"usage\",\"data\":{\"userType\":\"SOLO_USER\",\"usageState\":\"AVAILABLE\",\"usageCycle\":{\"start\":\"2026-10-10T00:00:00Z\",\"end\":\"2026-10-17T00:00:00Z\"}}}\n\n" +
	"data: [DONE]\n\n"

// stubCycleTransport：ws/proxy 走 billing 直查（operations），/chat 探测返回带 usageCycle 的 SSE 流。
func stubCycleTransport(t *testing.T, chatProbes *atomic.Int64) *Router {
	t.Helper()
	r := newTestRouter(t)
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Host, "gateway.postman.com") && strings.HasSuffix(req.URL.Path, "/chat") {
			chatProbes.Add(1)
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(probeUsageSSE)),
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Request:    req,
			}, nil
		}
		// ws/proxy（bifrost）：billing operations 直查
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		resp := "not found"
		if strings.Contains(body.Path, "/operations") {
			resp = billingOpsJSON(100)
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(resp)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    req,
		}, nil
	})}
	return r
}

func TestBackfillCycleWhenNull(t *testing.T) {
	probes := &atomic.Int64{}
	r := stubCycleTransport(t, probes)
	acc, _ := r.Store.GetAccount(1)
	if acc.QuotaCycleStart != nil || acc.QuotaCycleEnd != nil {
		t.Fatal("precondition: fresh account must have NULL cycle")
	}
	pr := r.probeAccountQuota(t.Context(), acc, false)
	if !pr.OK {
		t.Fatalf("probe must succeed via billing ops, got error: %s", pr.Error)
	}
	if probes.Load() != 1 {
		t.Fatalf("exactly one chat probe expected for cycle backfill, got %d", probes.Load())
	}
	fresh, _ := r.Store.GetAccount(acc.ID)
	if fresh.QuotaCycleStart == nil || fresh.QuotaCycleEnd == nil {
		t.Fatalf("cycle must be backfilled, got start=%v end=%v", fresh.QuotaCycleStart, fresh.QuotaCycleEnd)
	}
	if want := "2026-10-17T00:00:00Z"; fresh.QuotaCycleEnd.Format("2006-01-02T15:04:05Z") != want {
		t.Fatalf("cycle end = %v, want %s", fresh.QuotaCycleEnd, want)
	}
}

func TestNoBackfillWhenCyclePresent(t *testing.T) {
	probes := &atomic.Int64{}
	r := stubCycleTransport(t, probes)
	acc, _ := r.Store.GetAccount(1)
	// 模拟周期已有值（此前聊天已写入）
	start, end := *parseT(t, "2026-10-01T00:00:00Z"), *parseT(t, "2026-10-08T00:00:00Z")
	if err := r.Store.SetQuotaCycle(acc.ID, &start, &end); err != nil {
		t.Fatal(err)
	}
	r.probeAccountQuota(t.Context(), acc, false)
	if probes.Load() != 0 {
		t.Fatalf("cycle present: no chat probe expected, got %d", probes.Load())
	}
}

func TestBackfillSilentWhenProbeFails(t *testing.T) {
	r := newTestRouter(t)
	// ws/proxy 直查成功，但 /chat 探测 500：额度结果不受影响，周期留待下次。
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Host, "gateway.postman.com") && strings.HasSuffix(req.URL.Path, "/chat") {
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("boom")), Header: http.Header{}, Request: req}, nil
		}
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		resp := "not found"
		if strings.Contains(body.Path, "/operations") {
			resp = billingOpsJSON(100)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: http.Header{"Content-Type": []string{"application/json"}}, Request: req}, nil
	})}
	acc, _ := r.Store.GetAccount(1)
	pr := r.probeAccountQuota(t.Context(), acc, false)
	if !pr.OK {
		t.Fatalf("billing ops result must survive probe failure, got error: %s", pr.Error)
	}
	fresh, _ := r.Store.GetAccount(acc.ID)
	if fresh.QuotaCycleStart != nil {
		t.Fatalf("failed probe must not write cycle, got %v", fresh.QuotaCycleStart)
	}
}

func parseT(t *testing.T, s string) *time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return &tt
}
