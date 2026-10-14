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

// TrialStatus 是 GET /api/trials?client_trial_id=<id> 的解析结果（accountTrialDetails）。
// 零消耗查询：isAllowed=false 或 activationCount>0 表示该 trial 槽位已用过/不可开。
type TrialStatus struct {
	TrialID              string `json:"id"` // 服务端 trial ID（solo-7-days-extension-trial 等）
	IsAllowed            bool   `json:"isAllowed"`
	TrialActivationCount int    `json:"trialActivationCount"`
	TrialStart           string `json:"trialStart"` // "2026-10-14 07:12:46" 格式；未开过为空
	TrialEnd             string `json:"trialEnd"`
	Duration             int    `json:"duration"` // 天数
}

// FetchTrialStatus 查询账号某 client trial ID 的槽位状态（零消耗）。
func (p *Provider) FetchTrialStatus(ctx context.Context, acc *store.Account, clientTrialID string, egressAttempt int) (*TrialStatus, error) {
	tokens, err := p.GetTokens(acc)
	if err != nil {
		return nil, err
	}
	body, err := p.doWSProxy(ctx, acc, tokens, "billing", "get", "/api/trials?client_trial_id="+clientTrialID, egressAttempt)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Result              string       `json:"result"`
		TrialID             string       `json:"id"`
		IsAllowed           *bool        `json:"isAllowed,omitempty"`
		Duration            int          `json:"duration"`
		AccountTrialDetails *TrialStatus `json:"accountTrialDetails"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("trial status parse: %w", err)
	}
	if parsed.Result != "success" {
		return nil, fmt.Errorf("trial status error: %s", truncateRunes(string(body), 200))
	}
	// accountTrialDetails 缺 trial_id/isAllowed 顶层语义，此处拼装统一形状：
	// isAllowed 缺省按 false 处理（保守：查不到明确许可就不开）。
	st := &TrialStatus{TrialID: parsed.TrialID, Duration: parsed.Duration, IsAllowed: false}
	if parsed.AccountTrialDetails != nil {
		st.IsAllowed = parsed.AccountTrialDetails.IsAllowed
		st.TrialActivationCount = parsed.AccountTrialDetails.TrialActivationCount
		st.TrialStart = parsed.AccountTrialDetails.TrialStart
		st.TrialEnd = parsed.AccountTrialDetails.TrialEnd
	}
	return st, nil
}

// StartTrialResult 是 POST /api/trials 成功响应的解析结果。
type StartTrialResult struct {
	AccountID      string `json:"account_id"`
	TeamID         string `json:"team_id"`
	TrialID        string `json:"trial_id"`
	TrialPeriodEnd string `json:"trial_period_end"`
}

// StartTrial 为账号开启一个 trial（body {id: clientTrialID}）。
// 成功响应 {"result":"success",...}；业务拒绝（maxAllowedTrials=1 等）返回 400 + error body。
func (p *Provider) StartTrial(ctx context.Context, acc *store.Account, clientTrialID string, egressAttempt int) (*StartTrialResult, error) {
	tokens, err := p.GetTokens(acc)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"service": "billing", "method": "POST", "path": "/api/trials", "body": map[string]string{"id": clientTrialID}})
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
	h := req.Header
	if tokens.WorkspaceID != "" {
		h.Set("x-entity-team-id", tokens.WorkspaceID) // startTrial 实测必须带（2026-09-23 抓包）
	}
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
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("start trial (%d): %s", resp.StatusCode, truncateRunes(string(b), 200))
	}
	var out StartTrialResult
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("start trial parse: %w", err)
	}
	if out.TrialID == "" {
		return nil, fmt.Errorf("start trial unexpected body: %s", truncateRunes(string(b), 200))
	}
	return &out, nil
}
