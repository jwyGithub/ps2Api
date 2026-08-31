package router

import (
	"strconv"
	"time"

	"ps2api/internal/pool"
)

// cacheProbeEnabled 读持久化设置（默认关）。探针是「度量窗口」工具而非常开设施：
// 每个可缓存请求写一行 cache_probe，长期常开会让表无界增长，故 opt-in。
func (r *Router) cacheProbeEnabled() bool {
	v, _ := r.Store.GetSetting("cache_probe_enabled")
	on, _ := strconv.ParseBool(v)
	return on
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
// 才降级为跨账号 failover(接受会话降级)。默认 2。此级消耗普通重试预算(retry_count)。
func (r *Router) stickyEgressBudget() int {
	v, _ := r.Store.GetSetting("sticky_egress_retries")
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 2
	}
	return n
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

// preferQuotaMode 读取「403 换号选号策略」设置 prefer_quota_on_403，映射为 pool.QuotaMode：
//   - "off"      → RoundRobin：关闭额度优先，403 换号也走普通轮询；
//   - "absolute" → Absolute：按剩余额度绝对值最高优先；
//   - 其他/""/"ratio"（默认）→ Ratio：按剩余额度比例最高优先。
// 仅影响 403 网关 failover 换号那一步；普通轮询选号与会话粘性不受此开关影响。
func (r *Router) preferQuotaMode() pool.QuotaMode {
	v, _ := r.Store.GetSetting("prefer_quota_on_403")
	switch v {
	case "off":
		return pool.QuotaModeRoundRobin
	case "absolute":
		return pool.QuotaModeAbsolute
	default:
		return pool.QuotaModeRatio
	}
}

// reservationTTL 读取账号「软预留」窗口时长（设置 account_reservation_seconds，默认 90s）。
// 每次成功交付后据此把账号标记为「最近被占用」，普通轮询选号在窗口内优先避开它，把不同客户端
// 摊到不同账号，避免多客户端挤同一号导致额度被快速烧穿、频繁换号。设为 0（或负）即关闭该机制
// （Reserve 收到 d<=0 直接不预留）。空值/非法回退默认 90s。
func (r *Router) reservationTTL() time.Duration {
	v, _ := r.Store.GetSetting("account_reservation_seconds")
	if v == "" {
		return 90 * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 90 * time.Second
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
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
