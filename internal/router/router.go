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

// stickyEgressBudget 是续聊(有可复用历史)遇网关 403 时，「钉住原账号、轮换出口 IP」这一级
// 允许尝试的最大次数。预算内保住 Postman 服务端会话(零上下文损失)只换出口；预算耗尽仍被拦，
// 才降级为跨账号 failover(接受会话降级)。默认 2。注意总重试次数仍受 retry_count 约束——
// 若要让「多出口轮换 + 兜底 failover」都有空间，应把 retry_count 调到 ≥ 该预算 + 1。
func (r *Router) stickyEgressBudget() int {
	v, _ := r.Store.GetSetting("sticky_egress_retries")
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 2
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

// selectAccount 在 pickAccount 之上加一层「钉住」：当 pinned 非空且未被排除、账号仍可用时，
// 强制返回它。续聊遇网关(403)拦截时用它把重试牢牢固定在原账号上——换号会丢失 Postman 服务端
// 会话上下文（降级为 USER_QUERY + 历史截断），比单纯依赖会话指纹粘性更可靠（指纹粘性在服务重启
// 后可能失效）。pinned 为空时退回常规的粘性 / 号池轮询选择。
func (r *Router) selectAccount(pinned *store.Account, excluded map[int64]bool, messages []provider.ChatMessage) (*store.Account, bool, error) {
	if pinned != nil && !excluded[pinned.ID] {
		if acc, err := r.Store.GetAccount(pinned.ID); err == nil && acc.Status == "active" && acc.Enabled {
			return acc, false, nil
		}
	}
	return r.pickAccount(excluded, messages)
}

func (r *Router) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.Result, *store.Account, error) {
	defer r.probe(req)()
	var last string
	excluded := map[int64]bool{}
	gatewayBlocks := 0
	stickyEgressTries := 0                 // 续聊 403 已用掉的「钉账号换出口」次数
	stickyBudget := r.stickyEgressBudget() // 该级预算，耗尽后降级为跨账号 failover
	var pinnedAcc *store.Account           // 续聊遇网关拦截时钉住原账号，绝不换号（换号会丢服务端会话上下文）
	attempts := r.retryCount()
	if !r.failoverEnabled() {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(100*(1<<attempt)) * time.Millisecond)
		}
		acc, poolUsed, err := r.selectAccount(pinnedAcc, excluded, req.Messages)
		if err != nil {
			provider.Trace(ctx, "router.error", map[string]interface{}{"attempt": attempt + 1, "error": err.Error()})
			// 因网关拦截逐个排除账号后耗尽了可用账号：返回明确的网关拦截错误(而非笼统的
			// "无可用账号")，让调用方知道根因是上游 Cloudflare 风控、且本次未产生任何输出。
			if gatewayBlocks > 0 {
				return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
			}
			return nil, nil, err
		}
		provider.Trace(ctx, "router.attempt", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "email": acc.Email, "model": req.Model})
		if req.GatewayRetry && !req.GatewayRetryRotateEgress {
			// 旧式降级重试：保持同一出口，让新签发的 Cloudflare cookie 绑定同一客户端路径。
			req.EgressAttempt = attempt - 1
		} else {
			// 常规请求 / 续聊「钉账号换出口」重试：出口序号随 attempt 自然递增，
			// 经 (stickyBase+EgressAttempt)%N 切到下一个出口 IP。
			req.EgressAttempt = attempt
		}
		started := time.Now()
		res := r.Provider.Chat(ctx, acc, req)
		req.GatewayRetry = false
		req.GatewayRetryRotateEgress = false
		if poolUsed {
			r.Pool.Done(acc.ID)
		}
		r.persistQuota(acc, res)
		r.logAttempt(acc, req.Model, res, started, req.Endpoint)
		if res.Success {
			provider.Trace(ctx, "router.success", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "email": acc.Email})
			r.Pool.MarkUsed(acc.ID)
			return res, acc, nil
		}
		last = res.Error
		provider.Trace(ctx, "router.failure", map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "email": acc.Email, "error": res.Error})
		if res.GatewayBlocked {
			provider.Trace(ctx, "router.gateway_blocked", map[string]interface{}{"account_id": acc.ID, "email": acc.Email, "error": res.Error})
			r.alertRequestRejected(acc, res)
			if provider.HasReusableHistory(req.Messages) {
				// 续聊 403 两级升级处理：
				// 【一级】钉住原账号（保住 Postman 服务端会话：conversationId/TOOL_RESPONSE 不降级），
				//   但轮换出口 IP——403 是有状态的出口信誉风控，换 IP 大概率通过。此级零上下文损失：
				//   不改写 req.Messages（保住会话指纹），只在出站边界净化摘要并压缩 third-party schema。
				if stickyEgressTries < stickyBudget && attempt < attempts-1 {
					pinnedAcc = acc
					stickyEgressTries++
					req.GatewayRetry = true
					req.GatewayRetryRotateEgress = true
					provider.Trace(ctx, "router.gateway_sticky_retry", map[string]interface{}{"account_id": acc.ID, "egress_try": stickyEgressTries, "rotate_egress": true})
					time.Sleep(gatewayBackoff(attempt))
					continue
				}
				// 【二级】同账号轮换出口仍被拦 → 降级为跨账号 failover。会丢服务端会话上下文
				//   （降级为 USER_QUERY + 历史截断「失忆」），但拿到降级答案好过硬失败 403。
				gatewayBlocks++
				excluded[acc.ID] = true
				pinnedAcc = nil
				r.Pool.MarkGatewayBlocked(acc.ID, r.gatewayCooldownDur())
				if attempt < attempts-1 {
					time.Sleep(gatewayBackoff(attempt))
					continue
				}
				return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
			}
			// 新对话：无服务端会话可丢，换号 failover 安全——排除当前账号并打冷却标记，退避后换其他账号。
			gatewayBlocks++
			excluded[acc.ID] = true
			pinnedAcc = nil
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
	stickyEgressTries := 0                 // 续聊 403 已用掉的「钉账号换出口」次数
	stickyBudget := r.stickyEgressBudget() // 该级预算，耗尽后降级为跨账号 failover
	var pinnedAcc *store.Account           // 续聊遇网关拦截时钉住原账号，绝不换号（换号会丢服务端会话上下文）
	attempts := r.retryCount()
	if !r.failoverEnabled() {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		acc, poolUsed, err := r.selectAccount(pinnedAcc, excluded, req.Messages)
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
		if req.GatewayRetry && !req.GatewayRetryRotateEgress {
			// 旧式降级重试：保持同一出口。
			req.EgressAttempt = attempt - 1
		} else {
			// 常规请求 / 续聊「钉账号换出口」重试：出口随 attempt 轮换。
			req.EgressAttempt = attempt
		}
		started := time.Now()
		res := r.Provider.StreamChat(ctx, acc, req, trackedEmit)
		req.GatewayRetry = false
		req.GatewayRetryRotateEgress = false
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
			// 诱因是有状态的 Cloudflare 风控（WAF/Bot 评分/速率），退避后重试常能成功；见 Chat 内说明。
			// 延迟开流（首个 delta 前不落 200 / 不发事件）保证网关 403 时 emitted 仍为 false，故可重试；
			// 若已吐出过内容则不能重试（会重复输出），返回错误、由客户端侧发起「继续」。
			provider.Trace(ctx, "router.gateway_blocked", map[string]interface{}{"account_id": acc.ID, "error": res.Error, "stream": true})
			r.alertRequestRejected(acc, res)
			if emitted {
				return nil, nil, &RouteError{Message: "Stream failed after output started: " + last}
			}
			if provider.HasReusableHistory(req.Messages) {
				// 【一级】钉住原账号（保住服务端会话），轮换出口 IP 重试；不改 req.Messages。
				if stickyEgressTries < stickyBudget && attempt < attempts-1 {
					pinnedAcc = acc
					stickyEgressTries++
					req.GatewayRetry = true
					req.GatewayRetryRotateEgress = true
					provider.Trace(ctx, "router.gateway_sticky_retry", map[string]interface{}{"account_id": acc.ID, "egress_try": stickyEgressTries, "rotate_egress": true, "stream": true})
					time.Sleep(gatewayBackoff(attempt))
					continue
				}
				// 【二级】同账号轮换出口仍被拦 → 降级跨账号 failover（接受服务端会话降级）。
				gatewayBlocks++
				excluded[acc.ID] = true
				pinnedAcc = nil
				r.Pool.MarkGatewayBlocked(acc.ID, r.gatewayCooldownDur())
				if attempt < attempts-1 {
					time.Sleep(gatewayBackoff(attempt))
					continue
				}
				return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
			}
			// 新对话：无服务端会话可丢，换号 failover 安全。
			gatewayBlocks++
			excluded[acc.ID] = true
			pinnedAcc = nil
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
			pr := r.probeAccountQuota(ctx, acc)
			mu.Lock()
			out = append(out, pr)
			mu.Unlock()
		}(acc)
	}
	wg.Wait()
	return out
}

