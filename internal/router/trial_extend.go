// trial_extend.go —— 每日 23:00 自动为「可续期」账号开启 Solo Extension Trial。
//
// 背景（2026-10-14 逆向+实测，见 memory/extend-trial-and-new-models）：
//   - 服务端 trial 槽位独立：base（client_solo_7_days_trial）与 extension
//     （client_solo_7_days_extension_trial）各 maxAllowedTrials=1，互不占额度。
//   - extension 无需 base 前置，对任何号可直接开，额度 400k/7 天（与 base 相同）。
//   - 槽位状态零消耗可查：GET /api/trials?client_trial_id=<id> → isAllowed /
//     trialActivationCount / trialStart。
//
// 本任务每晚 23:00 扫启用账号：ext 槽位 isAllowed && 未激活（activationCount==0 且
// 无 trialStart）即开 ext，成功后异步刷新额度落库。槽位天然幂等（激活过就不再开），
// 无需额外去重状态。开过 ext 的号次日照常被 RefreshDueQuotas 处理周期重置。
package router

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// trialExtendConcurrency extension trial 开通的并发数（ws/proxy 有限频，低并发稳）。
const trialExtendConcurrency = 3

// clientSoloExtensionTrial 是 extension 槽位的 client trial ID。
const clientSoloExtensionTrial = "client_solo_7_days_extension_trial"

// clientSoloBaseTrial 是 base 槽位的 client trial ID（用于活跃期检查）。
const clientSoloBaseTrial = "client_solo_7_days_trial"

// StartDailyTrialExtend 每天本地时间 22:00 检查可续期账号并自动开启 extension trial，
// 直到 ctx 取消。刻意排在 23:10 FREE 清理之前：开通后 plan/额度传播有延迟，
// 23 点开、23:10 查可能还读到 FREE 计划旧值而被误清理。
func (r *Router) StartDailyTrialExtend(ctx context.Context) {
	const hour = 22
	for {
		timer := time.NewTimer(time.Until(nextDaily(time.Now(), hour)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			started, skipped, failed := r.ExtendTrialsDue(ctx)
			log.Printf("[trial-extend] 每日 extension trial 检查完成: 开通 %d, 不可续/已激活 %d, 失败 %d", started, skipped, failed)
		}
	}
}

// ExtendTrialsDue 扫描启用账号，为 extension 槽位可开（isAllowed 且未激活）的账号开通
// ext trial 并刷新额度。返回 (开通数, 跳过数, 失败数)。
// 跳过含：查询失败（不区分，保守不开）、isAllowed=false、已激活过、无 user_id 等。
func (r *Router) ExtendTrialsDue(ctx context.Context) (started, skipped, failed int) {
	accounts, err := r.Store.ListAccounts()
	if err != nil {
		return 0, 0, 0
	}
	sem := make(chan struct{}, trialExtendConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *store.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			ok, isSkip := r.extendTrialForAccount(ctx, acc)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				started++
			} else if isSkip {
				skipped++
			} else {
				failed++
			}
		}(acc)
	}
	wg.Wait()
	return started, skipped, failed
}

