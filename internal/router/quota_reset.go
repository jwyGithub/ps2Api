package router

import (
	"context"
	"log"
	"sync"
	"time"

	"ps2api/internal/store"
)

// RefreshDueQuotas 对「额度周期重置时间（QuotaCycleEnd）已过」的启用账号发起探测刷新。
// 供每日定时任务调用；探测成功后新的 cycleEnd（在未来）经 persistQuota 落库，
// 自然去重——同一周期不会被重复刷新。刻意不跳过 exhausted 账号：重置后探测到
// AVAILABLE 且余量恢复，applyUsageState 会自动把它转回 active，这正是本任务的目的。
// 只跳过手动停用 / BLOCKED 停用的账号（enabled=false）。
func (r *Router) RefreshDueQuotas(ctx context.Context) []ProbeResult {
	accounts, err := r.Store.ListAccounts()
	if err != nil {
		return nil
	}
	now := time.Now()
	var out []ProbeResult
	sem := make(chan struct{}, probeConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, acc := range accounts {
		if !acc.Enabled || acc.QuotaCycleEnd == nil || acc.QuotaCycleEnd.After(now) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			pr := r.probeAccountQuota(ctx, acc, false)
			mu.Lock()
			out = append(out, pr)
			mu.Unlock()
		}(acc)
	}
	wg.Wait()
	return out
}

// StartDailyQuotaRefresh 每天本地时间 23:00 检查是否有额度周期已重置的账号并自动刷新，
// 直到 ctx 取消。重置时间如 09/15 21:23 的账号会在当晚 23:00 被扫到并刷新。
func (r *Router) StartDailyQuotaRefresh(ctx context.Context) {
	const hour = 23
	for {
		timer := time.NewTimer(time.Until(nextDaily(time.Now(), hour)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			results := r.RefreshDueQuotas(ctx)
			ok, failed := 0, 0
			for _, pr := range results {
				if pr.OK {
					ok++
				} else {
					failed++
				}
			}
			log.Printf("[quota-refresh] 每日额度刷新完成: %d 个周期已重置账号, 成功 %d, 失败 %d", len(results), ok, failed)
		}
	}
}

// nextDaily 返回 now 之后（严格大于 now）下一个本地时间 hour 点整。
// now 已过今日 hour 点则返回明天同一时刻，保证不会立即触发。
func nextDaily(now time.Time, hour int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
