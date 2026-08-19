package provider

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// proxyPool 管理可配置的出口代理集合，为每个代理 URL 缓存一个复用连接池的 *http.Client。
//
// 出口选择是无状态确定式：索引 = (stickyBase(accountID) + egressAttempt) mod N。
// 因此同一账号默认粘在同一出口（连接复用、会话稳定），只有当 egressAttempt 递增
// （router 遇 Cloudflare 403 重试时 +1）才切到下一个出口 IP；当 egressAttempt >= N
// （所有出口都试过）或未配置代理时回退本机直连，保证服务不中断。
type proxyPool struct {
	list    func() []string // 运行时代理列表来源（读自持久化设置），面板改动即时生效
	mu      sync.Mutex
	clients map[string]*http.Client // 按代理 URL 缓存，复用连接池
}

func newProxyPool() *proxyPool {
	return &proxyPool{clients: map[string]*http.Client{}}
}

// SetProxyList 注入运行时代理列表来源。fn 返回原始配置串（可含换行/逗号分隔的多个 URL）。
func (p *Provider) SetProxyList(fn func() []string) { p.proxies.list = fn }

// CheckProxies 检测给定的代理列表（原始配置串，可含多行/逗号分隔），
// 返回每个出口的连通性与响应耗时。单个代理超时上限 8s。
func (p *Provider) CheckProxies(ctx context.Context, urls []string) []ProxyCheckResult {
	return p.proxies.CheckProxies(ctx, urls, 8*time.Second)
}

// selectFor 返回本次出站应使用的代理 client 与出口标签。
// ok=false 表示应走本机直连：未配置代理，或本次 egressAttempt 已越过所有出口
// （即所有代理都试过后的兜底）。
func (pp *proxyPool) selectFor(accountID int64, egressAttempt int) (client *http.Client, egress string, ok bool) {
	if pp == nil {
		return nil, "", false
	}
	urls := pp.urls()
	n := len(urls)
	if n == 0 || egressAttempt < 0 || egressAttempt >= n {
		return nil, "", false
	}
	idx := (stickyBase(accountID) + egressAttempt) % n
	raw := urls[idx]
	c := pp.clientFor(raw)
	if c == nil {
		return nil, "", false
	}
	return c, raw, true
}

func (pp *proxyPool) urls() []string {
	if pp == nil || pp.list == nil {
		return nil
	}
	return parseProxyURLs(pp.list())
}

// clientFor 按代理 URL 惰性构建并缓存 client。无效 URL 返回 nil（selectFor 据此回退直连）。
func (pp *proxyPool) clientFor(raw string) *http.Client {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if c, ok := pp.clients[raw]; ok {
		return c
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		pp.clients[raw] = nil // 记住无效项，避免每次请求重复解析
		return nil
	}
	c := &http.Client{
		Timeout: 0, // 与直连一致：用 ctx 控制超时，流式不能有总超时
		Transport: &http.Transport{
			Proxy:                 http.ProxyURL(u), // 标准库原生支持 http/https/socks5 代理
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
	pp.clients[raw] = c
	return c
}

// ─── 出口代理连通性检测 ─────────────────────────────────────────

// proxyCheckTarget 是检测代理用的探测目标：真实上游网关主机。
// 只要能经代理拿到任意 HTTP 响应（状态码由上游决定），就说明代理隧道打通；
// 传输层报错（拨号失败/超时/认证拒绝）才判定该代理不可用。
const proxyCheckTarget = "https://gateway.postman.com/"

// ProxyCheckResult 单个代理的检测结果。URL 已脱敏（隐藏密码）。
// OK 项之间可按 LatencyMs 比较，值越小代理越快。
type ProxyCheckResult struct {
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// CheckProxies 并发检测每个代理能否连通上游并测其响应耗时。
// 面板「出口代理检测」用它在启用前发现配置错误/不可达的出口，
// 并按 LatencyMs 比出哪个出口更快。perTimeout 为单个代理的超时上限。
func (pp *proxyPool) CheckProxies(ctx context.Context, urls []string, perTimeout time.Duration) []ProxyCheckResult {
	urls = parseProxyURLs(urls)
	results := make([]ProxyCheckResult, len(urls))
	var wg sync.WaitGroup
	for i, raw := range urls {
		wg.Add(1)
		go func(i int, raw string) {
			defer wg.Done()
			results[i] = pp.checkOne(ctx, raw, perTimeout)
		}(i, raw)
	}
	wg.Wait()
	return results
}

func (pp *proxyPool) checkOne(ctx context.Context, raw string, perTimeout time.Duration) ProxyCheckResult {
	r := ProxyCheckResult{URL: redactProxyURL(raw)}
	client := pp.clientFor(raw)
	if client == nil {
		r.Error = "无效的代理 URL（缺少 scheme 或 host）"
		return r
	}
	cctx, cancel := context.WithTimeout(ctx, perTimeout)
	defer cancel()
	// 用 HEAD 减少传输量；对 https 目标代理走 CONNECT 隧道，方法不影响连通性判定。
	req, err := http.NewRequestWithContext(cctx, http.MethodHead, proxyCheckTarget, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			r.Error = "连接超时（代理不可达或响应过慢）"
		} else {
			r.Error = strings.TrimSpace(err.Error())
		}
		return r
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	r.LatencyMs = time.Since(started).Milliseconds()
	r.Status = resp.StatusCode
	r.OK = true // 拿到 HTTP 响应即代理隧道已打通
	return r
}

// redactProxyURL 脱敏代理 URL 中的密码，便于把检测结果安全回显到面板。
func redactProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPass := u.User.Password(); hasPass {
		u.User = url.UserPassword(u.User.Username(), "****")
	}
	return u.String()
}

// parseProxyURLs 把原始配置串拆成去重、已校验的代理 URL 列表。
// 分隔符兼容换行、逗号、分号、空白，方便面板里粘贴多行或逗号分隔。
func parseProxyURLs(raw []string) []string {
	var out []string
	seen := map[string]bool{}
	split := func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';' || r == ' ' || r == '\t'
	}
	for _, chunk := range raw {
		for _, field := range strings.FieldsFunc(chunk, split) {
			s := strings.TrimSpace(field)
			if s == "" || seen[s] {
				continue
			}
			u, err := url.Parse(s)
			if err != nil || u.Scheme == "" || u.Host == "" {
				continue // 跳过缺 scheme/host 的无效项，保证 N 只计有效出口
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// stickyBase 由账号 ID 推出一个稳定的非负基数，用于把账号确定性地映射到某个出口，
// 使同一账号默认粘在同一出口 IP（无需保存状态）。
func stickyBase(accountID int64) int {
	if accountID < 0 {
		accountID = -accountID
	}
	return int(accountID % 1_000_000_007)
}
