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
	shadow   shadowProbe
}

func New(s *store.Store) *Router {
	return &Router{Pool: pool.New(s), Provider: provider.New(), Store: s, shadow: shadowProbe{inflight: map[string]int{}}}
}

// shadowProbe 影子缓存探针：只度量、不改变任何返回值。
// 持久信号（重复命中率）落在 store.cache_probe；并发撞车数（single-flight 潜在收益）
// 是运行时量，内存计数即可——重启归零、多实例各计各的（会低估，见 README）。
type shadowProbe struct {
	mu         sync.Mutex
	inflight   map[string]int
	collisions int64
}

// cacheProbeEnabled 读持久化设置（默认关）。探针是「度量窗口」工具而非常开设施：
// 每个可缓存请求写一行 cache_probe，长期常开会让表无界增长，故 opt-in。
func (r *Router) cacheProbeEnabled() bool {
	v, _ := r.Store.GetSetting("cache_probe_enabled")
	on, _ := strconv.ParseBool(v)
	return on
}

// probe 在请求入口记录一次探针，返回出口回调（用于并发在途计数递减）。
// 非可缓存请求或探针关闭时为空操作。
func (r *Router) probe(req *provider.ChatRequest) func() {
	if !r.cacheProbeEnabled() || !provider.IsCacheable(req) {
		return func() {}
	}
	key := provider.CacheKey(req)
	_ = r.Store.RecordCacheProbe(key)
	r.shadow.mu.Lock()
	r.shadow.inflight[key]++
	if r.shadow.inflight[key] > 1 {
		r.shadow.collisions++ // 同一指纹并发在途 = single-flight 本可省一次上游调用
	}
	r.shadow.mu.Unlock()
	return func() {
		r.shadow.mu.Lock()
		if r.shadow.inflight[key]--; r.shadow.inflight[key] <= 0 {
			delete(r.shadow.inflight, key)
		}
		r.shadow.mu.Unlock()
	}
}

// CacheProbeStats 汇总探针数据供只读端点展示。
func (r *Router) CacheProbeStats() map[string]interface{} {
	distinct, repeats, _ := r.Store.CacheProbeStats()
	total := distinct + repeats
	var rate float64
	if total > 0 {
		rate = float64(repeats) / float64(total)
	}
	r.shadow.mu.Lock()
	collisions := r.shadow.collisions
	r.shadow.mu.Unlock()
	return map[string]interface{}{
		"enabled":           r.cacheProbeEnabled(),
		"cacheableRequests": total,
		"distinctRequests":  distinct,
		"potentialHits":     repeats,
		"potentialHitRate":  rate,
		"singleflightSaved": collisions,
	}
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
	defer r.probe(req)()
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
		if res.RequestRejected {
			// 请求内容被拒(坏请求、工具名冲突等)——账号本身可用,不标记、不换号重试,直接返回。
			provider.Trace(ctx, "router.request_rejected", map[string]interface{}{"account_id": acc.ID, "error": res.Error})
			r.alertRequestRejected(acc, res)
			return nil, nil, &RouteError{Message: res.Error}
		}
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
	defer r.probe(req)()
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
		if res.RequestRejected {
			// 请求内容被拒——账号可用,不标记、不换号,直接返回。
			provider.Trace(ctx, "router.request_rejected", map[string]interface{}{"account_id": acc.ID, "error": res.Error, "stream": true})
			r.alertRequestRejected(acc, res)
			return nil, nil, &RouteError{Message: res.Error}
		}
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

// alertRequestRejected 在请求被网关拒绝且带有排查上下文（如 Cloudflare 403 的 Ray ID、
// 出站 body 大小、响应体片段）时写入一条告警，展示到仪表盘的告警面板，方便定位 403 诱因。
// 无诊断详情（普通坏请求/工具名冲突等）时不打扰。按账号去重，避免同号连续 403 刷屏。
func (r *Router) alertRequestRejected(acc *store.Account, res *provider.Result) {
	if res == nil || res.RejectionDetail == "" {
		return
	}
	title := "请求被网关拒绝: " + acc.Email
	msg := res.Error + "\n" + res.RejectionDetail
	_ = r.Store.CreateAlert("warning", title, msg, "account", &acc.ID, "gateway_rejected")
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