// extendTrialForAccount 对单账号查 ext 槽位并按需开通。
// 返回 (是否开通, 是否属跳过类)。开通成功后异步刷新额度（与对话后刷新同一路径）。
func (r *Router) extendTrialForAccount(ctx context.Context, acc *store.Account) (started, skipped bool) {
	st, err := r.Provider.FetchTrialStatus(ctx, acc, clientSoloExtensionTrial, 0)
	if err != nil {
		log.Printf("[trial-extend] 账号 %d(%s) 查询 ext 槽位失败: %v", acc.ID, acc.Email, err)
		return false, false
	}
	// 槽位查询是零消耗的：顺手把上游真实激活次数镜像落库（存改动前已开过 ext 的存量号
	// 库里还是 0，每晚对齐一次即与上游一致；值相同则跳过写库）。
	r.syncExtTrialCount(acc.ID, acc.ExtTrialCount, st.TrialActivationCount)
	// 已激活过（activationCount>0 或有 trialStart）或明确不允许 → 跳过。
	if !st.IsAllowed || st.TrialActivationCount > 0 || st.TrialStart != "" {
		// 存量周期纠正（2026-10-16）：ext 已激活且未过期的号，库里周期可能还是 base
		// 时代的旧值（开通后落库是 10-16 才加的）。每晚本来就查了槽位（零消耗），
		// 顺手把活跃 trial 的周期同步落库。
		r.syncCycleFromTrial(acc.ID, st.TrialStart, st.TrialEnd)
		return false, true
	}
	// 实测坑（2026-10-14，pmdd197ed91a）：base trial 未到期时开 ext，ext 只给到 base
	// 原到期日（1 天）并把 base 截断——正确顺序是 base 用完到期再开 ext 才给满 7 天。
	// 故 base 槽位仍在活跃期内（trialEnd 在未来）时跳过，等下一晚再查。
	base, err := r.Provider.FetchTrialStatus(ctx, acc, clientSoloBaseTrial, 0)
	if err != nil {
		log.Printf("[trial-extend] 账号 %d(%s) 查询 base 槽位失败: %v", acc.ID, acc.Email, err)
		return false, false
	}
	if base.TrialStart != "" && baseTrialActive(base) {
		// base 活跃期内真实周期就是 base trial 周期——同样顺手纠正存量旧值。
		r.syncCycleFromTrial(acc.ID, base.TrialStart, base.TrialEnd)
		return false, true
	}
	res, err := r.Provider.StartTrial(ctx, acc, clientSoloExtensionTrial, 0)
	if err != nil {
		log.Printf("[trial-extend] 账号 %d(%s) 开通 extension trial 失败: %v", acc.ID, acc.Email, err)
		return false, false
	}
	log.Printf("[trial-extend] 账号 %d(%s) extension trial 已开通, 截止 %s", acc.ID, acc.Email, res.TrialPeriodEnd)
	if err := r.Store.SetExtTrialCount(acc.ID, 1); err != nil {
		log.Printf("[trial-extend] 账号 %d 续期次数落库失败: %v", acc.ID, err)
	}
	r.syncCycleFromTrialEnd(acc.ID, res.TrialPeriodEnd)
	// 异步刷新额度：开通后额度传播有 1-2 分钟延迟，延迟拉取一次落库（失败不影响，次日照常兜底）。
	go func(id int64) {
		time.Sleep(2 * time.Minute)
		fresh, err := r.Store.GetAccount(id)
		if err != nil || fresh == nil || !fresh.Enabled {
			return
		}
		if !r.fetchBillingOpsResult(context.Background(), fresh, ProbeResult{AccountID: fresh.ID, Email: fresh.Email}) {
			log.Printf("[trial-extend] 账号 %d(%s) 开通后刷新额度失败（静默）", id, fresh.Email)
		}
	}(acc.ID)
	return true, false
}

// trialTimeLayout 是 trial 接口返回的时间格式（"2006-01-02 15:04:05"，时区为服务端本地）。
const trialTimeLayout = "2006-01-02 15:04:05"

// syncExtTrialCount 把上游 ext 槽位的激活次数镜像落库。仅在次数变化时写（避免每晚重复
// UPDATE 推高 updated_at）；上游查询失败不影响调用方主流程（本函数零 panic、静默失败）。
func (r *Router) syncExtTrialCount(id int64, current, upstream int) {
	if current == upstream {
		return
	}
	if err := r.Store.SetExtTrialCount(id, upstream); err != nil {
		log.Printf("[trial-extend] 账号 %d 续期次数同步失败: %v", id, err)
		return
	}
	log.Printf("[trial-extend] 账号 %d 续期次数已同步: %d → %d", id, current, upstream)
}

// syncCycleFromTrialEnd 开通 trial 后把新周期落库：end 取 trial_period_end，start 取当前
// 时间。quotaCycleStart/End 驱动 RefreshDueQuotas 的周期重置扫描与面板展示——不更新的话
// 库里还是 base trial 的旧周期（已过期），每晚被当作「周期已重置」重复探测，且面板显示的
// 重置时间错误。零额外请求：POST /api/trials 响应自带 trial_period_end。
func (r *Router) syncCycleFromTrialEnd(id int64, trialPeriodEnd string) {
	end, err := time.ParseInLocation(trialTimeLayout, trialPeriodEnd, time.Local)
	if err != nil || end.IsZero() {
		log.Printf("[trial-extend] 账号 %d trial_period_end 解析失败（周期留待下次刷新补正）: %s", id, trialPeriodEnd)
		return
	}
	now := time.Now()
	if err := r.Store.SetQuotaCycle(id, &now, &end); err != nil {
		log.Printf("[trial-extend] 账号 %d 周期落库失败: %v", id, err)
	}
}

