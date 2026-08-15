package router

import (
	"context"
	"strconv"
	"sync"
	"time"

	"ps2api/internal/pool"
	"ps2api/internal/provider"
	"ps2api/internal/store"
)

type Router struct {
	Pool     *pool.Pool
	Provider *provider.Provider
	Store    *store.Store
}

func New(s *store.Store) *Router {
	return &Router{Pool: pool.New(s), Provider: provider.New(), Store: s}
}

// retryCount 从持久化设置读取请求重试次数（默认 3）。
func (r *Router) retryCount() int {
	v, _ := r.Store.GetSetting("retry_count")
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 3
	}
	return n
}

// failoverEnabled 从持久化设置读取「失败自动切换账号」开关（默认开启）。
func (r *Router) failoverEnabled() bool {
	v, _ := r.Store.GetSetting("failover_enabled")
	if v == "" {
		return true
	}
	on, err := strconv.ParseBool(v)
	if err != nil {
		return true
	}
	return on
}

// pickAccount 优先返回该会话粘性绑定的账号（续聊固定回首次使用的账号，
// 避免池子轮询换号导致 Postman 会话上下文丢失）；无会话、粘性账号失效或被
// 排除时回退到号池轮询。返回值 poolUsed 表示该账号来自 Pool（需要 Done）。
func (r *Router) pickAccount(excluded map[int64]bool, messages []provider.ChatMessage) (*store.Account, bool, error) {
	if accID, ok := r.Provider.StickyAccount(messages); ok && !excluded[accID] {
		if acc, err := r.Store.GetAccount(accID); err == nil && acc.Status == "active" && acc.Enabled {
			return acc, false, nil
		}
	}
	acc, err := r.Pool.Next(excluded)
	return acc, true, err
}

func (r *Router) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.Result, *store.Account, error) {
	var last string
	excluded := map[int64]bool{}
	attempts := r.retryCount()
	if !r.failoverEnabled() {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(100*(1<<attempt)) * time.Millisecond)
		}
		acc, poolUsed, err := r.pickAccount(excluded, req.Messages)
		if err != nil {
			provider.Trace(ctx, "router.error", map[string]interface{}{"attempt": attempt + 1, "error": err.Error()})
			return nil, nil, err
		}
		provider.Trace(ctx, "router.attempt", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "model": req.Model})
		started := time.Now()
		res := r.Provider.Chat(ctx, acc, req)
		if poolUsed {
			r.Pool.Done(acc.ID)
		}
		r.persistQuota(acc, res)
		r.logAttempt(acc, req.Model, res, started, req.Endpoint)
		if res.Success {
			provider.Trace(ctx, "router.success", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID})
			r.Pool.MarkUsed(acc.ID)
			return res, acc, nil
		}
		last = res.Error
		provider.Trace(ctx, "router.failure", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "error": res.Error})
		if res.QuotaExhausted {
			excluded[acc.ID] = true
			r.Pool.MarkExhausted(acc.ID)
			continue
		}
		if res.RateLimited || res.AuthFailed || pool.IsTransient(res.Error) {
			r.Pool.MarkTransient(acc.ID, res.Error)
			continue
		}
		r.Pool.MarkError(acc.ID, res.Error)
	}
	return nil, nil, &RouteError{Message: "All accounts failed. Last error: " + last}
}

