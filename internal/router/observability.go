package router

import (
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// alertRequestRejected 在请求被网关拒绝且带有排查上下文（如 Cloudflare 403 的 Ray ID、
// 出站 body 大小、响应体片段）时写入一条告警，展示到仪表盘的告警面板，方便定位 403 诱因。
// 无诊断详情（普通坏请求/工具名冲突等）时不打扰。按账号去重，避免同号连续 403 刷屏。
func (r *Router) alertRequestRejected(acc *store.Account, res *provider.Result) {
	if res == nil || res.RejectionDetail == "" {
		return
	}
	title := "请求被网关拒绝: " + acc.Email
	msg := res.Error + "\n" + res.RejectionDetail
	// 网关(Cloudflare)拦截时附上近 1 小时 403 按请求体大小的分布，用真实数据佐证
	// body 大小与 403 是否相关，而非仅凭当前单条请求臆测。
	if res.GatewayBlocked {
		if dist, err := r.Store.Cloudflare403BodySizeSummary(60 * time.Minute); err == nil && dist != "" {
			msg += "\n\n" + dist
		}
	}
	_ = r.Store.CreateAlert("warning", title, msg, "account", &acc.ID, "gateway_rejected")
}

// logAttempt 把每次上游调用（无论成败）都写入 request_logs，
// 失败次数、错误率、平均延迟、P95 等指标全部来自真实日志。
func (r *Router) logAttempt(acc *store.Account, model string, res *provider.Result, started time.Time, endpoint string) {
	l := &store.RequestLog{AccountID: &acc.ID, Model: model, Endpoint: endpoint, DurationMs: time.Since(started).Milliseconds(), RequestBytes: res.RequestBytes}
	if res.Success {
		l.Status = "success"
		l.PromptTokens = res.PromptTokens
		l.CompletionTokens = res.CompletionTokens
		l.TotalTokens = res.PromptTokens + res.CompletionTokens
	} else {
		l.Status = "error"
		l.ErrorMessage = res.Error
	}
	_ = r.Store.LogRequest(l)
}

// persistQuota 把聊天流 usage 与响应头限流快照写入账号。
func (r *Router) persistQuota(acc *store.Account, res *provider.Result) {
	if res == nil {
		return
	}
	if usage := res.Usage; usage != nil && usage.Limit > 0 {
		remaining := usage.Limit - usage.Usage - usage.Overage
		if remaining < 0 || res.QuotaExhausted {
			remaining = 0
		}
		thresholds := make([]store.QuotaThreshold, len(usage.WarningThresholds))
		for i, threshold := range usage.WarningThresholds {
			thresholds[i] = store.QuotaThreshold{Value: threshold.Value, Unit: threshold.Unit}
		}
		var cycleStart, cycleEnd *time.Time
		if usage.UsageCycle != nil {
			cycleStart, cycleEnd = &usage.UsageCycle.Start, &usage.UsageCycle.End
		}
		_ = r.Store.SetQuotaSnapshot(acc.ID, store.QuotaSnapshot{
			Plan: usage.UserType, State: usage.UsageState, Limit: usage.Limit, Used: usage.Usage,
			Remaining: remaining, Overage: usage.Overage, Spillage: usage.Spillage,
			AllowOverage: usage.AllowOverage, TeamPooled: usage.IsTeamPooled,
			WarningThresholds: thresholds, CycleStart: cycleStart, CycleEnd: cycleEnd,
		})
	}
	if rate := res.RateLimit; rate != nil {
		_ = r.Store.SetRateLimit(acc.ID, rate.Limit, rate.Remaining, rate.WindowSeconds, rate.ResetAt)
	}
}
