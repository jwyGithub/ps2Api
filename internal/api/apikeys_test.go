package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// newTestStore 开一个临时库供 api 层测试使用（api_keys 表为空 = 引导态）。
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/api_test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKeySlots(t *testing.T) {
	var s keySlots
	// limit=0（不限）应始终可获取：不同 id 各取一次，避免 SA4000（|| 两侧表达式相同）。
	if !s.acquire(1, 0) || !s.acquire(2, 0) {
		t.Fatal("limit=0 (不限) 应始终可获取")
	}
	s.release(1) // 释放不存在的计数不应 panic / 变负
	// 连续两次 acquire 占满并发 2。
	if !s.acquire(1, 2) {
		t.Fatal("并发 2 内第一次获取应成功")
	}
	if !s.acquire(1, 2) {
		t.Fatal("并发 2 内第二次获取应成功")
	}
	if s.acquire(1, 2) {
		t.Fatal("超过并发限制应拒绝")
	}
	s.release(1)
	if !s.acquire(1, 2) {
		t.Fatal("释放后应恢复可获取")
	}
}

// TestAPIKeyAuth 覆盖鉴权全口径：引导态开放 / 未知 / 停用 / 过期 / 超额 / 通过。
func TestAPIKeyAuth(t *testing.T) {
	srv := &Server{Store: newTestStore(t)}
	expired := time.Now().Add(-time.Hour)
	if _, err := srv.Store.CreateAPIKey("sk-valid", "有效", nil, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store.CreateAPIKey("sk-expired", "过期", &expired, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Store.CreateAPIKey("sk-disabled", "停用", nil, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.UpdateAPIKey(3, "停用", nil, 0, 0, 1, false); err != nil {
		t.Fatal(err)
	}
	kQuota, err := srv.Store.CreateAPIKey("sk-quota", "超额", nil, 100, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.AddAPIKeyUsage(kQuota.ID, 100); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		key        string
		wantStatus int // 期望 auth 的结果：200=通过（未写响应），其它=拒绝时写下的状态码
	}{
		{"sk-valid", 200},
		{"sk-unknown", 401},
		{"sk-disabled", 401},
		{"sk-expired", 401},
		{"sk-quota", 429},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+c.key)
		w := httptest.NewRecorder()
		ok := srv.auth(w, req)
		if c.wantStatus == 200 && !ok {
			t.Fatalf("key=%q 应通过，got %d %s", c.key, w.Code, w.Body.String())
		}
		if c.wantStatus != 200 && (ok || w.Code != c.wantStatus) {
			t.Fatalf("key=%q 期望拒绝(%d)，got ok=%v code=%d body=%s", c.key, c.wantStatus, ok, w.Code, w.Body.String())
		}
	}

	// 引导态：一个密钥都没有的独立实例应全开放。
	boot := &Server{Store: newTestStore(t)}
	w := httptest.NewRecorder()
	if !boot.auth(w, httptest.NewRequest("POST", "/v1/chat/completions", nil)) {
		t.Fatalf("引导态（无密钥）应开放，got %d %s", w.Code, w.Body.String())
	}
}

// TestChargeKey 按 credits×倍率 回写用量（2026-09-23 ad08a82 起计量口径从
// token 估算改为 AI credits，见 provider.Result.Credits 注释）。
func TestChargeKey(t *testing.T) {
	srv := &Server{Store: newTestStore(t)}
	k, err := srv.Store.CreateAPIKey("sk-charge", "计费", nil, 0, 0, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), keyCtxKey{}, k)
	// credits=40 → ceil(40×1.5)=60。tokens 字段仍在响应 usage 里报告，但不参与计费。
	srv.chargeKey(ctx, &provider.Result{Credits: 40, PromptTokens: 10, CompletionTokens: 30})
	got, _ := srv.Store.GetAPIKey(k.ID)
	if got.QuotaUsed != 60 { // ceil(40*1.5)
		t.Fatalf("quota_used = %d, want 60", got.QuotaUsed)
	}
	// 无密钥的 ctx 是 no-op；零 credits 也 no-op（旧 token 字段不再触发计费）。
	srv.chargeKey(context.Background(), &provider.Result{Credits: 1})
	srv.chargeKey(ctx, &provider.Result{PromptTokens: 10, CompletionTokens: 30})
	srv.chargeKey(ctx, &provider.Result{})
	got, _ = srv.Store.GetAPIKey(k.ID)
	if got.QuotaUsed != 60 {
		t.Fatalf("no-op charge changed usage: %d", got.QuotaUsed)
	}
}

// TestOpsAPIKeyAuth 钉住排查端点的 Key 放行：有效 Key 直接过（先 Key 后会话），
// 无 Key + 已设密码时 401，清单外端点不得被 Key 放行。
func TestOpsAPIKeyAuth(t *testing.T) {
	srv := &Server{Store: newTestStore(t)}
	if _, err := srv.Store.CreateAPIKey("sk-ops", "排查", nil, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_PASSWORD", "pw") // 开登录：逼出「只认会话」的默认分支

	ops := []struct{ method, path string }{
		{"GET", "/api/waf/analyze?log_id=1"},
		{"GET", "/api/waf/baselines?log_id=1"},
		{"GET", "/api/waf-signatures"},
		{"POST", "/api/waf/probe"},
		{"GET", "/api/waf/probe/probe-1"},
		{"DELETE", "/api/waf/probe/probe-1"},
		{"POST", "/api/sql-query"},
		{"GET", "/api/request-logs"},
		{"GET", "/api/logs"},
		{"GET", "/api/stats"},
	}
	for _, c := range ops {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer sk-ops")
		w := httptest.NewRecorder()
		if !srv.auth(w, req) {
			t.Fatalf("%s %s 带有效 Key 应放行，got %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	// 无 Key：401（已设密码、无会话）。
	req := httptest.NewRequest("POST", "/api/sql-query", nil)
	w := httptest.NewRecorder()
	ok := srv.auth(w, req)
	if ok || w.Code != 401 {
		t.Fatalf("无 Key 应 401，got ok=%v code=%d", ok, w.Code)
	}
	// 清单外端点不得被 Key 放行（敏感管理面）。
	for _, p := range []string{"/api/keys", "/api/settings", "/api/analytics", "/api/proxy-check"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Authorization", "Bearer sk-ops")
		w := httptest.NewRecorder()
		if srv.auth(w, req) {
			t.Fatalf("%s 不得被 Key 放行", p)
		}
	}
	// 引导态（无密码、无 Key）：放行不变。
	boot := &Server{Store: newTestStore(t)}
	if !boot.auth(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/sql-query", nil)) {
		t.Fatal("引导态应开放")
	}
}
