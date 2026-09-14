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

// TestChargeKey 按 (prompt+completion)×倍率 回写用量。
func TestChargeKey(t *testing.T) {
	srv := &Server{Store: newTestStore(t)}
	k, err := srv.Store.CreateAPIKey("sk-charge", "计费", nil, 0, 0, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), keyCtxKey{}, k)
	srv.chargeKey(ctx, &provider.Result{PromptTokens: 10, CompletionTokens: 30})
	got, _ := srv.Store.GetAPIKey(k.ID)
	if got.QuotaUsed != 60 { // (10+30)*1.5
		t.Fatalf("quota_used = %d, want 60", got.QuotaUsed)
	}
	// 无密钥的 ctx 是 no-op；零 token 也 no-op。
	srv.chargeKey(context.Background(), &provider.Result{PromptTokens: 1, CompletionTokens: 1})
	srv.chargeKey(ctx, &provider.Result{})
	got, _ = srv.Store.GetAPIKey(k.ID)
	if got.QuotaUsed != 60 {
		t.Fatalf("no-op charge changed usage: %d", got.QuotaUsed)
	}
}
