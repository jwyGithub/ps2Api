// billing.go —— Postman「billing」微服务直查（ws/proxy 协议，2026-09-29 逆向+实测）。
// 端点: POST https://bifrost-https-v10.gw.postman.com/ws/proxy
//   body: {"service":"billing","method":"get","path":"/api/users/<uid>/operations?view=artemis"}
// 一次拉回 14 项额度全景（ai_millicredits/flow_requests/api_usage/...），零消耗——
// 取代 probe.go 里「发一次真请求烧 token」的探测口径（保留 probe 作兜底）。
// 实测注意：① user_id 必须真实（错/别人的 → 403 空体）；② bifrost 直连超时，
// 须走出口代理池（与 /chat 同池复用）；③ toolsets 端点不计 ai_millicredits，
// 此处的 usage 是 /chat(Agent Mode) 的消耗（见 memory/postman-billing-wsproxy-apis）。

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ps2api/internal/store"
)

// BillingOpsTimeout billing 直查的请求超时。
const BillingOpsTimeout = 30 * time.Second

// wsProxyURL 是 ws/proxy 转发端点（客户端 __WP_HTTPS_GATEWAY_PRIVATE_URL__ 域）。
const wsProxyURL = "https://bifrost-https-v10.gw.postman.com/ws/proxy"

// BillingUsage 是 operations 里单条额度条目（只取网关关心的字段）。
type BillingUsage struct {
	Name     string  `json:"name"`
	Limit    float64 `json:"limit"`
	Usage    float64 `json:"usage"`
	Overage  float64 `json:"overage"`
	Spillage float64 `json:"spillage"`
	Plan     string  `json:"plan"`
	IsPooled string  `json:"is_pooled"`
}

// BillingOpsResult 是一次 operations 查询的解析结果。
type BillingOpsResult struct {
	AI      *BillingUsage // ai_millicredits 条目（额度主口径）
	All     []BillingUsage
	RawBody []byte
}

// wsProxyHeaders 构造 ws/proxy 出站头（与客户端 bundle 的 y() 函数同口径：
// x-access-token + x-app-version；multi-login 网关实测多发无害，保留双 token 兼容）。
func (p *Provider) wsProxyHeaders(tokens *Tokens) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	if tokens.AccessToken != "" {
		h.Set("x-access-token", tokens.AccessToken)
		if tokens.MultiLoginToken != "" {
			h.Set("x-multi-login-token", tokens.MultiLoginToken)
		}
	}
	h.Set("x-app-version", ToolsetsAppVersion)
	h.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Postman/"+ToolsetsAppVersion+" Electron/37.10.3 Safari/537.36")
	return h
}

// doWSProxy 发一次 ws/proxy 转发调用并返回原始 body。走与 /chat 相同的出口代理池
// （bifrost 直连超时是实测结论）；egressAttempt 用于换出口重试。
func (p *Provider) doWSProxy(ctx context.Context, acc *store.Account, tokens *Tokens, service, method, path string, egressAttempt int) ([]byte, error) {
	payload, _ := json.Marshal(map[string]string{"service": service, "method": method, "path": path})
	ctx, cancel := context.WithTimeout(ctx, BillingOpsTimeout)
	defer cancel()
	client, egress, viaProxy := p.proxies.selectFor(acc.ID, egressAttempt)
	if !viaProxy {
		client, egress = p.Client, "direct"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", wsProxyURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header = p.wsProxyHeaders(tokens)
	p.applyCookies(acc.ID, req, egress)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("billing auth failed (%d)", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("billing ws/proxy error (%d): %s", resp.StatusCode, truncateRunes(string(b), 200))
	}
	return b, nil
}

// FetchBillingOps 拉取账号的 operations 额度全景并解析。
// egressAttempt 用于出口重试（与 streamInternal 的出口尝试口径一致）。
func (p *Provider) FetchBillingOps(ctx context.Context, acc *store.Account, egressAttempt int) (*BillingOpsResult, error) {
	tokens, err := p.GetTokens(acc)
	if err != nil {
		return nil, err
	}
	if tokens.UserID == "" || tokens.UserID == "0" {
		return nil, fmt.Errorf("account has no user_id (billing ops 需要真实 user_id)")
	}
	path := "/api/users/" + tokens.UserID + "/operations?view=artemis&dropdown_order=true"
	body, err := p.doWSProxy(ctx, acc, tokens, "billing", "get", path, egressAttempt)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Operations []BillingUsage `json:"operations"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("billing ops parse: %w", err)
	}
	out := &BillingOpsResult{All: parsed.Operations, RawBody: body}
	for i, o := range parsed.Operations {
		if o.Name == "ai_millicredits" {
			out.AI = &parsed.Operations[i]
			break
		}
	}
	return out, nil
}