// ProbeAccountsByIDs 对给定 ID 集合的账号并发探测额度并写库，返回逐账号结果。
// 供导入后自动刷新额度使用：仅覆盖本次导入的账号子集，比整池 ProbeQuotas 更轻。
// 与 ProbeQuotas 一致，跳过禁用 / 已耗尽（exhausted）账号——探测拿不到有效数据。
func (r *Router) ProbeAccountsByIDs(ctx context.Context, ids []int64) []ProbeResult {
	var out []ProbeResult
	sem := make(chan struct{}, probeConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		acc, err := r.Store.GetAccount(id)
		if err != nil || acc == nil || !acc.Enabled || acc.Status == "exhausted" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			pr := r.probeAccountQuota(ctx, acc)
			mu.Lock()
			out = append(out, pr)
			mu.Unlock()
		}(acc)
	}
	wg.Wait()
	return out
}

// probeAccountQuota 对单个账号执行一次探测并写库，返回逐账号结果。
// ProbeQuotas（批量）与 ProbeAccountQuota（单账号）共用此逻辑。
func (r *Router) probeAccountQuota(ctx context.Context, acc *store.Account) ProbeResult {
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
	return pr
}

// ProbeAccountQuota 对指定 ID 的单个账号发起一次额度探测并写库，
// 供号池页每行「刷新额度」按钮调用。账号不存在时返回错误。
func (r *Router) ProbeAccountQuota(ctx context.Context, id int64) (ProbeResult, error) {
	acc, err := r.Store.GetAccount(id)
	if err != nil {
		return ProbeResult{}, err
	}
	return r.probeAccountQuota(ctx, acc), nil
}
