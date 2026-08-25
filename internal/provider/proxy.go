package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// accountCookieJars keeps Cloudflare/session cookies isolated by account, host,
// and configured egress. Captured cookies are never shared across accounts.
type accountCookieJars struct {
	mu   sync.Mutex
	jars map[string]*cookiejar.Jar
}

func newAccountCookieJars() *accountCookieJars {
	return &accountCookieJars{jars: map[string]*cookiejar.Jar{}}
}

func (s *accountCookieJars) jarFor(accountID int64, host, egress string) *cookiejar.Jar {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strconv.FormatInt(accountID, 10) + "|" + host + "|" + egress
	if jar := s.jars[key]; jar != nil {
		return jar
	}
	jar, _ := cookiejar.New(nil)
	s.jars[key] = jar
	return jar
}

func (s *accountCookieJars) cookies(accountID int64, u *url.URL, egress string) []*http.Cookie {
	if s == nil || u == nil {
		return nil
	}
	return s.jarFor(accountID, u.Host, egress).Cookies(u)
}

func (s *accountCookieJars) remember(accountID int64, u *url.URL, egress string, cookies []*http.Cookie) {
	if s == nil || u == nil || len(cookies) == 0 {
		return
	}
	s.jarFor(accountID, u.Host, egress).SetCookies(u, cookies)
}

// proxyPool 管理可配置的出口代理集合，为每个代理 URL 缓存一个复用连接池的 *http.Client。
//
// 出口选择是无状态确定式：索引 = (stickyBase(accountID) + egressAttempt) mod N。
// 因此同一账号默认粘在同一出口（连接复用、会话稳定），只有当 egressAttempt 递增
// （router 遇 Cloudflare 403 重试时 +1）才切到下一个出口 IP。只要配置了至少一个出口代理，
// 每次出站都经代理（egressAttempt 越界会环回到有效出口，不再回退直连）；仅当未配置任何
// 代理时才走本机直连。
type proxyPool struct {
	list     func() []string // 运行时代理列表来源（读自持久化设置），面板改动即时生效
	fallback func() bool     // 「代理全挂兜底直连」开关来源；nil 视为关闭
	mu       sync.Mutex
	clients  map[string]*http.Client // 按代理 URL 缓存，复用连接池
}

func newProxyPool() *proxyPool {
	return &proxyPool{clients: map[string]*http.Client{}}
}

// SetProxyList 注入运行时代理列表来源。fn 返回原始配置串（可含换行/逗号分隔的多个 URL）。
func (p *Provider) SetProxyList(fn func() []string) { p.proxies.list = fn }

// SetProxyFallbackDirect 注入「代理全挂兜底直连」开关来源。开启后，当出口代理在传输层
// 不可达（拨号/CONNECT 失败）时，本次请求改用本机直连重试一次而非直接失败；关闭则严格
// 只走代理（代理全挂即请求失败）。每次请求实时读取，面板改动即时生效。
func (p *Provider) SetProxyFallbackDirect(fn func() bool) { p.proxies.fallback = fn }

// fallbackDirectEnabled 报告是否开启「代理全挂兜底直连」。来源未注入时视为关闭。
func (pp *proxyPool) fallbackDirectEnabled() bool {
	return pp != nil && pp.fallback != nil && pp.fallback()
}

// CheckProxies 检测给定的代理列表（原始配置串，可含多行/逗号分隔），
// 返回每个出口的连通性与响应耗时。单个代理超时上限 8s。
func (p *Provider) CheckProxies(ctx context.Context, urls []string) []ProxyCheckResult {
	return p.proxies.CheckProxies(ctx, urls, 8*time.Second)
}

// CheckProxyDetail 对单个代理做「详细测试」：在连通性/耗时之外，额外经该代理查询出口
// 公网 IP 与归属地（地区码/省/市/运营商），供代理页单条「测试」按钮展示出口现场。
// 每一步超时上限 10s。
func (p *Provider) CheckProxyDetail(ctx context.Context, raw string) ProxyCheckResult {
	return p.proxies.checkOneDetail(ctx, raw, 10*time.Second)
}

