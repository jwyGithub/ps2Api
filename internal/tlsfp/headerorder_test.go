package tlsfp

import (
	"net/http"
	"sort"
	"testing"
)

// TestOrderedHeaderNamesFollowsChromeOrder 锁死头顺序契约：orderedHeaderNames 必须按
// Chromium 线序输出，且明确【不是】字母序（字母序是任何真实浏览器都不会有的顺序，
// 会在 Cloudflare 的 JA4H 头顺序指纹上露馅）。
func TestOrderedHeaderNamesFollowsChromeOrder(t *testing.T) {
	// 用 provider.buildHeaders(web 分支) 实际会发的头集合构造。
	h := http.Header{}
	for _, name := range []string{
		"Accept", "Accept-Encoding", "Accept-Language", "Content-Type", "Cookie",
		"Origin", "Priority", "Referer", "Sec-Ch-Ua", "Sec-Ch-Ua-Mobile",
		"Sec-Ch-Ua-Platform", "Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site",
		"User-Agent", "X-Access-Token", "X-App-Version", "X-Pstmn-Req-Service",
	} {
		h.Set(name, "x")
	}

	got := orderedHeaderNames(h)

	// 期望顺序：chromeHeaderOrder 中出现的头按表内顺序排列（表内没出现在 h 里的跳过）。
	want := []string{}
	present := map[string]bool{}
	for name := range h {
		present[name] = true
	}
	byLower := map[string]string{}
	for name := range h {
		byLower[toLower(name)] = name
	}
	for _, lower := range chromeHeaderOrder {
		if key, ok := byLower[lower]; ok {
			want = append(want, key)
		}
	}

	if len(got) != len(want) {
		t.Fatalf("头数量不一致：got %d want %d\ngot=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("头顺序不符 Chromium 线序：\ngot =%v\nwant=%v", got, want)
		}
	}

	// 反向断言：绝不能等于字母序。
	alpha := make([]string, len(got))
	copy(alpha, got)
	sort.Strings(alpha)
	same := true
	for i := range alpha {
		if alpha[i] != got[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatalf("头顺序退化成了字母序（露馅信号）：%v", got)
	}

	// 关键锚点：sec-ch-ua 必须早于 user-agent、user-agent 必须早于 accept-encoding。
	assertBefore(t, got, "Sec-Ch-Ua", "User-Agent")
	assertBefore(t, got, "User-Agent", "Accept-Encoding")
	assertBefore(t, got, "Accept-Encoding", "Cookie")
}

// TestOrderedHeaderNamesAppendsUnknown 校验未登记头稳定追加到末尾、不丢头。
func TestOrderedHeaderNamesAppendsUnknown(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "x")
	h.Set("Zeta-Custom", "x")
	h.Set("Alpha-Custom", "x")

	got := orderedHeaderNames(h)
	if len(got) != 3 {
		t.Fatalf("期望 3 个头，实际 %v", got)
	}
	if got[0] != "User-Agent" {
		t.Fatalf("已登记头应在前，实际 %v", got)
	}
	// 未登记头按字母序追加。
	if got[1] != "Alpha-Custom" || got[2] != "Zeta-Custom" {
		t.Fatalf("未登记头应按字母序追加，实际 %v", got)
	}
}

func assertBefore(t *testing.T, order []string, a, b string) {
	t.Helper()
	ia, ib := -1, -1
	for i, name := range order {
		if name == a {
			ia = i
		}
		if name == b {
			ib = i
		}
	}
	if ia < 0 || ib < 0 {
		t.Fatalf("锚点缺失：%q(%d) 或 %q(%d) 不在 %v", a, ia, b, ib, order)
	}
	if ia >= ib {
		t.Fatalf("顺序错误：%q 应早于 %q，实际 %v", a, b, order)
	}
}

// toLower 供测试用的小写化（与生产代码 strings.ToLower 一致，避免额外 import）。
func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
