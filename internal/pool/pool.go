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
	// reservedUntil 记录账号「最近被某客户端占用」的软预留截止时间：每次成功交付后由
	// 路由层 Reserve 刷新。inFlight 只在请求真正在飞时 >0，一个对话在回合间隙（用户打字/
	// agent 思考）里 inFlight 归 0，账号看起来空闲，别的客户端就会挑到同一个号，导致多客户端
	// 挤同一账号、额度被快速烧穿、频繁换号。reservedUntil 补上这个「回合间隙」的空档：普通
	// 轮询选号时把仍在预留窗口内的账号作为「次级」避让项——同等 inFlight 负载下优先挑没被占用
	// 的号，从而把不同客户端摊到不同账号上。纯软避让：永不把账号踢出候选，池子小/全被预留时
	// 仍照常复用，不影响可用性。内存态、重启归零（与 inFlight / gatewayCooldown 一致）。
	reservedUntil map[int64]time.Time
}

func New(s *store.Store) *Pool {
	return &Pool{store: s, last: -1, inFlight: map[int64]int{}, gatewayCooldown: map[int64]time.Time{}, reservedUntil: map[int64]time.Time{}}
}

// QuotaMode 控制 403 网关 failover 换号时的选号策略（由设置 prefer_quota_on_403 决定）。
type QuotaMode int

const (
	// QuotaModeRoundRobin 关闭额度优先：换号也走普通轮询（inFlight 负载最低），不看剩余额度。
	QuotaModeRoundRobin QuotaMode = iota
	// QuotaModeRatio 按剩余额度比例 RateRemaining/RateLimit 降序：各账号 RateLimit 不一致时更公平。
	QuotaModeRatio
	// QuotaModeAbsolute 按剩余额度绝对值 RateRemaining 降序：只看余量绝对多少。
	QuotaModeAbsolute
)

func (p *Pool) Next(excluded map[int64]bool) (*store.Account, error) {
	return p.next(excluded, QuotaModeRoundRobin)
}

// NextByRateRemaining 是 NextByQuota(QuotaModeRatio) 的别名，保留以兼容既有调用方/测试。
func (p *Pool) NextByRateRemaining(excluded map[int64]bool) (*store.Account, error) {
	return p.next(excluded, QuotaModeRatio)
}

// NextByQuota 与 Next 语义一致（同样跳过 excluded/冷却账号、同样 inFlight++、返回可用账号），
// 区别在于按 mode 指定的额度策略选号。专用于 403 网关 failover 换号——被上游 Cloudflare 风控拦截后，
// 优先切到「最新鲜、余量最满」的账号，降低换到的新号立刻又被风控/限流拦截的概率。
// 额度并列（或 mode=RoundRobin）时再比 inFlight 负载（低者优先）。
func (p *Pool) NextByQuota(excluded map[int64]bool, mode QuotaMode) (*store.Account, error) {
	return p.next(excluded, mode)
}

