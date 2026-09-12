package api

import (
	"strings"
	"testing"

	"ps2api/internal/provider"
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
