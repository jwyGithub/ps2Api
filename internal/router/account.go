package router

import (
	"ps2api/internal/pool"
	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// usableForSticky 判断账号能否承接「会话粘性 / 钉账号」的回落：要求账号已启用，且额度
// 未耗尽。续聊的 Postman 服务端会话只存在于这一个账号上，换号会丢上下文（降级为无历史的
// USER_QUERY），故优先固定在原账号上——即便该号上次是普通异常（status=="error"）也宁可
// 原号退避重试，也不要静默换号交付「失忆」答案。唯有额度确定耗尽才放弃粘性：那种号发出去
// 必然拿不到结果，继续钉住只会重试到失败，须回退号池。
//
// 额度耗尽的判定同时看两处，缺一不可：
//   - status=="exhausted"：由 applyUsageState 在收到耗尽信号时写入的显式标记；
//   - QuotaLimit>0 && QuotaRemaining<=0：真实余量。把余量烧到 0 的那一次响应，上游 usageState
//     往往还是 AVAILABLE，status 未必来得及翻成 exhausted；若只看 status，粘性会把这个余量为 0
//     的空号当成健康号反复交付、每次都拿不到结果。这里直连余量兜底（与 pool.quotaExhausted 同规则），
//     余量见底即放弃粘性、回退号池（号池同样会跳过空号），二者保持一致。
func usableForSticky(acc *store.Account, err error) bool {
	return err == nil && acc != nil && acc.Enabled &&
		acc.Status != "exhausted" &&
		!(acc.QuotaLimit > 0 && acc.QuotaRemaining <= 0)
}

// pickAccount 优先返回该会话粘性绑定的账号（续聊固定回首次使用的账号，
// 避免池子轮询换号导致 Postman 会话上下文丢失）；无会话、粘性账号失效或被
// 排除时回退到号池轮询。返回值 poolUsed 表示该账号来自 Pool（需要 Done）。
// preferQuota 为真时，走号池轮询回退的那一步改用「剩余额度优先」选号（Pool.NextByRateRemaining）——
// 用于 403 网关 failover 换号时优先切到剩余频率额度最满的账号。会话粘性命中仍优先，与选号策略无关。
func (r *Router) pickAccount(excluded map[int64]bool, messages []provider.ChatMessage, preferQuota bool) (*store.Account, bool, error) {
	if accID, ok := r.Provider.StickyAccount(messages); ok && !excluded[accID] {
		if acc, err := r.Store.GetAccount(accID); usableForSticky(acc, err) {
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
		if acc, err := r.Store.GetAccount(pinned.ID); usableForSticky(acc, err) {
			return acc, false, nil
		}
	}
	return r.pickAccount(excluded, messages, preferQuota)
}
