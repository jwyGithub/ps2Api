package router

import (
	"context"
	"sync"

	"ps2api/internal/store"
)

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
