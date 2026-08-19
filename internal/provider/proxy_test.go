package provider

import "testing"

func TestParseProxyURLs(t *testing.T) {
	in := []string{"socks5://127.0.0.1:1080, http://u:p@host:3128\nhttps://a.example:8443 ; not-a-url\nsocks5://127.0.0.1:1080"}
	got := parseProxyURLs(in)
	want := []string{"socks5://127.0.0.1:1080", "http://u:p@host:3128", "https://a.example:8443"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseProxyURLsSkipsInvalid(t *testing.T) {
	// 缺 scheme 或 host 的项应被跳过，保证 N 只计有效出口。
	if got := parseProxyURLs([]string{"host:3128, ://x, http://"}); len(got) != 0 {
		t.Fatalf("expected no valid urls, got %v", got)
	}
}

func TestProxyPoolNoConfigFallsBackToDirect(t *testing.T) {
	pp := newProxyPool() // list == nil
	if _, _, ok := pp.selectFor(1, 0); ok {
		t.Fatal("no proxy configured must return ok=false (direct)")
	}
	pp.list = func() []string { return nil }
	if _, _, ok := pp.selectFor(1, 0); ok {
		t.Fatal("empty proxy list must return ok=false (direct)")
	}
}

func TestProxyPoolStickyAndRotateOnAttempt(t *testing.T) {
	urls := []string{"http://p0:1", "http://p1:1", "http://p2:1"}
	pp := newProxyPool()
	pp.list = func() []string { return urls }

	// 同一账号、attempt 0：粘性出口稳定不变。
	_, e0a, ok := pp.selectFor(7, 0)
	if !ok {
		t.Fatal("attempt 0 should select a proxy")
	}
	if _, e0b, _ := pp.selectFor(7, 0); e0b != e0a {
		t.Fatalf("sticky egress must be stable: %q vs %q", e0a, e0b)
	}

	// attempt 递增 → 依次切到不同出口。
	_, e1, _ := pp.selectFor(7, 1)
	_, e2, _ := pp.selectFor(7, 2)
	if e0a == e1 || e1 == e2 || e0a == e2 {
		t.Fatalf("egress must rotate across attempts: %q %q %q", e0a, e1, e2)
	}

	// attempt >= N（所有出口都试过）→ 回退直连。
	if _, _, ok := pp.selectFor(7, 3); ok {
		t.Fatal("egressAttempt >= N must fall back to direct (ok=false)")
	}
}

func TestProxyPoolClientCachedAndReused(t *testing.T) {
	pp := newProxyPool()
	c1 := pp.clientFor("http://cache-me:8080")
	c2 := pp.clientFor("http://cache-me:8080")
	if c1 == nil || c1 != c2 {
		t.Fatal("clientFor must cache and reuse the same *http.Client per URL")
	}
	if pp.clientFor("::::bad") != nil {
		t.Fatal("invalid URL must yield nil client")
	}
}
