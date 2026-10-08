package router

import (
	"testing"
	"time"

	"ps2api/internal/store"
)

func TestIsFreePlanForCleanup(t *testing.T) {
	cases := []struct {
		plan             string
		limit, remaining float64
		want             bool
	}{
		{"FREE_USER", 500, 0, true},
		{"FREE_USER", 500, 300, true},       // FREE_USER 无条件清（不管余量）
		{"FREE_USER", 0, 0, true},           // 额度未知的 FREE_USER 也清
		{"sync-free-202603", 50, 0, true},   // sync-free 耗尽才清
		{"sync-free-202603", 50, 23, false}, // sync-free 还有余量，保留
		{"sync-free-202603", 0, 0, false},   // 额度未知不清（信息不足）
		{"sync-solo-trial-202603", 400, 399, false},
		{"", 0, 0, false},
		{"PAID_USER", 400000, 55701, false},
	}
	for _, c := range cases {
		if got := isFreePlanForCleanup(c.plan, c.limit, c.remaining); got != c.want {
			t.Errorf("isFreePlanForCleanup(%q,%v,%v) = %v, want %v", c.plan, c.limit, c.remaining, got, c.want)
		}
	}
}

// 钉住清理口径的两个保护条件：观察期一个自然日；FREE_USER 无条件清。
func TestFreeCleanupGuardrails(t *testing.T) {
	if freeCleanupGraceDays != 1 {
		t.Fatalf("freeCleanupGraceDays = %d, want 1 (注册观察期一个自然日)", freeCleanupGraceDays)
	}
	var acc store.Account
	acc.Plan = "FREE_USER"
	acc.QuotaLimit = 500
	acc.CreatedAt = time.Now().Add(-2 * 24 * time.Hour)
	if !isFreePlanForCleanup(acc.Plan, acc.QuotaLimit, acc.QuotaRemaining) {
		t.Fatal("老 FREE_USER 应命中清理口径")
	}
	// 观察期内（刚注册）的 FREE_USER 不应被立即清——由 purgeFreeAccountsOnce 的
	// CreatedAt 检查保证，这里钉住时间比较方向：
	acc.CreatedAt = time.Now()
	grace := time.Now().AddDate(0, 0, -freeCleanupGraceDays)
	if !acc.CreatedAt.After(grace) {
		t.Fatal("刚注册账号必须落在观察期内")
	}
}