// syncCycleFromTrial 存量周期纠正：用已查到的 trial 槽位时间（start/end，零消耗查询的
// 副产物）同步账号周期。仅在 trial 仍在活跃期内（end 在未来）且库中周期与 trial 周期
// 不一致时写库——避免每晚重复写同样的值，也让已过期 trial 的号走 backfill 路径补采
// usageCycle（其真实周期是额度周期而非 trial 周期）。
func (r *Router) syncCycleFromTrial(id int64, trialStart, trialEnd string) {
	if trialStart == "" || trialEnd == "" {
		return
	}
	start, err1 := time.ParseInLocation(trialTimeLayout, trialStart, time.Local)
	end, err2 := time.ParseInLocation(trialTimeLayout, trialEnd, time.Local)
	if err1 != nil || err2 != nil || !end.After(time.Now()) {
		return
	}
	acc, err := r.Store.GetAccount(id)
	if err != nil || acc == nil {
		return
	}
	if acc.QuotaCycleStart != nil && acc.QuotaCycleEnd != nil &&
		acc.QuotaCycleStart.Equal(start) && acc.QuotaCycleEnd.Equal(end) {
		return // 库里已是 trial 周期，无需写
	}
	if err := r.Store.SetQuotaCycle(id, &start, &end); err != nil {
		log.Printf("[trial-extend] 账号 %d 周期纠正失败: %v", id, err)
		return
	}
	log.Printf("[trial-extend] 账号 %d 周期已按活跃 trial 纠正: %s → %s", id, trialStart, trialEnd)
}

// baseTrialActive 判断 base trial 是否仍在活跃期：trialEnd 能解析且在未来（留 1h 缓冲，
// 临期 1 小时内也不开 ext，避免边界上截断 base）。解析失败视为不活跃（开 ext 由服务端
// maxAllowedTrials 兜底，不会重复开通）。
func baseTrialActive(st *provider.TrialStatus) bool {
	end, err := time.Parse(trialTimeLayout, st.TrialEnd)
	if err != nil {
		return false
	}
	return end.After(time.Now().Add(time.Hour))
}

// RefreshAccountQuotaWithTrial 是面板「账号卡片 → 刷新」按钮的组合动作：
// 先按常规探测额度写库（与 ProbeAccountQuota 同路径），随后仅当探测结论为「额度耗尽」时，
// 自动判断该账号能否继续开 extension trial（exhausted 的号重置时间在晚上，白天点了也白点——
// 若 ext 槽位可开则立即续上，省去干等）。返回探测结果 + 试用动作说明。
//
// 判定与开通复用 extendTrialForAccount 的安全顺序（base 活跃期内不开 ext，避免截断），
// 但本方法面向单次手动点击，需要把「为什么没开」的类别带回给前端提示，故单独实现判定，
// 开通动作仍复用同一结论口径：
//   - trial=skipped-base-active：base 仍在活跃期，ext 开了会被截断 → 跳过（前端提示等 base 到期）
//   - trial=skipped-used：ext 槽位已用过（activationCount>0 / trialStart 非空 / !isAllowed）→ 无续期可开
//   - trial=started：已开通 ext，稍后额度刷新（异步延迟拉取落库）
//   - trial=none：额度未耗尽且非 sync-free-*（无需判断）；禁用号也归此类
//   - trial=error：槽位查询/开通失败（上游波动），不影响额度探测结果本身
func (r *Router) RefreshAccountQuotaWithTrial(ctx context.Context, id int64) (ProbeResult, string, error) {
	pr, err := r.ProbeAccountQuota(ctx, id)
	if err != nil {
		return pr, "none", err
	}
	acc, err := r.Store.GetAccount(id)
	if err != nil || acc == nil {
		return pr, "none", err
	}
	// 额度未耗尽时，plan 为 sync-free-* 的账号也要判断试用（2026-10-15 线上实测
	// pmdd197ed91a）：base trial 到期回落 sync-free 后周期重置会补 100 额度，
	// 号不再耗尽 → 按旧口径永远走 none，ext 续期断掉。sync-free-* 两种来源
	// （注册产线纯免费号 / base 到期回落号）开 ext 都是收益（400k/7天），且与
	// 每晚 ExtendTrialsDue（不看耗尽、base 不活跃就开）口径一致，这里对齐。
	// 禁用号不自动开 trial（恢复是人工动作）。
	exhausted := acc.Status == "exhausted" || (pr.OK && pr.Limit > 0 && pr.Remaining <= 0)
	fallbackFree := strings.HasPrefix(acc.Plan, "sync-free-")
	if !exhausted && !fallbackFree {
		return pr, "none", nil
	}
	if !acc.Enabled {
		return pr, "none", nil
	}
	// 额度耗尽 → 查 ext 槽位与 base 活跃期，按 ExtendTrialsDue 同口径决定是否开通。
	action, started := r.evaluateTrialForExhausted(ctx, acc)
	_ = started // started 时异步额度刷新已在 evaluate 内启动；action 供前端提示
	return pr, action, nil
}

