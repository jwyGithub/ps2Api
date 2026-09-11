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
