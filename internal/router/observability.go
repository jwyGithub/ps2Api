package router

import (
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// logAttempt 把每次上游调用（无论成败）都写入 request_logs，
// 失败次数、错误率、平均延迟、P95 等指标全部来自真实日志。
func (r *Router) logAttempt(acc *store.Account, req *provider.ChatRequest, res *provider.Result, started time.Time) {
	// 计量口径：本次实际消耗的 AI credits（上游累计用量增量），取代旧的 token 估算。
	// 就地写回 res.Credits，供 API Key 计费（chargeKey）复用同一口径。
	res.Credits = creditsConsumed(acc, res)
	l := &store.RequestLog{
		AccountID:       &acc.ID,
		Credits:         res.Credits,
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
	} else {
		l.Status = "error"
		l.ErrorMessage = res.Error
	}
	_ = r.Store.LogRequest(l)
}

// creditsConsumed 计算本次请求实际消耗的 AI credits：上游返回的累计用量(usage.Usage)
// 减去账号本次请求前的用量快照(acc.QuotaUsed)。没有有效 usage、或账号尚无基线
// (QuotaUsed<=0，通常是首次采集)时记 0，避免把累计值误当单次增量；累计用量单调不减，
// 负增量(周期重置等)一律归 0。计算后就地把 acc.QuotaUsed 推进到最新累计值，
// 使同一账号的连续重试不会重复计量。
func creditsConsumed(acc *store.Account, res *provider.Result) float64 {
	if res == nil || res.Usage == nil || res.Usage.Limit <= 0 {
		return 0
	}
	if acc.QuotaUsed <= 0 {
		// 首次没有基线：仅建立基线，本次不计（下次起增量才可信）。
		if res.Usage.Usage > 0 {
			acc.QuotaUsed = res.Usage.Usage
		}
		return 0
	}
	delta := res.Usage.Usage - acc.QuotaUsed
	acc.QuotaUsed = res.Usage.Usage
	if delta < 0 {
		return 0
	}
	return delta
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
