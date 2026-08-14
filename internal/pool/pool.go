package pool

import (
	"fmt"
	"strings"
	"sync"

	"ps2api/internal/store"
)

type Pool struct {
	store    *store.Store
	mu       sync.Mutex
	last     int
	inFlight map[int64]int
}

func New(s *store.Store) *Pool { return &Pool{store: s, last: -1, inFlight: map[int64]int{}} }

func (p *Pool) Next(excluded map[int64]bool) (*store.Account, error) {
	accounts, err := p.store.ActiveAccounts()
	if err != nil || len(accounts) == 0 {
		return nil, errNoAccounts(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	start := (p.last + 1) % len(accounts)
	best := -1
	bestLoad := int(^uint(0) >> 1)
	for i := 0; i < len(accounts); i++ {
		idx := (start + i) % len(accounts)
		acc := accounts[idx]
		if excluded[acc.ID] {
			continue
		}
		load := p.inFlight[acc.ID]
		if load < bestLoad {
			best, bestLoad = idx, load
		}
		if load == 0 {
			break
		}
	}
	if best < 0 {
		return nil, errNoAccounts(nil)
	}
	p.last = best
	p.inFlight[accounts[best].ID]++
	return accounts[best], nil
}

func (p *Pool) Done(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight[id] <= 1 {
		delete(p.inFlight, id)
	} else {
		p.inFlight[id]--
	}
}

func (p *Pool) MarkUsed(id int64) { _ = p.store.MarkUsed(id) }
// MarkExhausted 只置状态；真实额度（limit/remaining）由 router.persistQuota 在
// 每次聊天后写入（usage 事件携带 limit/usage，耗尽时 remaining 会算成 0）。
// 同时落一条真实告警记录（去重：同账号未处理告警只留一条）。
func (p *Pool) MarkExhausted(id int64) {
	_ = p.store.SetAccountStatus(id, "exhausted", "Postman AI quota exceeded")
	sid := id
	acc, _ := p.store.GetAccount(id)
	title := "账号额度耗尽"
	if acc != nil && acc.Email != "" {
		title = "账号额度耗尽: " + acc.Email
	}
	_ = p.store.CreateAlert("warning", title, "Postman AI quota exceeded，已自动切换到其他账号", "account", &sid, "quota_exhausted")
}
func (p *Pool) MarkError(id int64, msg string) {
	_ = p.store.SetAccountStatus(id, "error", msg)
	sid := id
	acc, _ := p.store.GetAccount(id)
	title := "账号异常"
	if acc != nil && acc.Email != "" {
		title = "账号异常: " + acc.Email
	}
	_ = p.store.CreateAlert("severe", title, msg, "account", &sid, "account_error")
}
func (p *Pool) MarkTransient(id int64, msg string) { _ = p.store.SetAccountStatus(id, "active", msg) }

func errNoAccounts(cause error) error {
	if cause != nil {
		return cause
	}
	return &NoAccountsError{}
}

type NoAccountsError struct{}

func (*NoAccountsError) Error() string {
	return "No active accounts available. Add a Postman account first."
}

func IsTransient(msg string) bool {
	for _, s := range []string{"timeout", "econnreset", "connection reset", "fetch failed", "network", "rate limited", "429"} {
		if strings.Contains(strings.ToLower(msg), s) {
			return true
		}
	}
	return false
}
func FormatNoAccounts() string { return fmt.Sprint((&NoAccountsError{}).Error()) }