func (r *Router) Stream(ctx context.Context, req *provider.ChatRequest, emit provider.EmitFunc) (*provider.Result, *store.Account, error) {
	var last string
	emitted := false
	trackedEmit := func(d provider.Delta) error {
		emitted = true
		return emit(d)
	}
	excluded := map[int64]bool{}
	attempts := r.retryCount()
	if !r.failoverEnabled() {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		acc, poolUsed, err := r.pickAccount(excluded, req.Messages)
		if err != nil {
			provider.Trace(ctx, "router.error", map[string]interface{}{"attempt": attempt + 1, "error": err.Error()})
			return nil, nil, err
		}
		provider.Trace(ctx, "router.attempt", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "model": req.Model, "stream": true})
		started := time.Now()
		res := r.Provider.StreamChat(ctx, acc, req, trackedEmit)
		if poolUsed {
			r.Pool.Done(acc.ID)
		}
		r.persistQuota(acc, res)
		r.logAttempt(acc, req.Model, res, started, req.Endpoint)
		if res.Success {
			provider.Trace(ctx, "router.success", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "stream": true})
			r.Pool.MarkUsed(acc.ID)
			return res, acc, nil
		}
		last = res.Error
		provider.Trace(ctx, "router.failure", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "error": res.Error, "stream": true})
		if res.QuotaExhausted {
			excluded[acc.ID] = true
			r.Pool.MarkExhausted(acc.ID)
			if emitted {
				return nil, nil, &RouteError{Message: "Stream failed after output started: " + last}
			}
			continue
		}
		if res.RateLimited || res.AuthFailed || pool.IsTransient(res.Error) {
			r.Pool.MarkTransient(acc.ID, res.Error)
			if emitted {
				return nil, nil, &RouteError{Message: "Stream failed after output started: " + last}
			}
			continue
		}
		if emitted {
			return nil, nil, &RouteError{Message: "Stream failed after output started: " + last}
		}
		r.Pool.MarkError(acc.ID, res.Error)
		break
	}
	return nil, nil, &RouteError{Message: "All accounts failed. Last error: " + last}
}

// logAttempt 把每次上游调用（无论成败）都写入 request_logs，
// 失败次数、错误率、平均延迟、P95 等指标全部来自真实日志。
func (r *Router) logAttempt(acc *store.Account, model string, res *provider.Result, started time.Time, endpoint string) {
	l := &store.RequestLog{AccountID: &acc.ID, Model: model, Endpoint: endpoint, DurationMs: time.Since(started).Milliseconds()}
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

type RouteError struct{ Message string }

func (e *RouteError) Error() string { return e.Message }

// ProbeResult 单个账号额度探测的结果。
type ProbeResult struct {
	AccountID int64   `json:"accountId"`
	Email     string  `json:"email"`
	OK        bool    `json:"ok"`
	Limit     float64 `json:"limit"`
	Remaining float64 `json:"remaining"`
	Error     string  `json:"error,omitempty"`
}

// probeConcurrency 额度探测的并发数。
const probeConcurrency = 3

// ProbeQuotas 对所有启用的账号（含已采集过额度的存量账号）发起一次轻量探测调用，
// 拿到真实额度写库并返回逐账号结果。单次探测仅消耗几 token，可用于核实
// limit/remaining 是否有变化；额度管理页「刷新额度」按钮调用的就是这个。
// 跳过已耗尽（exhausted）账号——上游直接拒绝，探测拿不到有效数据。
func (r *Router) ProbeQuotas(ctx context.Context) []ProbeResult {
	accounts, err := r.Store.ListAccounts()
	if err != nil {
		return nil
	}
	var out []ProbeResult
	sem := make(chan struct{}, probeConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, acc := range accounts {
		if !acc.Enabled || acc.Status == "exhausted" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			pr := ProbeResult{AccountID: acc.ID, Email: acc.Email}
			res := r.Provider.ProbeQuota(ctx, acc)
			// 限流头可能在没有 usage 对象的响应中返回，也要先落库。
			if res != nil {
				r.persistQuota(acc, res)
			}
			if res != nil && res.Usage != nil && res.Usage.Limit > 0 {
				remaining := res.Usage.Limit - res.Usage.Usage - res.Usage.Overage
				if remaining < 0 || res.QuotaExhausted {
					remaining = 0
				}
				pr.OK = true
				pr.Limit = res.Usage.Limit
				pr.Remaining = remaining
			} else if res != nil && res.Error != "" {
				pr.Error = res.Error
			} else {
				pr.Error = "no usage returned"
			}
			mu.Lock()
			out = append(out, pr)
			mu.Unlock()
		}(acc)
	}
	wg.Wait()
	return out
}
