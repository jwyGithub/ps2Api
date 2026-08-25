package router

import (
	"context"
	"time"

	"ps2api/internal/pool"
	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// attemptPlan captures the few points where the non-streaming (Chat) and the
// streaming (Stream) retry loops genuinely differ. Everything else — account
// selection, egress rotation, quota persistence, logging, and the whole
// gateway-block two-tier failover — is shared verbatim inside runAttempts.
type attemptPlan struct {
	// stream decorates trace payloads with {"stream": true}; when false the
	// account-bearing traces instead carry {"email": ...}, matching the
	// original Chat/Stream logging one field at a time.
	stream bool

	// invoke performs the actual provider call for the selected account
	// (Provider.Chat vs Provider.StreamChat with a tracked emit callback).
	invoke func(acc *store.Account) *provider.Result

	// emitted reports whether any stream output has already been flushed. Once
	// true a retry would duplicate already-sent deltas, so the loop must fail
	// hard instead of retrying. Always false for Chat, which never streams.
	emitted func() bool
}

// trace decorates a trace payload with the stream/email discriminator exactly
// as the original loops did: streaming logs get {"stream": true}; non-streaming
// logs get {"email": ...} on the calls that previously carried it (withEmail).
func (p attemptPlan) trace(m map[string]interface{}, acc *store.Account, withEmail bool) map[string]interface{} {
	if p.stream {
		m["stream"] = true
	} else if withEmail {
		m["email"] = acc.Email
	}
	return m
}

// runAttempts is the shared retry/failover driver behind Chat and Stream. The
// two callers differ only through the supplied attemptPlan; the control flow,
// budgets, and gateway two-tier failover below are identical to the previous
// hand-duplicated versions and are covered by router_test.go.
func (r *Router) runAttempts(ctx context.Context, req *provider.ChatRequest, plan attemptPlan) (*provider.Result, *store.Account, error) {
	var last string
	excluded := map[int64]bool{}
	gatewayBlocks := 0
	stickyEgressTries := 0                 // 续聊 403 已用掉的「钉账号换出口」次数
	stickyBudget := r.stickyEgressBudget() // 该级预算，耗尽后降级为跨账号 failover
	var pinnedAcc *store.Account           // 续聊遇网关拦截时钉住原账号，绝不换号（换号会丢服务端会话上下文）
	attempts := r.retryCount()
	gwBudget := r.gatewayFailoverBudget()
	if !r.failoverEnabled() {
		attempts = 1
		gwBudget = 1
	}
	// maxAttempts 是循环硬上限：普通失败重试(超时/5xx/限流/额度/钉账号换出口)占用 attempts 额度；
	// 每次「网关拦截跨账号 failover」会把上限 +1 作为补偿，使网关换号不吞掉普通重试预算——
	// 两类预算解耦后，即便 retry_count 很小，被网关拦时仍能遍历大号池逐个兜底(见 gatewayFailoverBudget)。
	maxAttempts := attempts
	// egressSeq 是「当前账号」的出口序号，与全局 attempt 解耦：同账号重试时递增以轮换代理
	// 出口 IP；一旦跨账号 failover 换号就归零，让新账号从自身粘性出口重新走代理池——绝不因
	// 全局重试数堆高而越过所有出口(egressAttempt>=N)回退本机直连(换号往往因 403，直连必再被拦)。
	egressSeq := 0
	var prevAcc int64

	// abort returns a hard-fail result once stream output has begun: retrying
	// would duplicate already-flushed deltas. It is a no-op for Chat, whose
	// emitted() is always false.
	abort := func() (error, bool) {
		if plan.emitted() {
			return &RouteError{Message: "Stream failed after output started: " + last}, true
		}
		return nil, false
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 每次重试前退避 100*2^attempt ms，缓解瞬时错误(超时/5xx/限流)的雪崩式立即重试。
		// 流式下同样安全：首个 delta 之前不落 200、不发事件，退避不会影响已开流的连接。
		if attempt > 0 {
			time.Sleep(time.Duration(100*(1<<attempt)) * time.Millisecond)
		}
		// 已因 403 发生过跨账号 failover（gatewayBlocks>0）后，换号优先切到剩余频率额度最满
		// （理想 30/30）的账号——被 Cloudflare 风控拦截后，最新鲜/余量最满的号更不易再次被拦。
		acc, poolUsed, err := r.selectAccount(pinnedAcc, excluded, req.Messages, gatewayBlocks > 0)
		if err != nil {
			provider.Trace(ctx, "router.error", map[string]interface{}{"attempt": attempt + 1, "error": err.Error()})
			// 因网关拦截逐个排除账号后耗尽了可用账号：返回明确的网关拦截错误(而非笼统的
			// "无可用账号")，让调用方知道根因是上游 Cloudflare 风控、且本次未产生任何输出。
			if gatewayBlocks > 0 {
				return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
			}
			return nil, nil, err
		}
		provider.Trace(ctx, "router.attempt", plan.trace(map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "model": req.Model}, acc, true))
		egressSeq = nextEgressSeq(egressSeq, prevAcc, acc.ID, attempt == 0)
		prevAcc = acc.ID
		if req.GatewayRetry && !req.GatewayRetryRotateEgress {
			// 旧式降级重试：保持同一出口，让新签发的 Cloudflare cookie 绑定同一客户端路径。
			req.EgressAttempt = egressSeq - 1
		} else {
			// 常规请求 / 续聊「钉账号换出口」重试：出口序号按账号内序列递增，
			// 经 (stickyBase+EgressAttempt)%N 切到下一个出口 IP。
			req.EgressAttempt = egressSeq
		}
		started := time.Now()
		res := plan.invoke(acc)
		req.GatewayRetry = false
		req.GatewayRetryRotateEgress = false
		if poolUsed {
			r.Pool.Done(acc.ID)
		}
		r.persistQuota(acc, res)
		r.logAttempt(acc, req.Model, res, started, req.Endpoint)
		if res.Success {
			provider.Trace(ctx, "router.success", plan.trace(map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID}, acc, true))
			r.Pool.MarkUsed(acc.ID)
			return res, acc, nil
		}
		last = res.Error
		provider.Trace(ctx, "router.failure", plan.trace(map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "error": res.Error}, acc, true))
		// 客户端已断开（或请求被取消）：已经没有接收方，任何重试都无法把结果交付出去。继续换号
		// 只会在别的账号上白建一个 Postman 会话、消耗其额度，并把这个与账号无关的失败逐个记成
		// 账号异常；最后还会用 "Client disconnected" 覆盖掉真正的首因错误。立即返回。
		if ctx.Err() != nil || res.Error == provider.ErrClientDisconnected {
			provider.Trace(ctx, "router.client_gone", plan.trace(map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "error": last}, acc, false))
			return nil, nil, &RouteError{Message: last}
		}
		if res.GatewayBlocked {
			// 诱因是有状态的 Cloudflare 风控（WAF/Bot 评分/速率），退避后重试常能成功。
			// 流式下：延迟开流（首个 delta 前不落 200 / 不发事件）保证网关 403 时 emitted 仍为 false，故可重试；
			// 若已吐出过内容则不能重试（会重复输出），返回错误、由客户端侧发起「继续」。
			provider.Trace(ctx, "router.gateway_blocked", plan.trace(map[string]interface{}{"account_id": acc.ID, "error": res.Error}, acc, true))
			r.alertRequestRejected(acc, res)
			if e, done := abort(); done {
				return nil, nil, e
			}
			if provider.HasReusableHistory(req.Messages) {
				// 续聊 403 两级升级处理：
				// 【一级】钉住原账号（保住 Postman 服务端会话：conversationId/TOOL_RESPONSE 不降级），
				//   但轮换出口 IP——403 是有状态的出口信誉风控，换 IP 大概率通过。此级零上下文损失：
				//   不改写 req.Messages（保住会话指纹），只在出站边界净化摘要并压缩 third-party schema。
				if stickyEgressTries < stickyBudget && attempt < maxAttempts-1 {
					pinnedAcc = acc
					stickyEgressTries++
					req.GatewayRetry = true
					req.GatewayRetryRotateEgress = true
					provider.Trace(ctx, "router.gateway_sticky_retry", plan.trace(map[string]interface{}{"account_id": acc.ID, "egress_try": stickyEgressTries, "rotate_egress": true}, acc, false))
					time.Sleep(gatewayBackoff(attempt))
					continue
				}
				// 【二级】同账号轮换出口仍被拦 → 降级为跨账号 failover。会丢服务端会话上下文
				//   （降级为 USER_QUERY + 历史截断「失忆」），但拿到降级答案好过硬失败 403。
				gatewayBlocks++
				excluded[acc.ID] = true
				pinnedAcc = nil
				r.Pool.MarkGatewayBlocked(acc.ID, r.gatewayCooldownDur())
				if gatewayBlocks < gwBudget {
					maxAttempts++
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
			if gatewayBlocks < gwBudget {
				maxAttempts++
				time.Sleep(gatewayBackoff(attempt))
				continue
			}
			return nil, nil, &RouteError{Message: gatewayBlockedError(gatewayBlocks), GatewayBlocked: true}
		}
		if res.RequestRejected {
			// 请求内容被拒(坏请求、工具名冲突等)——账号本身可用,不标记、不换号重试,直接返回。
			provider.Trace(ctx, "router.request_rejected", plan.trace(map[string]interface{}{"account_id": acc.ID, "error": res.Error}, acc, false))
			r.alertRequestRejected(acc, res)
			return nil, nil, &RouteError{Message: res.Error}
		}
		if res.QuotaExhausted {
			excluded[acc.ID] = true
			r.Pool.MarkExhausted(acc.ID)
			if e, done := abort(); done {
				return nil, nil, e
			}
			continue
		}
		if res.RateLimited || res.AuthFailed || pool.IsTransient(res.Error) {
			r.Pool.MarkTransient(acc.ID, res.Error)
			if e, done := abort(); done {
				return nil, nil, e
			}
			continue
		}
		if e, done := abort(); done {
			return nil, nil, e
		}
		// 续聊（Postman 服务端已有会话）遇到「与账号无关」的失败：钉住原账号原地重试，绝不换号。
		// 换号会让服务端 conversationId 失效，请求被降级为 USER_QUERY 且历史截断到几百字节（失忆），
		// 之后必然再次失败、并把同一个错误逐个传染给后面的账号——这正是「一次上游抖动毁一批号、
		// 同时交付一个丢了上下文的答案」的根因。额度耗尽/限流/认证失败是账号自身问题，已在上面
		// 各自的分支里换号，走不到这里。
		if provider.HasReusableHistory(req.Messages) {
			pinnedAcc = acc
		}
		if res.UpstreamFailure {
			// 上游自己调模型失败（Policy Error 等）：账号是健康的。只记录错误文案，绝不 MarkError——
			// 那会把账号写成 status=error，既踢出 ActiveAccounts 又打断会话粘性（见 usableForSticky）。
			provider.Trace(ctx, "router.upstream_failure", plan.trace(map[string]interface{}{"attempt": attempt + 1, "account_id": acc.ID, "error": res.Error}, acc, false))
			r.Pool.MarkTransient(acc.ID, res.Error)
			continue
		}
		// 未分类错误：标记账号后继续兜底。此处 abort() 已前置守卫，走到这里
		// emitted 必为 false（流式尚未吐出任何 delta），故重试绝不会重复输出。
		r.Pool.MarkError(acc.ID, res.Error)
	}
	return nil, nil, &RouteError{Message: "All accounts failed. Last error: " + last}
}
