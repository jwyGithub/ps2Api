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
	r := &Router{Pool: pool.New(s), Provider: provider.New(), Store: s, shadow: shadowProbe{inflight: map[string]int{}}}
	// 出口代理池：仅当 proxy_enabled=true 且配置了 proxy_urls 时启用，否则返回 nil → 走本机直连。
	// 每次请求实时读设置，面板改动即时生效、无需重启。
	r.Provider.SetProxyList(func() []string {
		if on, _ := r.Store.GetSetting("proxy_enabled"); on != "true" {
			return nil
		}
		v, _ := r.Store.GetSetting("proxy_urls")
		if v == "" {
			return nil
		}
		return []string{v}
	})
	return r
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

// gatewayBackoff 为网关(Cloudflare)风控拦截的重试提供退避,比普通失败重试更长,
// 给速率/评分窗口降温时间:0.5s、1s、2s… 上限 5s。
func gatewayBackoff(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * 500 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

// gatewayCooldownDur 读取被网关拦截账号的冷却时长（默认 5 分钟）。冷却期内号池优先跳过该账号。
func (r *Router) gatewayCooldownDur() time.Duration {
	v, _ := r.Store.GetSetting("gateway_cooldown_seconds")
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(n) * time.Second
}

// maxGatewayCompactTries 限制「压缩后原号重试」的次数，把剩余重试额度留给换号 failover——
// 压缩是首选(对症)修复，但若压到底仍被拦，就该退回换号，故不能吃光所有 attempt。
const maxGatewayCompactTries = 2

// gatewayCompactBudget 读取网关拦截后压缩 tool_result 正文的单块字节预算（默认 8192，下限 1024）。
// 首次压缩用此预算，之后每次减半到下限，逐步收紧。
func (r *Router) gatewayCompactBudget() int {
	v, _ := r.Store.GetSetting("gateway_compact_bytes")
	n, err := strconv.Atoi(v)
	if err != nil || n < gatewayCompactFloor {
		return 8192
	}
	return n
}

// gatewayCompactFloor 是压缩预算的下限：低于它就不再压缩(避免把 tool_result 削到无意义)，
// 转而回退换号 failover。
const gatewayCompactFloor = 1024

// nextCompactBudget 把压缩预算减半，但不低于下限，供逐步收紧的多次压缩重试使用。
func nextCompactBudget(b int) int {
	if b /= 2; b < gatewayCompactFloor {
		b = gatewayCompactFloor
	}
	return b
}

// gatewayBlockedError 生成面向调用方(agent 终端/模型)的明确错误：说明这是上游网关(Cloudflare)
// 风控拦截、已尝试多少个账号、且本次「未产生任何输出」，便于终端干净停止任务、模型安全重做。
func gatewayBlockedError(triedAccounts int) string {
	return "上游网关(Cloudflare)持续拦截：已尝试 " + strconv.Itoa(triedAccounts) +
		" 个账号仍返回 403(风控/Bot 校验)。本次请求已中断，未产生任何输出。可稍后重试，或发送\"继续\"以恢复此前任务。"
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
	gatewayBlocks := 0
	compactBudget := r.gatewayCompactBudget()
	compactTries := 0
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
			// 因网关拦截逐个排除账号后耗尽了可用账号：返回明确的网关拦截错误(而非笼统的
			// "无可用账号")，让调用方知道根因是上游 Cloudflare 风控、且本次未产生任何输出。
			if gatewayBlocks > 0 {
				return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
			}
			return nil, nil, err
		}
		provider.Trace(ctx, "router.attempt", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "model": req.Model})
		req.EgressAttempt = attempt // 遇 403 重试时递增，逐个切换代理出口 IP
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
		if res.GatewayBlocked {
			// 真实诱因是 Cloudflare WAF 托管内容规则命中请求体里累积的类 HTML/JS 文本(巨型
			// tool_result:原始文件转储、网页抓取等),随 tool_use↔tool_result 往返轮次增多而累积
			// ——不是账号身份、也不单纯是字节数。故首选「压缩掉这些巨型 tool_result 正文后原号重试」
			// (对症):既缩体积又剥离触发规则的标记文本。压到下限仍被拦才回退换号 failover(兜底)。
			provider.Trace(ctx, "router.gateway_blocked", map[string]interface{}{"account_id": acc.ID, "error": res.Error})
			r.alertRequestRejected(acc, res)
			if compactTries < maxGatewayCompactTries && attempt < attempts-1 {
				if newMsgs, ok := provider.CompactMessages(req.Messages, compactBudget); ok {
					provider.Trace(ctx, "router.gateway_compact_retry", map[string]interface{}{"account_id": acc.ID, "budget": compactBudget, "try": compactTries + 1})
					req.Messages = newMsgs
					compactBudget = nextCompactBudget(compactBudget)
					compactTries++
					time.Sleep(gatewayBackoff(attempt))
					continue // 原号重试(不排除、不冷却):诱因是请求体而非账号
				}
			}
			// 压缩已到底/无可压 → 回退换号 failover:排除当前账号并打冷却标记,退避后换其他账号。
			gatewayBlocks++
			excluded[acc.ID] = true
			r.Pool.MarkGatewayBlocked(acc.ID, r.gatewayCooldownDur())
			if attempt < attempts-1 {
				time.Sleep(gatewayBackoff(attempt))
				continue
			}
			return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
		}
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
	gatewayBlocks := 0
	compactBudget := r.gatewayCompactBudget()
	compactTries := 0
	attempts := r.retryCount()
	if !r.failoverEnabled() {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		acc, poolUsed, err := r.pickAccount(excluded, req.Messages)
		if err != nil {
			provider.Trace(ctx, "router.error", map[string]interface{}{"attempt": attempt + 1, "error": err.Error()})
			// 因网关拦截逐个排除账号后耗尽了可用账号：返回明确的网关拦截错误(而非笼统的
			// "无可用账号")，让调用方知道根因是上游 Cloudflare 风控、且本次未产生任何输出。
			if gatewayBlocks > 0 {
				return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
			}
			return nil, nil, err
		}
		provider.Trace(ctx, "router.attempt", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "model": req.Model, "stream": true})
		req.EgressAttempt = attempt // 遇 403 重试时递增，逐个切换代理出口 IP
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
		if res.GatewayBlocked {
			// 诱因是请求体里累积的巨型 tool_result(类 HTML/JS 文本触发 Cloudflare WAF 托管规则),
			// 而非账号身份(见 Chat 内说明)。首选压缩 tool_result 正文后原号重试,压到底再回退换号。
			// 但若已吐出过内容则不能重试(会重复输出),此时返回错误、由客户端侧发起「继续」。
			// 注:延迟开流(首个 delta 前不落 200/不发事件)保证网关 403 时 emitted 仍为 false,故压缩/换号重试可行。
			provider.Trace(ctx, "router.gateway_blocked", map[string]interface{}{"account_id": acc.ID, "error": res.Error, "stream": true})
			r.alertRequestRejected(acc, res)
			if emitted {
				return nil, nil, &RouteError{Message: "Stream failed after output started: " + last}
			}
			if compactTries < maxGatewayCompactTries && attempt < attempts-1 {
				if newMsgs, ok := provider.CompactMessages(req.Messages, compactBudget); ok {
					provider.Trace(ctx, "router.gateway_compact_retry", map[string]interface{}{"account_id": acc.ID, "budget": compactBudget, "try": compactTries + 1, "stream": true})
					req.Messages = newMsgs
					compactBudget = nextCompactBudget(compactBudget)
					compactTries++
					time.Sleep(gatewayBackoff(attempt))
					continue // 原号重试(不排除、不冷却):诱因是请求体而非账号
				}
			}
			gatewayBlocks++
			excluded[acc.ID] = true
			r.Pool.MarkGatewayBlocked(acc.ID, r.gatewayCooldownDur())
			if attempt < attempts-1 {
				time.Sleep(gatewayBackoff(attempt))
				continue
			}
			return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
		}
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

type RouteError struct {
	Message string
	// GatewayBlocked 标记该失败源于上游网关(Cloudflare)风控拦截(403)且所有尝试账号均被拦。
	// 供 HTTP 层选择合适的状态码/文案,让 agent 终端能明确「上游拦截、非本地错误」。
	GatewayBlocked bool
}

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
