package provider

import (
	"net/http"
	"net/url"
	"testing"
)

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

	// 启用出口代理后每次出站都必须经代理：attempt >= N 时按出口数环回到有效出口，
	// 而非回退本机直连——保证「开启即全量走代理」。此处 (stickyBase(7)+3)%3 == (7+0)%3，
	// 因此 attempt 3 环回到 attempt 0 的同一出口。
	_, e3, ok := pp.selectFor(7, 3)
	if !ok {
		t.Fatal("egressAttempt >= N must still select a proxy (wrap), never fall back to direct")
	}
	if e3 != e0a {
		t.Fatalf("egressAttempt >= N must wrap to a valid egress: got %q want %q", e3, e0a)
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

func TestAccountCookieJarsAreIsolated(t *testing.T) {
	store := newAccountCookieJars()
	u, _ := url.Parse("https://example.com/chat")
	jar := store.jarFor(1, u.Host, "proxy-a")
	jar.SetCookies(u, []*http.Cookie{{Name: "__cf_bm", Value: "account-1"}})
	if got := store.cookies(1, u, "proxy-a"); len(got) != 1 || got[0].Value != "account-1" {
		t.Fatalf("same account cookie missing: %#v", got)
	}
	if got := store.cookies(2, u, "proxy-a"); len(got) != 0 {
		t.Fatalf("cookie leaked across accounts: %#v", got)
	}
	if got := store.cookies(1, u, "proxy-b"); len(got) != 0 {
		t.Fatalf("cookie leaked across egresses: %#v", got)
	}
}

func TestApplyCookiesMergesAndRefreshesSessionCookie(t *testing.T) {
	p := New()
	u, _ := url.Parse("https://example.com/chat")
	p.cookies.remember(1, u, "direct", []*http.Cookie{
		{Name: "postman.sid", Value: "new"},
		{Name: "__cf_bm", Value: "cf"},
	})
	req, _ := http.NewRequest(http.MethodPost, u.String(), nil)
	req.Header.Set("Cookie", "postman.sid=old; custom=value")
	p.applyCookies(1, req, "direct")
	if got := req.Header.Get("Cookie"); got != "custom=value; __cf_bm=cf; postman.sid=new" {
		t.Fatalf("merged cookies = %q", got)
	}
}
