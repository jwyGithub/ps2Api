package router

import (
	"ps2api/internal/pool"
	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// pickAccount 优先返回该会话粘性绑定的账号（续聊固定回首次使用的账号，
// 避免池子轮询换号导致 Postman 会话上下文丢失）；无会话、粘性账号失效或被
// 排除时回退到号池轮询。返回值 poolUsed 表示该账号来自 Pool（需要 Done）。
// preferQuota 为真时，走号池轮询回退的那一步改用「剩余额度优先」选号（Pool.NextByRateRemaining）——
// 用于 403 网关 failover 换号时优先切到剩余频率额度最满的账号。会话粘性命中仍优先，与选号策略无关。
func (r *Router) pickAccount(excluded map[int64]bool, messages []provider.ChatMessage, preferQuota bool) (*store.Account, bool, error) {
	if accID, ok := r.Provider.StickyAccount(messages); ok && !excluded[accID] {
		if acc, err := r.Store.GetAccount(accID); err == nil && acc.Status == "active" && acc.Enabled {
			return acc, false, nil
		}
	}
	// preferQuota 仅表示「本次是 403 换号」；具体是否/如何按额度优先由 prefer_quota_on_403 开关决定。
	// 开关为 off(RoundRobin) 时即便 403 换号也退回普通轮询，与关闭该策略等价。
	if preferQuota {
		if mode := r.preferQuotaMode(); mode != pool.QuotaModeRoundRobin {
			acc, err := r.Pool.NextByQuota(excluded, mode)
			return acc, true, err
		}
	}
	acc, err := r.Pool.Next(excluded)
	return acc, true, err
}

// selectAccount 在 pickAccount 之上加一层「钉住」：当 pinned 非空且未被排除、账号仍可用时，
// 强制返回它。续聊遇网关(403)拦截时用它把重试牢牢固定在原账号上——换号会丢失 Postman 服务端
// 会话上下文（降级为 USER_QUERY + 历史截断），比单纯依赖会话指纹粘性更可靠（指纹粘性在服务重启
// 后可能失效）。pinned 为空时退回常规的粘性 / 号池轮询选择。
func (r *Router) selectAccount(pinned *store.Account, excluded map[int64]bool, messages []provider.ChatMessage, preferQuota bool) (*store.Account, bool, error) {
	if pinned != nil && !excluded[pinned.ID] {
		if acc, err := r.Store.GetAccount(pinned.ID); err == nil && acc.Status == "active" && acc.Enabled {
			return acc, false, nil
		}
	}
	return r.pickAccount(excluded, messages, preferQuota)
}
