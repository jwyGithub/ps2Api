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
//
// 额度统一来源（2026-10-08 改）：快照数值仅作兜底，权威来源是 billing ops 直查。
// 写完快照后异步调度一次 billing ops 刷新（零消耗、不阻塞响应、带去抖），
// 用 operations.ai_millicredits 的权威值覆盖快照——两条路径的 plan/量纲/周期就此归一。
func (r *Router) persistQuota(acc *store.Account, res *provider.Result) {
	if res == nil {
		return
	}
	// 额度统一来源（2026-10-08 定案）：usage 事件数值量纲随套餐漂移（FREE ÷100、trial ÷1000
	// 才能对齐 billing），不可落库——sse.go handleUsage 已把数值字段清零，此处只在
	// 「库中尚无快照」（QuotaLimit==0，新号首聊）时写状态占位（数值保持 0=未采集，
	// ProbeQuotas/metrics 对 0 的既有语义就是"待采集/跳过"），随后 scheduleBillingRefresh
	// 的权威值立刻接管。库中已有快照时数值完全不动——conversation 快照覆盖 billing
	// 权威值正是 400→4000 事故的根源。
	if usage := res.Usage; usage != nil && acc.QuotaLimit <= 0 {
		_ = r.Store.SetQuotaSnapshot(acc.ID, store.QuotaSnapshot{
			Plan: acc.Plan, State: usage.UsageState,
		})
	}
	if rate := res.RateLimit; rate != nil {
		_ = r.Store.SetRateLimit(acc.ID, rate.Limit, rate.Remaining, rate.WindowSeconds, rate.ResetAt)
	}
	// 对话后异步刷新一次 billing ops 权威额度（去抖：同账号 30s 内只跑一次）。
	r.scheduleBillingRefresh(acc.ID)
}
