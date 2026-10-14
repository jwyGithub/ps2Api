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
	// 已激活过（activationCount>0 或有 trialStart）或明确不允许 → 跳过。
	if !st.IsAllowed || st.TrialActivationCount > 0 || st.TrialStart != "" {
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
		return false, true
	}
	res, err := r.Provider.StartTrial(ctx, acc, clientSoloExtensionTrial, 0)
	if err != nil {
		log.Printf("[trial-extend] 账号 %d(%s) 开通 extension trial 失败: %v", acc.ID, acc.Email, err)
		return false, false
	}
	log.Printf("[trial-extend] 账号 %d(%s) extension trial 已开通, 截止 %s", acc.ID, acc.Email, res.TrialPeriodEnd)
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
