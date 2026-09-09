package api

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ps2api/internal/router"
	"ps2api/internal/store"
)

// newLoginServer 启一个带登录环境变量的面板测试服务（API Key 留空，
// 面板默认引导态，鉴权只考察登录会话本身）。
func newLoginServer(t *testing.T, password string) *httptest.Server {
	t.Helper()
	t.Setenv("ADMIN_USERNAME", "admin")
	t.Setenv("ADMIN_PASSWORD", password)
	db, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Server{Store: db, Router: router.New(db)}).Register(mux)
	return httptest.NewServer(mux)
}

func postLogin(ts *httptest.Server, body string) (*http.Response, error) {
	return http.Post(ts.URL+"/api/login", "application/json", strings.NewReader(body))
}

func TestLoginWrongCredentials(t *testing.T) {
	ts := newLoginServer(t, "s3cret!")
	defer ts.Close()
	for _, body := range []string{
		`{"username":"admin","password":"bad"}`,
		`{"username":"root","password":"s3cret!"}`,
	} {
		res, err := postLogin(ts, body)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("%s 应 401，got %d", body, res.StatusCode)
		}
	}
}

func TestLoginIssuesSessionCookieAndGrantsPanelAccess(t *testing.T) {
	ts := newLoginServer(t, "s3cret!")
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	res, err := client.Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"s3cret!"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("正确凭据应 200，got %d", res.StatusCode)
	}

	home, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	home.Body.Close()
	if home.StatusCode != 200 {
		t.Fatalf("登录后访问 / 应 200，got %d", home.StatusCode)
	}
}

func TestDashboardRedirectsToLoginWhenSessionMissing(t *testing.T) {
	ts := newLoginServer(t, "s3cret!")
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 302 || res.Header.Get("Location") != "/login" {
		t.Fatalf("未登录访问 / 应 302 跳 /login，got %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestLoginDisabledWithoutPassword(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	db, err := store.Open(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Server{Store: db, Router: router.New(db)}).Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("未设 ADMIN_PASSWORD 时 / 应直接可访问，got %d", res.StatusCode)
	}
	res2, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != 302 || res2.Header.Get("Location") != "/" {
		t.Fatalf("登录关闭时 /login 应 302 跳 /，got %d %s", res2.StatusCode, res2.Header.Get("Location"))
	}
}
