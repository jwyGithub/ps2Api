package pool

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"ps2api/internal/store"
)

type Pool struct {
	store    *store.Store
	mu       sync.Mutex
	last     int
	inFlight map[int64]int
	// gatewayCooldown 记录被上游网关(Cloudflare)风控拦截的账号的冷却截止时间。
	// 这类 403 是账号身份被 WAF/Bot 评分标记，短期内换号才有效、重试同号必然再被拦，
	// 故在冷却窗口内 Next 优先跳过这些「被烧」账号。内存态、重启归零（与 inFlight 一致）。
	gatewayCooldown map[int64]time.Time
}

func New(s *store.Store) *Pool {
	return &Pool{store: s, last: -1, inFlight: map[int64]int{}, gatewayCooldown: map[int64]time.Time{}}
}

func (p *Pool) Next(excluded map[int64]bool) (*store.Account, error) {
	accounts, err := p.store.ActiveAccounts()
	if err != nil || len(accounts) == 0 {
		return nil, errNoAccounts(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 第一轮：跳过 excluded 且跳过仍在网关冷却窗口内的账号，优先挑选负载最低的健康账号。
	// 第二轮兜底（skipCooldown=false）：所有健康账号都在冷却时，宁可再试冷却中的账号，
	// 也不直接失败——冷却是「降级」而非「禁用」。
	pick := func(skipCooldown bool) int {
		start := (p.last + 1) % len(accounts)
		best := -1
		bestLoad := int(^uint(0) >> 1)
		for i := 0; i < len(accounts); i++ {
			idx := (start + i) % len(accounts)
			acc := accounts[idx]
			if excluded[acc.ID] {
				continue
			}
			if skipCooldown {
				if until, ok := p.gatewayCooldown[acc.ID]; ok && now.Before(until) {
					continue
				}
			}
			load := p.inFlight[acc.ID]
			if load < bestLoad {
				best, bestLoad = idx, load
			}
			if load == 0 {
				break
			}
		}
		return best
	}
	best := pick(true)
	if best < 0 {
		best = pick(false)
	}
	if best < 0 {
		return nil, errNoAccounts(nil)
	}
	p.last = best
	p.inFlight[accounts[best].ID]++
	return accounts[best], nil
}

// MarkGatewayBlocked 将账号置入网关冷却窗口。被上游 Cloudflare 风控按身份拦截(403)的账号，
// 短期内重试必然再被拦，冷却期内 Next 会优先跳过它、改用健康账号，实现「被烧号自动降级」。
// 不改账号 status（账号本身健康、额度可用），冷却是纯路由层的临时降级，到期自动恢复。
func (p *Pool) MarkGatewayBlocked(id int64, d time.Duration) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gatewayCooldown[id] = time.Now().Add(d)
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
	for _, s := range []string{"timeout", "econnreset", "connection reset", "fetch failed", "network", "rate limited", "429",
		"write tcp", "read tcp", "broken pipe", "eof", "connection refused", "no such host", "i/o timeout", "tls handshake"} {
		if strings.Contains(strings.ToLower(msg), s) {
			return true
		}
	}
	return false
}
func FormatNoAccounts() string { return fmt.Sprint((&NoAccountsError{}).Error()) }