// evaluateTrialForExhausted 对耗尽账号执行 ext trial 判定与开通。
// 返回动作类别（skipped-base-active / skipped-used / started / error）与是否开通。
// 开通成功后与 extendTrialForAccount 相同：异步延迟 2 分钟刷新额度落库（传播延迟）。
func (r *Router) evaluateTrialForExhausted(ctx context.Context, acc *store.Account) (string, bool) {
	st, err := r.Provider.FetchTrialStatus(ctx, acc, clientSoloExtensionTrial, 0)
	if err != nil {
		log.Printf("[trial-refresh] 账号 %d(%s) 查询 ext 槽位失败: %v", acc.ID, acc.Email, err)
		return "error", false
	}
	// 与每晚 ExtendTrialsDue 同口径：零消耗查询顺手镜像上游激活次数（存量号对齐）。
	r.syncExtTrialCount(acc.ID, acc.ExtTrialCount, st.TrialActivationCount)
	if !st.IsAllowed || st.TrialActivationCount > 0 || st.TrialStart != "" {
		return "skipped-used", false
	}
	base, err := r.Provider.FetchTrialStatus(ctx, acc, clientSoloBaseTrial, 0)
	if err != nil {
		log.Printf("[trial-refresh] 账号 %d(%s) 查询 base 槽位失败: %v", acc.ID, acc.Email, err)
		return "error", false
	}
	if base.TrialStart != "" && baseTrialActive(base) {
		// base 仍在活跃期：现在开 ext 只会拿到 base 原到期日（被截断）。跳过，等 base 到期。
		return "skipped-base-active", false
	}
	res, err := r.Provider.StartTrial(ctx, acc, clientSoloExtensionTrial, 0)
	if err != nil {
		log.Printf("[trial-refresh] 账号 %d(%s) 开通 extension trial 失败: %v", acc.ID, acc.Email, err)
		return "error", false
	}
	log.Printf("[trial-refresh] 账号 %d(%s) 手动刷新触发 extension trial 开通, 截止 %s", acc.ID, acc.Email, res.TrialPeriodEnd)
	if err := r.Store.SetExtTrialCount(acc.ID, 1); err != nil {
		log.Printf("[trial-refresh] 账号 %d 续期次数落库失败: %v", acc.ID, err)
	}
	r.syncCycleFromTrialEnd(acc.ID, res.TrialPeriodEnd)
	// 开通后额度传播有 1-2 分钟延迟，延迟拉取一次落库（失败静默，晚点定期探测会兜底）。
	go func(id int64) {
		time.Sleep(2 * time.Minute)
		fresh, err := r.Store.GetAccount(id)
		if err != nil || fresh == nil || !fresh.Enabled {
			return
		}
		if !r.fetchBillingOpsResult(context.Background(), fresh, ProbeResult{AccountID: fresh.ID, Email: fresh.Email}) {
			log.Printf("[trial-refresh] 账号 %d(%s) 开通后刷新额度失败（静默）", id, fresh.Email)
		}
	}(acc.ID)
	return "started", true
}
