package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ps2api/internal/store"
)

// 零密钥引导态契约：api_keys 表为空时 /api/* 全开放——不带头、或带着同源残留的
// 失效 Bearer（浏览器 localStorage 里旧部署的密钥）都不许拒绝。否则首个密钥建好前
// 面板就被锁死（导入账号、新增 key 全部 invalid_api_key）。
func TestBootstrapOpenWithoutKeys(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(db)
	mux := http.NewServeMux()
	srv.Register(mux)

	for _, tc := range []struct {
		name, method, path, body, bearer string
	}{
		{"list keys", "GET", "/api/keys", "", ""},
		{"create key", "POST", "/api/keys", `{"name":"a"}`, ""},
		{"import accounts", "POST", "/api/accounts/import", `{"version":1,"accounts":[]}`, ""},
		{"import accounts with stale bearer", "POST", "/api/accounts/import", `{"version":1,"accounts":[]}`, "sk-stale-from-old-deploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 每个子例用独立库：建过 key 之后「表非空→需鉴权」是另一条（正确）路径，别混进来。
			db, err := store.Open(t.TempDir() + "/t.db")
			if err != nil {
				t.Fatal(err)
			}
			s := New(db)
			m := http.NewServeMux()
			s.Register(m)

			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, req)
			if rec.Code == 401 || strings.Contains(rec.Body.String(), "invalid_api_key") {
				t.Fatalf("bootstrap state rejected: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// 面板与密钥解耦后的鉴权口径（设了 ADMIN_PASSWORD）：
//   - /api/* 只认登录会话，任一有效 API Key 的 Bearer 不再是面板凭据；
//   - /v1/* 认 API Key，不认会话 Cookie；
//   - 「网关测试」回环令牌过 /v1 鉴权（不依赖任何业务密钥存在）。
func TestPanelSessionVsV1KeyAuth(t *testing.T) {
	const password = "pw-test"
	t.Setenv("ADMIN_PASSWORD", password)

	db, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAPIKey("sk-panel", "测试", nil, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	srv := New(db)
	mux := http.NewServeMux()
	srv.Register(mux)

	// 登录拿会话 Cookie。
	login := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	login.Header.Set("Content-Type", "application/json")
	lr := httptest.NewRecorder()
	mux.ServeHTTP(lr, login)
	if lr.Code != 200 {
		t.Fatalf("login = %d %s", lr.Code, lr.Body.String())
	}
	cookie := lr.Header().Get("Set-Cookie")
	if cookie == "" {
		t.Fatal("login 未签发会话 Cookie")
	}

	cases := []struct {
		name, path, bearer, cookie string
		want                       int
	}{
		{"panel rejects valid api key", "/api/keys", "sk-panel", "", 401},
		{"panel accepts session", "/api/keys", "", cookie, 200},
		{"v1 rejects session cookie", "/v1/models", "", cookie, 401},
		{"v1 accepts api key", "/v1/models", "sk-panel", "", 200},
		{"v1 accepts svc loopback token", "/v1/models", srv.svcTokenIssue(), "", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", c.path, nil)
			if c.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+c.bearer)
			}
			if c.cookie != "" {
				req.Header.Set("Cookie", cookie)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("%s -> %d (want %d): %s", c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}

	// 回环令牌注销后立即失效。
	tok := srv.svcTokenIssue()
	srv.svcTokenRevoke(tok)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("revoked svc token -> %d (want 401)", rec.Code)
	}
}