// selectFor 返回本次出站应使用的代理 client 与出口标签。
// ok=false 仅表示未配置任何可用出口代理（proxy_enabled 关闭或 proxy_urls 为空/全部无效）
// → 走本机直连。只要配置了至少一个出口代理，本次请求就一定经代理出站：egressAttempt 越界
//（>=N 或 <0）时按出口数环回到有效出口，而非回退本机直连——即「开启启用出口代理后所有发往
// 上游的请求都走代理」。egressAttempt 递增仅用于在多出口间轮换 IP（403 换号时）。
func (pp *proxyPool) selectFor(accountID int64, egressAttempt int) (client *http.Client, egress string, ok bool) {
	if pp == nil {
		return nil, "", false
	}
	urls := pp.urls()
	n := len(urls)
	if n == 0 {
		return nil, "", false
	}
	// 非负取模：把出口序号钉进 [0,N)，越界即环回，绝不因序号越界回退直连。
	idx := ((stickyBase(accountID)+egressAttempt)%n + n) % n
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
		// 经代理出站也携带同一套 uTLS 伪装指纹：CONNECT/SOCKS5 隧道 + HelloCustom 握手，
		// 由 newFingerprintProxyTransport 内的 DialTLSContext 接管（不再用标准库 Proxy 字段，
		// 否则目标握手会退回 Go 默认指纹，与直连不一致，风控绕过失效）。
		Transport: newFingerprintProxyTransport(u),
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
// 出口地理信息（EgressIP/CountryCode/Region/City/Org）仅在单代理「详细测试」时填充，
// 批量连通性检测为节省耗时不做地理查询，这些字段留空。
type ProxyCheckResult struct {
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	// 出口现场信息：经该代理实际出站时对端看到的公网 IP 与其归属地。
	EgressIP    string `json:"egressIp,omitempty"`
	CountryCode string `json:"countryCode,omitempty"` // 两位地区码，如 US、JP、HK
	Region      string `json:"region,omitempty"`      // 省/州
	City        string `json:"city,omitempty"`
	Org         string `json:"org,omitempty"` // 运营商/ASN，如 "AS15169 Google LLC"
	GeoError    string `json:"geoError,omitempty"`
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

// proxyGeoTarget 是查询出口公网 IP 与归属地的目标。ipinfo.io/json 支持 https、
// 免 token 有免费额度，返回 ip/country(两位地区码)/region/city/org。
const proxyGeoTarget = "https://ipinfo.io/json"

// checkOneDetail 在 checkOne（连通上游 + 耗时）基础上，再经同一代理 client 查询出口
// 公网 IP 与归属地。地理查询失败不影响连通性判定，仅在 GeoError 里记原因。
func (pp *proxyPool) checkOneDetail(ctx context.Context, raw string, perTimeout time.Duration) ProxyCheckResult {
	r := pp.checkOne(ctx, raw, perTimeout)
	client := pp.clientFor(raw)
	if client == nil {
		return r
	}
	gctx, cancel := context.WithTimeout(ctx, perTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(gctx, http.MethodGet, proxyGeoTarget, nil)
	if err != nil {
		r.GeoError = err.Error()
		return r
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if gctx.Err() == context.DeadlineExceeded {
			r.GeoError = "查询出口 IP 超时"
		} else {
			r.GeoError = strings.TrimSpace(err.Error())
		}
		return r
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var g struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
		Region  string `json:"region"`
		City    string `json:"city"`
		Org     string `json:"org"`
	}
	if err := json.Unmarshal(body, &g); err != nil {
		r.GeoError = "解析出口 IP 响应失败"
		return r
	}
	r.EgressIP = g.IP
	r.CountryCode = g.Country
	r.Region = g.Region
	r.City = g.City
	r.Org = g.Org
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
