package router

import (
	"context"
	"log"
	"strings"
	"time"
)

// freeAccountCleanup.go —— 定时清理 FREE 账号。
//
// 背景（2026-10-08 风控实验结论）：Postman 上游安全分类器按「内容长度 × 账号套餐」分概率
// 拦截——FREE/sync-free 号阈值极低（长内容 flag 概率≈1），sync-solo-trial/PAID 号明显更宽。
// FREE 号在号池里不仅几乎交付不了结果，还会烧掉 failover 预算并污染 flag 统计，留着弊大于利。
// 本任务定期删除 plan 为 FREE 的账号（含额度归零的 sync-free 前缀号）。
//
// 删除口径（满足其一即删）：
//   - plan == "FREE_USER"（usage.userType 口径的免费号）
//   - plan 前缀 "sync-free-" 且额度已耗尽（remaining<=0）——未耗尽的 sync-free 还有 50 额度可用
//
// 保护条件：手动停用（enabled=false）的号不删——那是人留着的；今天注册的号不删——给注册
// 产线留一个自然日观察期，避免刚落库就被清掉。删除走 Store.DeleteAccount（request_logs
// 的 account_id 置 NULL，不留悬挂引用）。

// freeCleanupDays 注册观察期（天）：账号注册后满该天数才会被纳入清理。
const freeCleanupGraceDays = 1

// StartDailyFreeCleanup 每天本地时间 23:10（额度刷新/日志清理之后）清理 FREE 账号，直到 ctx 取消。
// 进程启动时先立即执行一次：长驻进程可能连着数天不重启，启动先清一遍保持池子干净。
func (r *Router) StartDailyFreeCleanup(ctx context.Context) {
	const hour = 23
	r.purgeFreeAccountsOnce()
	for {
		timer := time.NewTimer(time.Until(nextDaily(time.Now(), hour).Add(10 * time.Minute)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			r.purgeFreeAccountsOnce()
		}
	}
}

// purgeFreeAccountsOnce 执行一次 FREE 账号清理并打日志。
func (r *Router) purgeFreeAccountsOnce() {
	accounts, err := r.Store.ListAccounts()
	if err != nil {
		log.Printf("[free-cleanup] 列出账号失败: %v", err)
		return
	}
	grace := time.Now().AddDate(0, 0, -freeCleanupGraceDays)
	removed := 0
	for _, acc := range accounts {
		if !isFreePlanForCleanup(acc.Plan, acc.QuotaLimit, acc.QuotaRemaining) {
			continue
		}
		// 手动停用的号不删：停用是人工决定，清理器不越权。
		if !acc.Enabled {
			continue
		}
		// 观察期内的号不删：给注册产线留一个自然日（建库时间可能缺省，缺失按刚注册处理）。
		if acc.CreatedAt.IsZero() || acc.CreatedAt.After(grace) {
			continue
		}
		if err := r.Store.DeleteAccount(acc.ID); err != nil {
			log.Printf("[free-cleanup] 删除账号 %d(%s) 失败: %v", acc.ID, acc.Email, err)
			continue
		}
		removed++
		log.Printf("[free-cleanup] 已删除 FREE 账号 %d(%s) plan=%s remaining=%.0f/%.0f", acc.ID, acc.Email, acc.Plan, acc.QuotaRemaining, acc.QuotaLimit)
	}
	if removed > 0 {
		log.Printf("[free-cleanup] FREE 账号清理完成: 删除 %d 个", removed)
	}
}

// isFreePlanForCleanup 判断账号是否属于「应清理的 FREE 套餐」。
//   - plan == "FREE_USER"：usage.userType 口径的免费号，无条件清；
//   - plan 前缀 "sync-free-"：注册产线的同步免费号，仅当额度确认耗尽（limit>0 且 remaining<=0）
//     才清——还有余量的 sync-free 号（50/50）仍然能交付请求。
//   - plan 为空（额度从未刷新过）不清：信息不足，宁可保留。
func isFreePlanForCleanup(plan string, quotaLimit, quotaRemaining float64) bool {
	if plan == "FREE_USER" {
		return true
	}
	if strings.HasPrefix(plan, "sync-free-") {
		return quotaLimit > 0 && quotaRemaining <= 0
	}
	return false
}
