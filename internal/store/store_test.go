package store

import (
	"strings"
	"testing"
	"time"
)

// newTestStore 开一个临时库供统计查询测试使用。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/store_test.db")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestWafSignatureLikeSQL 钉住 SQL 片段生成：每个签名一条 LIKE，含 `<` 的签名
// 自动补转义形变体（json.Marshal 把 < 存成 <，SQL 子串匹配用 u003c）。
func TestWafSignatureLikeSQL(t *testing.T) {
	sql := WafSignatureLikeSQL([]string{"<script", "bin/cat"})
	want := `(upstream_body LIKE '%<script%' OR upstream_body LIKE '%u003cscript%'
		OR upstream_body LIKE '%bin/cat%')`
	if sql != want {
		t.Fatalf("WafSignatureLikeSQL =\n%s\nwant\n%s", sql, want)
	}
	// 空列表：恒 false 片段，保证拼进 SQL 不炸
	if got := WafSignatureLikeSQL(nil); got != "(0)" {
		t.Fatalf("nil probes should give '(0)', got %s", got)
	}
}

// TestCloudflare403SignatureSummaryInjected 统计口径沿用注入的签名表：
// 403 且命中签名的行计入 blockedHit，成功行计入 okHit。
func TestCloudflare403SignatureSummaryInjected(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	s.LogRequest(&RequestLog{Status: "error", ErrorMessage: "Postman gateway rejected request (403, Cloudflare)", UpstreamBody: `{"a":"./bin/cat x"}`, RequestBytes: 100})
	s.LogRequest(&RequestLog{Status: "success", UpstreamBody: `{"a":"clean"}`, RequestBytes: 100})
	out, err := s.Cloudflare403SignatureSummary(time.Hour, []string{"bin/cat"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1/1") || !strings.Contains(out, "0/1") {
		t.Fatalf("summary should report 1/1 blocked hit and 0/1 ok hit, got: %s", out)
	}
}

// TestWafAnalysisQueries 覆盖 403 列表分页、baseline 三级回退、候选与分桶。
func TestWafAnalysisQueries(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	now := time.Now()
	mk := func(status, errmsg, conv string, accID int64, bytes int, ageSec int) *RequestLog {
		return &RequestLog{Status: status, ErrorMessage: errmsg, ConversationID: conv,
			AccountID: &accID, RequestBytes: bytes, UpstreamBody: `{"a":1}`,
			CreatedAt: now.Add(-time.Duration(ageSec) * time.Second)}
	}
	var acc int64 = 7
	// 1: 成功(同会话) 2: 403 3: 成功(同账号无会话) 4: 403 5: 成功(他会话)
	s.LogRequest(mk("success", "", "convA", acc, 40000, 60))               // id 1
	s.LogRequest(mk("error", "(403, Cloudflare)", "convA", acc, 41000, 50)) // id 2
	s.LogRequest(mk("success", "", "", acc, 42000, 40))                     // id 3（无会话键 → 独立组）
	s.LogRequest(mk("error", "(403, Cloudflare)", "convB", acc, 43000, 30)) // id 4
	s.LogRequest(mk("success", "", "convC", acc+1, 44000, 20))              // id 5

	n, err := s.CountCloudflare403Logs()
	if err != nil || n != 2 {
		t.Fatalf("CountCloudflare403Logs = %d, %v; want 2", n, err)
	}
	logs, err := s.PageCloudflare403Logs(0, 10)
	if err != nil || len(logs) != 2 || logs[0].ID < logs[1].ID {
		t.Fatalf("PageCloudflare403Logs should be id DESC, got %v (%v)", logs, err)
	}

	// 单条查询
	l2, err := s.GetRequestLog(2)
	if err != nil || l2 == nil || l2.ConversationID != "convA" {
		t.Fatalf("GetRequestLog(2) = %+v, %v", l2, err)
	}
	if miss, _ := s.GetRequestLog(999); miss != nil {
		t.Fatal("GetRequestLog(999) should be nil, nil")
	}

	// baseline 三级回退：同会话成功(id1) → tier=conversation
	b, tier, err := s.FindWafBaseline(l2)
	if err != nil || b == nil || b.ID != 1 || tier != "conversation" {
		t.Fatalf("FindWafBaseline(convA) = id %d tier %q (%v); want id 1 conversation", bID(b), tier, err)
	}
	// convB 无同会话成功 → 同账号最近成功(id3) → tier=account
	l4, _ := s.GetRequestLog(4)
	b, tier, _ = s.FindWafBaseline(l4)
	if b == nil || b.ID != 3 || tier != "account" {
		t.Fatalf("FindWafBaseline(convB) = id %d tier %q; want id 3 account", bID(b), tier)
	}
	// 候选下拉：同会话/同账号/全局混合
	cands, err := s.WafBaselineCandidates(l4, 9)
	if err != nil || len(cands) == 0 {
		t.Fatalf("WafBaselineCandidates = %v (%v)", cands, err)
	}
	// 分桶：40-50K 桶 ok=3 fail=2
	buckets, err := s.WafSizeBuckets(now.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, bk := range buckets {
		if bk["bucket"] == "40K" {
			found = true
			if bk["ok"].(int64) != 3 || bk["fail"].(int64) != 2 {
				t.Fatalf("bucket 40K = %v; want ok=3 fail=2", bk)
			}
		}
	}
	if !found {
		t.Fatalf("bucket 40K missing in %v", buckets)
	}
}

func bID(l *RequestLog) int64 {
	if l == nil {
		return 0
	}
	return l.ID
}
