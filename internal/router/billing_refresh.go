package router

import (
	"context"
	"log"
	"sync"
	"time"
)

// billing_refresh.go —— 对话后异步刷新 billing ops 权威额度。
//
// 背景（2026-10-08 额度统一来源改造）：额度有两个来源——对话流 SSE usage 事件（millicredits
// 量纲，经 scaleUsageToCredits 除以 100 对齐）与 billing operations 直查（credits 原生口径）。
// 两者 plan/userType/周期字段形状不同，快照互相覆盖曾导致 plan 污染与数值漂移。统一后：
// 对话路径的 persistQuota 只写兜底快照，随后调度本任务用 billing ops 权威值覆盖——
// 库中额度永远向 operations.ai_millicredits 收敛，单一可信来源。
//
// 去抖：同账号 30s 内的多次对话只触发一次刷新（对话常成串到达，逐次刷新纯浪费往返）。
// 异步执行，不阻塞对话响应；失败静默（下次对话或每日 23:00 刷新兜底）。

// billingRefreshDebounce 是同账号两次刷新调度的最小间隔。
const billingRefreshDebounce = 30 * time.Second

// billingRefreshDelay 是调度后的延迟执行时间——等对话流完全收尾（usage 事件通常在
// 流末尾，立即查 billing 可能还读不到本次消耗）。
const billingRefreshDelay = 3 * time.Second

type billingRefresher struct {
	mu      sync.Mutex
	lastAt  map[int64]time.Time
	pending map[int64]bool // 已调度未执行，防重复排队
}

func newBillingRefresher() *billingRefresher {
	return &billingRefresher{lastAt: map[int64]time.Time{}, pending: map[int64]bool{}}
}

// scheduleBillingRefresh 异步调度一次 billing ops 刷新（去抖 + 延迟执行）。
func (r *Router) scheduleBillingRefresh(accountID int64) {
	if r.billingRefresh == nil {
		return
	}
	r.billingRefresh.mu.Lock()
	if now := time.Now(); now.Sub(r.billingRefresh.lastAt[accountID]) < billingRefreshDebounce ||
		r.billingRefresh.pending[accountID] {
		r.billingRefresh.mu.Unlock()
		return
	}
	r.billingRefresh.lastAt[accountID] = time.Now()
	r.billingRefresh.pending[accountID] = true
	r.billingRefresh.mu.Unlock()

	go func() {
		time.Sleep(billingRefreshDelay)
		acc, err := r.Store.GetAccount(accountID)
		if err != nil || acc == nil || !acc.Enabled {
			r.billingRefresh.mu.Lock()
			r.billingRefresh.pending[accountID] = false
			r.billingRefresh.mu.Unlock()
			return
		}
		// 零消耗直查；失败不回退烧 token 探测（对话后不必再烧额度），静默等下次兜底。
		if !r.fetchBillingOpsResult(context.Background(), acc, ProbeResult{AccountID: acc.ID, Email: acc.Email}) {
			log.Printf("[billing-refresh] 账号 %d(%s) 对话后刷新额度失败（静默，等下次兜底）", acc.ID, acc.Email)
		}
		r.billingRefresh.mu.Lock()
		r.billingRefresh.pending[accountID] = false
		r.billingRefresh.mu.Unlock()
	}()
}