func (p *Pool) next(excluded map[int64]bool, mode QuotaMode) (*store.Account, error) {
	accounts, err := p.store.ActiveAccounts()
	if err != nil || len(accounts) == 0 {
		return nil, errNoAccounts(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 三轮降级选号，排除项由硬到软：用量额度耗尽 > 网关冷却。
	//   轮1：跳过 excluded、网关冷却中、用量额度已耗尽的账号——挑最优健康账号。
	//   轮2：允许冷却中账号，但仍跳过额度耗尽——额度见底的号发出去必然拿不到结果，
	//        比「出口不够新鲜」更该排除，故额度优先级高于冷却。
	//   轮3：全放开（额度也不跳）——额度快照可能陈旧（如从未刷新/上周期归零），
	//        宁可一试也不直接硬失败。冷却/耗尽都是「降级」而非「禁用」。
	best := p.pickIndex(accounts, excluded, now, true, true, mode)
	if best < 0 {
		best = p.pickIndex(accounts, excluded, now, false, true, mode)
	}
	if best < 0 {
		best = p.pickIndex(accounts, excluded, now, false, false, mode)
	}
	if best < 0 {
		return nil, errNoAccounts(nil)
	}
	p.last = best
	p.inFlight[accounts[best].ID]++
	return accounts[best], nil
}

// quotaExhausted 判断账号的 AI 用量额度是否已耗尽：仅当额度上限已知（QuotaLimit>0）
// 且剩余 <=0 才判为耗尽。上限未知（QuotaLimit==0，如尚未刷新过额度）不误判为耗尽，
// 避免把额度信息缺失的号错误地排除出选号池。
func quotaExhausted(acc *store.Account) bool {
	return acc.QuotaLimit > 0 && acc.QuotaRemaining <= 0
}

// pickIndex 在 accounts 中挑一个候选下标，返回 -1 表示无可用账号。跳过 excluded；
// skipCooldown 为真时额外跳过仍在网关冷却窗口内的账号；
// skipExhausted 为真时额外跳过 AI 用量额度已耗尽（quotaExhausted）的账号——这类号即便
// 速率窗口新鲜（RateRemaining 高）也发不出有效结果，若不跳会在 403 换号「选额度最满」策略里
// 反被优先选中，白白浪费重试预算。
//   - mode=RoundRobin（默认/普通选号）：从轮询起点 (last+1) 出发，以 inFlight 负载升序为主键、
//     「是否仍在软预留窗口内」升序为次键挑账号——即同等负载下优先挑没被别的客户端占用的号，
//     把多客户端摊到不同账号上；仅当命中「负载 0 且未被预留」的理想账号才提前返回。预留只是
//     软避让、永不淘汰账号：全被预留时最优候选照样是某个被预留号，不影响可用性。
//   - mode=Ratio/Absolute（403 换号）：以剩余额度（比例或绝对值）降序为主键、
//     inFlight 负载升序为次键，必须遍历全部账号以找出额度最高者（不能提前 break）。
//     额度同、负载同时由轮询起点决定先后。软预留避让只作用于 RoundRobin，不介入 403 换号
//     （那一步的目标是切到「最新鲜、余量最满」的号，与占用避让正交）。
func (p *Pool) pickIndex(accounts []*store.Account, excluded map[int64]bool, now time.Time, skipCooldown, skipExhausted bool, mode QuotaMode) int {
	start := (p.last + 1) % len(accounts)
	best := -1
	bestLoad := int(^uint(0) >> 1)
	bestReserved := 2 // 0=未预留,1=预留中；初值 2 保证首个候选必被采纳
	bestRemaining, bestLimit := -1, 0
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
		if skipExhausted && quotaExhausted(acc) {
			continue
		}
		load := p.inFlight[acc.ID]
		if mode != QuotaModeRoundRobin {
			if best == -1 {
				best, bestRemaining, bestLimit, bestLoad = idx, acc.RateRemaining, acc.RateLimit, load
				continue
			}
			switch quotaCmp(mode, acc.RateRemaining, acc.RateLimit, bestRemaining, bestLimit) {
			case 1:
				best, bestRemaining, bestLimit, bestLoad = idx, acc.RateRemaining, acc.RateLimit, load
			case 0:
				if load < bestLoad {
					best, bestRemaining, bestLimit, bestLoad = idx, acc.RateRemaining, acc.RateLimit, load
				}
			}
			continue
		}
		// 主键 inFlight 负载升序、次键「软预留」升序：同等负载下优先未被占用的号。
		reserved := 0
		if until, ok := p.reservedUntil[acc.ID]; ok && now.Before(until) {
			reserved = 1
		}
		if load < bestLoad || (load == bestLoad && reserved < bestReserved) {
			best, bestLoad, bestReserved = idx, load, reserved
		}
		// 仅「负载 0 且未被预留」才是理想账号，可提前收工；否则继续找更优的。
		if load == 0 && reserved == 0 {
			break
		}
	}
	return best
}

// quotaCmp 按 mode 比较两账号的剩余额度，返回 1(a 更优)/-1(b 更优)/0(相等)。
// Absolute 直接比剩余绝对值 RateRemaining；其余（Ratio）走 ratioCmp 比剩余比例。
func quotaCmp(mode QuotaMode, ra, la, rb, lb int) int {
	if mode == QuotaModeAbsolute {
		switch {
		case ra > rb:
			return 1
		case ra < rb:
			return -1
		default:
			return 0
		}
	}
	return ratioCmp(ra, la, rb, lb)
}

// ratioCmp 比较两账号的剩余额度比例 (RateRemaining/RateLimit)。
// 返回 1 表示 a 更优（比例更高），-1 表示 b 更优，0 表示相等。
// 采用交叉相乘做整数比较，避免浮点误差；RateLimit<=0（额度上限未知）视为比例 0，排在最后。
// 用比例而非绝对剩余值，可在各账号 RateLimit 不一致时更公平地衡量「余量新鲜度」。
func ratioCmp(ra, la, rb, lb int) int {
	aKnown, bKnown := la > 0, lb > 0
	switch {
	case !aKnown && !bKnown:
		return 0
	case !aKnown:
		if rb > 0 {
			return -1
		}
		return 0
	case !bKnown:
		if ra > 0 {
			return 1
		}
		return 0
	}
	// ra/la vs rb/lb  ⇔  ra*lb vs rb*la（la、lb 均 >0，方向不变）
	left, right := int64(ra)*int64(lb), int64(rb)*int64(la)
	switch {
	case left > right:
		return 1
	case left < right:
		return -1
	default:
		return 0
	}
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

// Reserve 把账号标记为「最近被占用」，软预留窗口 d 内的普通轮询选号会优先避开它（次级避让，
// 见 pickIndex）。由路由层在每次成功交付后刷新，使「占用」标记从本回合完成时刻起算，覆盖对话
// 回合之间 inFlight 已归零的空档，避免别的客户端在这段间隙里挤到同一个账号。d<=0 时不预留
// （等价于关闭该机制，由设置 account_reservation_seconds=0 触发）。内存态、重启归零。
func (p *Pool) Reserve(id int64, d time.Duration) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reservedUntil[id] = time.Now().Add(d)
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
