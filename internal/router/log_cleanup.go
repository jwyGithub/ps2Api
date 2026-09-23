package router

import (
	"context"
	"log"
	"time"
)

// logRetentionDays 请求日志保留天数：只保留最近 7 天，更早的记录每日清理。
const logRetentionDays = 7

// StartDailyLogCleanup 每天本地时间 23:00 清理超过 logRetentionDays 天的请求日志，直到 ctx 取消。
// 复用 nextDaily 的对齐逻辑，与每日额度刷新同一时刻触发但各自独立 goroutine、互不影响。
// 进程启动时先立即清理一次：桌面场景常常不会正好开机到 23:00，先清一遍避免旧日志一直堆积。
func (r *Router) StartDailyLogCleanup(ctx context.Context) {
	const hour = 23
	r.purgeOldLogsOnce()
	for {
		timer := time.NewTimer(time.Until(nextDaily(time.Now(), hour)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			r.purgeOldLogsOnce()
		}
	}
}

// purgeOldLogsOnce 执行一次日志清理并打日志，失败只记录不中断循环。
func (r *Router) purgeOldLogsOnce() {
	n, err := r.Store.PurgeRequestLogsOlderThan(logRetentionDays)
	if err != nil {
		log.Printf("[log-cleanup] 请求日志清理失败: %v", err)
		return
	}
	log.Printf("[log-cleanup] 请求日志清理完成: 删除 %d 条超过 %d 天的记录", n, logRetentionDays)
}
