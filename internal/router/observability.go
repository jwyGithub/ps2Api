package router

import (
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// logAttempt 把每次上游调用（无论成败）都写入 request_logs，
// 失败次数、错误率、平均延迟、P95 等指标全部来自真实日志。
func (r *Router) logAttempt(acc *store.Account, req *provider.ChatRequest, res *provider.Result, started time.Time) {
	l := &store.RequestLog{
		AccountID:       &acc.ID,
		Model:           req.Model,
		Endpoint:        req.Endpoint,
		DurationMs:      time.Since(started).Milliseconds(),
		RequestBytes:    res.RequestBytes,
		Path:            req.ClientPath,
		Stream:          req.Stream,
		ClientBody:      req.ClientBody,
		ClientHeaders:   req.ClientHeaders,
		UpstreamBody:    res.UpstreamBody,
		UpstreamHeaders: res.UpstreamHeaders,
		UpstreamURL:     res.UpstreamURL,
		Egress:          res.Egress,
		ConversationID:  res.ConversationID,
	}
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
