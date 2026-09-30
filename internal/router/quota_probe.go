package router

import (
	"context"
	"sync"
	"time"

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
//
// 主路径 = billing operations 零消耗直查（fetchBillingOpsResult，2026-09-29 新增）：
// 直查失败（user_id 缺失/bifrost 不可达/解析失败等）才回退 ProbeQuota 烧 token 探测，
// 两条路径产出的 Result.Usage 形状一致（persistQuota/applyUsageState 无感）。
func (r *Router) probeAccountQuota(ctx context.Context, acc *store.Account, withDetail bool) ProbeResult {
	pr := ProbeResult{AccountID: acc.ID, Email: acc.Email}
	var res *provider.Result
	if r.fetchBillingOpsResult(ctx, acc, pr) {
		// 直查成功：重读账号再判定（写库后 acc 内存字段已过期，applyUsageState/Detail 需要新值）。
		if fresh, err := r.Store.GetAccount(acc.ID); err == nil && fresh != nil {
			acc = fresh
		}
		pr.OK = true
		pr.Limit = acc.QuotaLimit
		pr.Remaining = acc.QuotaRemaining
		if withDetail {
			// Detail 携带直查来源的轻量 Result（Usage 已落库，Error 为空）。
			pr.Detail = billingOpsResultForDetail(acc)
		}
		return pr
	}
	res = r.Provider.ProbeQuota(ctx, acc)
	// 限流头可能在没有 usage 对象的响应中返回，也要先落库。
	if res != nil {
		r.persistQuota(acc, res)
		// session 失效（guest_unusable / Jwt is missing / 401 等）：账号离线并停用，
		// 刷新额度页能直观看到哪些号 session 已死。探测通过时 applyUsageState 会自动恢复。
		if res.AuthFailed {
			r.markOffline(acc, "session 失效: "+res.Error)
		}
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

// markOffline 将 session 失效的账号标记为离线（status=offline）并停用（enabled=false），
// 从选号池（ActiveAccounts 只取 active+enabled）与会话粘性（usableForSticky 要求 enabled）
// 中摘除：session 已死的号（guest_unusable / Jwt is missing / sessions returned 401 等）
// 重试、换出口都不会成功，继续选中只会反复失败。恢复路径：单账号「刷新额度」探测通过
// （usageState=AVAILABLE）时 applyUsageState 自动转回 active 并重新启用。
func (r *Router) markOffline(acc *store.Account, msg string) {
	if acc.Status != "offline" {
		_ = r.Store.SetAccountStatus(acc.ID, "offline", msg)
	}
	if acc.Enabled {
		_ = r.Store.SetAccountEnabled(acc.ID, false)
	}
}

// MarkAccountOffline 是 markOffline 的导出包装：供 toolsets 透传路径
// （internal/api/toolsets.go）把 session 失效的号从池中摘除——与主路由 AuthFailed 同口径。
func (r *Router) MarkAccountOffline(acc *store.Account, msg string) {
	r.markOffline(acc, msg)
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
	case "EXCEEDED":
		// EXCEEDED = 额度耗尽：标 exhausted（不停用，等周期重置后探测恢复），与 AVAILABLE 分支
		// 里「余量算到 0」的口径一致。此前是状态盲区：账号带着 error（如聊天重试耗尽被 MarkError）
		// 时刷新报 EXCEEDED 也无法翻成 exhausted，面板一直显示异常。
		if acc.Status != "exhausted" {
			_ = r.Store.SetAccountStatus(acc.ID, "exhausted", "Postman AI quota exceeded (usageState=EXCEEDED)")
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

// fetchBillingOpsResult 用 billing operations 零消耗直查额度并落库。
// 返回 true 表示成功落库（调用方直接从库里取数即可）；false = 直查不可用，调用方回退烧 token 探测。
//
// 落库口径与 persistQuota 一致：plan=operations.plan（sync-free-202603 等，对齐 usage.userType
// 的计划语义）、state 按 remaining>0 推 AVAILABLE（billing 无 usageState 字段，余量即真相），
// 周期字段 billing 不返回、保持库中现值不动（QuotaSnapshot 零值周期字段不覆盖——SetQuotaSnapshot
// 全字段 UPDATE，故此处仅在成功拿到 AI 条目时调用，且周期传 nil 会清掉库里的周期——所以
// 直查路径不传周期：先读库保留原值）。
func (r *Router) fetchBillingOpsResult(ctx context.Context, acc *store.Account, pr ProbeResult) bool {
	// user_id 是硬前置：桌面注册产线落库的账号有；缺了直查必 403，直接走回退省一次往返。
	// （Account.Tokens 是 JSON blob 字符串，user_id 在里面——GetTokens 负责解析与校验。）
	ops, err := r.Provider.FetchBillingOps(ctx, acc, 0)
	if err != nil || ops == nil || ops.AI == nil || ops.AI.Limit <= 0 {
		return false
	}
	ai := ops.AI
	remaining := ai.Limit - ai.Usage - ai.Overage
	if remaining < 0 {
		remaining = 0
	}
	// state：直查无 usageState，按余量推——>0 视为 AVAILABLE，==0 视为 EXCEEDED 的等价语义。
	// exhausted 判定与 resQuotaExhausted 对齐（remaining==0）。
	state := "AVAILABLE"
	if remaining == 0 {
		state = "EXCEEDED"
	}
	// 直查路径同步账号健康状态（与烧 token 探测的 applyUsageState 同口径，此前从不写 status，
	// error/exhausted 账号刷新后状态原地不动）：余量耗尽标 exhausted（不停用）；余量恢复则翻回
	// active 并启用——刷新是运维人工核对动作，且 RefreshDueQuotas 靠它在周期重置后复活账号。
	// BLOCKED 直查探测不到（无 usageState），维持现状不动。
	if remaining == 0 {
		if acc.Status != "exhausted" {
			_ = r.Store.SetAccountStatus(acc.ID, "exhausted", "Postman AI quota exhausted (billing ops remaining=0)")
		}
	} else if acc.Status != "active" {
		_ = r.Store.SetAccountStatus(acc.ID, "active", "")
	}
	if !acc.Enabled && remaining > 0 {
		_ = r.Store.SetAccountEnabled(acc.ID, true)
	}
	// 周期字段保留库中现值：读一次当前账号快照（acc 上的值可能过期，直接查库）。
	fresh, err := r.Store.GetAccount(acc.ID)
	var cycleStart, cycleEnd *time.Time
	if err == nil && fresh != nil {
		cycleStart, cycleEnd = fresh.QuotaCycleStart, fresh.QuotaCycleEnd
	}
	pooled := ai.IsPooled == "1" || ai.IsPooled == "true"
	_ = r.Store.SetQuotaSnapshot(acc.ID, store.QuotaSnapshot{
		Plan: ai.Plan, State: state, Limit: ai.Limit, Used: ai.Usage,
		Remaining: remaining, Overage: ai.Overage, Spillage: ai.Spillage,
		AllowOverage: ai.Overage > 0, TeamPooled: pooled,
		CycleStart: cycleStart, CycleEnd: cycleEnd,
	})
	return true
}

// billingOpsResultForDetail 构造直查路径的 Detail Result（Usage 从库中快照回填，
// 形状与烧 token 探测的 Result 一致，前端「刷新额度」详情无感）。
func billingOpsResultForDetail(acc *store.Account) *provider.Result {
	res := &provider.Result{Success: true}
	if acc.QuotaLimit > 0 {
		res.Usage = &provider.Usage{
			Limit: acc.QuotaLimit, Usage: acc.QuotaUsed, Overage: acc.QuotaOverage,
			Spillage: acc.QuotaSpillage, UserType: acc.Plan, UsageState: acc.QuotaState,
		}
	}
	return res
}
