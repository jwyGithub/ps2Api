package router

import (
	"context"
	"sync"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// ProbeResult 单个账号额度探测的结果。
type ProbeResult struct {
	AccountID int64             `json:"accountId"`
	Email     string            `json:"email"`
	OK        bool              `json:"ok"`
	Limit     float64           `json:"limit"`
	Remaining float64           `json:"remaining"`
	Error     string            `json:"error,omitempty"`
	// Detail 仅在单账号探测时填充，携带上游返回的完整原始结果（内容、usage、错误状态等），
	// 供「刷新额度」按钮的调用方展示完整响应现场；批量探测时为 nil 以节省内存。
	Detail    *provider.Result  `json:"detail,omitempty"`
}

// probeConcurrency 额度探测的并发数。
const probeConcurrency = 3

// ProbeQuotas 增量刷新：仅对「从未成功采集过额度」的启用账号发起一次轻量探测调用，
// 拿到真实额度写库并返回逐账号结果。单次探测仅消耗几 token；额度管理页「刷新额度」
// 按钮调用的就是这个。判定「未采集过」的依据是 QuotaLimit <= 0——新导入或此前探测
// 失败(没拿到 usage)的账号 QuotaLimit 仍为 0，会被补齐；已有额度快照(QuotaLimit>0)
// 的账号直接跳过，避免重复消耗。同时跳过禁用 / 已耗尽（exhausted）账号——探测拿不到有效数据。
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
		// 增量：已采集过额度（QuotaLimit>0）的账号跳过，只补从未成功采集的。
		if acc.QuotaLimit > 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			pr := r.probeAccountQuota(ctx, acc, false)
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
			pr := r.probeAccountQuota(ctx, acc, false)
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
// withDetail 为 true 时将上游原始结果填入 ProbeResult.Detail，供单账号接口透传给前端。
func (r *Router) probeAccountQuota(ctx context.Context, acc *store.Account, withDetail bool) ProbeResult {
	pr := ProbeResult{AccountID: acc.ID, Email: acc.Email}
	res := r.Provider.ProbeQuota(ctx, acc)
	// 限流头可能在没有 usage 对象的响应中返回，也要先落库。
	if res != nil {
		r.persistQuota(acc, res)
		// 依据网关返回的 usageState 同步账号健康：BLOCKED 视为账号异常（error）并停用，
		// AVAILABLE 视为恢复正常并启用。其它状态（如无 usage）不动账号。
		r.applyUsageState(acc, res)
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
	if withDetail {
		pr.Detail = res
	}
	return pr
}

// ProbeAccountQuota 对指定 ID 的单个账号发起一次额度探测并写库，
// 供号池页每行「刷新额度」按钮调用。返回结果中携带完整的上游原始响应（Detail 字段）。
// 账号不存在时返回错误。
func (r *Router) ProbeAccountQuota(ctx context.Context, id int64) (ProbeResult, error) {
	acc, err := r.Store.GetAccount(id)
	if err != nil {
		return ProbeResult{}, err
	}
	return r.probeAccountQuota(ctx, acc, true), nil
}

// applyUsageState 依据上游网关返回的 usage.usageState 同步账号的健康状态与启用开关：
//   - BLOCKED：账号被网关封锁，属账号异常（而非单纯额度用尽），故标记为 error 并停用
//     （enabled=false），从选号池中摘除；待再次探测到 AVAILABLE 时自动恢复。注意这与「额度
//     耗尽」是两回事——额度是否耗尽只看余量（remaining==0），由 persistQuota/QuotaExhausted
//     分支据实际用量判定，不因 usageState 字面值而混淆。
//   - AVAILABLE：额度可用，账号恢复正常（status=active，清空错误信息）并启用（enabled=true），
//     供此前被停用的账号在再次探测通过后自动恢复。
//
// 其它状态或无 usage 时不改动账号，避免误判。仅在 usageState 明确变化时才写库。
func (r *Router) applyUsageState(acc *store.Account, res *provider.Result) {
	if res == nil || res.Usage == nil {
		return
	}
	switch res.Usage.UsageState {
	case "BLOCKED":
		// BLOCKED = 账号被网关封锁的异常状态，须停用（不是额度用尽）：标记 error 并 disable，
		// 从选号池摘除；额度是否耗尽由余量单独判定，二者互不干扰。
		if acc.Status != "error" {
			_ = r.Store.SetAccountStatus(acc.ID, "error", "usage state BLOCKED: account blocked by gateway")
		}
		if acc.Enabled {
			_ = r.Store.SetAccountEnabled(acc.ID, false)
		}
	case "AVAILABLE":
		// 状态同步：usageState 报 AVAILABLE 但真实余量已耗尽（remaining<=0）时，不能恢复成 active——
		// 那样会让「余量为 0 但 status=active」的空号被会话粘性/号池当成健康号反复交付。把余量烧到 0
		// 的那次响应，上游往往仍报 AVAILABLE（EXCEEDED 信号滞后），此处据真实余量把 status 翻成
		// exhausted，让「额度耗尽」这一事实对粘性判据(usableForSticky)与号池(quotaExhausted)一致可见。
		// 额度耗尽不停用（enabled 保持），待额度周期重置后经探测报 AVAILABLE 且余量恢复时再转回 active。
		if resQuotaExhausted(res) {
			if acc.Status != "exhausted" {
				_ = r.Store.SetAccountStatus(acc.ID, "exhausted", "Postman AI quota exhausted (remaining=0)")
			}
		} else if acc.Status != "active" {
			_ = r.Store.SetAccountStatus(acc.ID, "active", "")
		}
		if !acc.Enabled {
			_ = r.Store.SetAccountEnabled(acc.ID, true)
		}
	}
}

// resQuotaExhausted 依据上游 usage 判断该响应是否表明账号 AI 额度已耗尽：额度上限已知(Limit>0)
// 且算出的余量<=0，或上游已明确置 QuotaExhausted。与 persistQuota / probeAccountQuota 里
// 「remaining<0 || QuotaExhausted 时归 0」的口径一致，也与 pool.quotaExhausted 的余量规则对齐。
func resQuotaExhausted(res *provider.Result) bool {
	if res == nil || res.Usage == nil || res.Usage.Limit <= 0 {
		return false
	}
	remaining := res.Usage.Limit - res.Usage.Usage - res.Usage.Overage
	return remaining <= 0 || res.QuotaExhausted
}
