// toolsets.go —— Postman「工具集(Toolsets)」Anthropic 原生代理端点接入。
// 端点: POST https://<subdomain>.postman.co/_gw/toolsets/v1/messages
// 实测（2026-09-24）: claude-opus-5 / claude-sonnet-5 可用（Agent Mode /chat 白名单外），
// 请求/响应均为标准 Anthropic Messages 协议，故对 /v1/messages 客户端做原生透传（零转换）。
// 关键头 x-pstmn-req-service: ai-toolsets（缺它一律 404）。
// 限频: 连续 ~6 请求后 429 rate_limited / upstream_unavailable，需退避重试。
// 无 credits usage 事件（响应 usage 只有 token 数）→ 计费口径由调用方兜底（Usage=nil 时
// creditsConsumed 返回 0，observability.go:48 已有空保护）。

package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ps2api/internal/store"
)

const (
	// ToolsetsAppVersion 对齐 12.29.x 桌面端（toolsets 功能上线版本）。
	ToolsetsAppVersion = "12.29.2-260923-0231"
	// toolsetsRateLimitRetries 是 429 / upstream_unavailable 的退避重试次数（含首发的总尝试 = 1+retries）。
	toolsetsRateLimitRetries = 2
)

// ToolsetsModelMap 对外模型名 → 上游 toolsets 路由名（三段式 <name>/anthropic/anthropic-messages）。
var ToolsetsModelMap = map[string]string{
	"claude-opus-5":   "claude-opus-5/anthropic/anthropic-messages",
	"claude-sonnet-5": "claude-sonnet-5/anthropic/anthropic-messages",
}

// IsToolsetsModel 报告模型名是否走 toolsets 端点。
func IsToolsetsModel(model string) bool {
	_, ok := ToolsetsModelMap[strings.ToLower(strings.TrimSpace(model))]
	return ok
}

// ToolsetsProvider 复用 PostmanProvider 的出口基础设施（代理池/cookie jar/指纹 Transport），
// 但不走 streamInternal（协议不同、无会话粘性、无 credits usage）。
type ToolsetsProvider struct {
	base *Provider
}

// NewToolsetsProvider 基于现有 Provider 的出口设施构造。
func NewToolsetsProvider(base *Provider) *ToolsetsProvider {
	return &ToolsetsProvider{base: base}
}

func (tp *ToolsetsProvider) endpoint(tokens *Tokens) string {
	sub := tokens.WorkspaceSubdomain
	if sub == "" {
		sub = "go"
	}
	return "https://" + sub + ".postman.co/_gw/toolsets/v1/messages"
}

// buildHeaders 构造 toolsets 出站头。缺 x-pstmn-req-service: ai-toolsets 一律 404。
func (tp *ToolsetsProvider) buildHeaders(tokens *Tokens) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("x-pstmn-req-service", "ai-toolsets")
	h.Set("x-access-token", tokens.AccessToken)
	if tokens.MultiLoginToken != "" {
		h.Set("x-multi-login-token", tokens.MultiLoginToken)
	}
	h.Set("x-entity-team-id", tokens.WorkspaceID)
	h.Set("x-app-version", ToolsetsAppVersion)
	h.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Postman/"+ToolsetsAppVersion+" Electron/37.10.3 Safari/537.36")
	return h
}

// rewriteModelForUpstream 把客户端 body 里的 model 字段改写为三段式路由名。
// Anthropic body 无顺序敏感字段，parse→改→marshal 安全（unknown 字段由 map 保留）。
func rewriteModelForUpstream(raw []byte, clientModel string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("bad anthropic body: %w", err)
	}
	route, ok := ToolsetsModelMap[strings.ToLower(strings.TrimSpace(clientModel))]
	if !ok {
		return nil, fmt.Errorf("model %q is not a toolsets model", clientModel)
	}
	m["model"], _ = json.Marshal(route)
	return json.Marshal(m)
}

// rewriteModelInResponse 把上游响应里的 model 字段回写为客户端请求原名（响应直传不暴露三段式路由）。
func rewriteModelInResponse(raw []byte, clientModel string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	m["model"], _ = json.Marshal(clientModel)
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

func (tp *ToolsetsProvider) newUpstreamRequest(ctx context.Context, tokens *Tokens, body []byte) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, "POST", tp.endpoint(tokens), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	r.Header = tp.buildHeaders(tokens)
	return r, nil
}

// doWithRetry 发出请求并做限频退避。429 / upstream_unavailable 时重试 toolsetsRateLimitRetries 次。
// 首个成功响应（或非重试类错误）直接返回。
func (tp *ToolsetsProvider) doWithRetry(ctx context.Context, acc *store.Account, tokens *Tokens, body []byte, egressAttempt int) (*http.Response, error, string) {
	client, egress, viaProxy := tp.base.proxies.selectFor(acc.ID, egressAttempt)
	if !viaProxy {
		client, egress = tp.base.Client, "direct"
	}
	var lastErr error
	var lastResp *http.Response
	for attempt := 0; attempt <= toolsetsRateLimitRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * 8 * time.Second
			Trace(ctx, "toolsets.retry", map[string]interface{}{"account_id": acc.ID, "attempt": attempt, "delay_s": delay.Seconds(), "last_status": lastRespStatus(lastResp)})
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err(), egress
			}
		}
		req, err := tp.newUpstreamRequest(ctx, tokens, body)
		if err != nil {
			return nil, err, egress
		}
		tp.base.applyCookies(acc.ID, req, egress)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		// 限频/上游波动：读 body 后关闭，进入退避。
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
			resp.Body.Close()
			lastResp = resp
			lastErr = fmt.Errorf("toolsets upstream busy (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
			// upstream_unavailable 也可能以 200 + error JSON 出现，由调用方按 body 判定后回传重试。
			continue
		}
		return resp, nil, egress
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("toolsets upstream unavailable after retries")
	}
	return nil, lastErr, egress
}

func lastRespStatus(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

// Chat 非流式透传：raw 为客户端原始 Anthropic 请求体，返回上游原始 JSON（model 已回写客户端原名）。
// res 携带状态分类（Success/AuthFailed/RateLimited/RequestRejected/Error）供 Router 处理。
func (tp *ToolsetsProvider) Chat(ctx context.Context, acc *store.Account, raw []byte, clientModel string, egressAttempt int) (*Result, []byte) {
	res := &Result{}
	tokens, err := tp.base.GetTokens(acc)
	if err != nil {
		res.Error = err.Error()
		res.AuthFailed = true
		return res, nil
	}
	body, err := rewriteModelForUpstream(raw, clientModel)
	if err != nil {
		res.Error = err.Error()
		res.RequestRejected = true
		return res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err, egress := tp.doWithRetry(ctx, acc, tokens, body, egressAttempt)
	if err != nil {
		res.Error = err.Error()
		// doWithRetry 内部已把 429/503 归为重试耗尽——按限频处理让 router 退避。
		if strings.Contains(res.Error, "(429)") || strings.Contains(res.Error, "(503)") {
			res.RateLimited = true
		}
		return res, nil
	}
	defer resp.Body.Close()
	_ = egress
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		res.Error = fmt.Sprintf("toolsets auth failed (%d)", resp.StatusCode)
		res.AuthFailed = true
		return res, nil
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		res.Error = fmt.Sprintf("toolsets error (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode < 500 {
			res.RequestRejected = true
		}
		return res, nil
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		res.Error = "toolsets read error: " + err.Error()
		return res, nil
	}
	// 上游 200 也可能带 error JSON（如 upstream_unavailable）——按错误分类。
	var probe struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &probe) == nil && probe.Error != nil {
		res.Error = "toolsets error: " + probe.Error.Code + ": " + probe.Error.Message
		if probe.Error.Code == "rate_limited" {
			res.RateLimited = true
		} else if probe.Error.Code == "not_found_error" {
			res.RequestRejected = true
		}
		return res, nil
	}
	res.Success = true
	res.CompletionTokens = estimateAnthropicOutputTokens(b)
	return res, rewriteModelInResponse(b, clientModel)
}

// StreamChat 流式透传：上游 Anthropic SSE 事件逐条经 emit 回调写给客户端。
// emit(event string, data []byte) 中 event 为 SSE 事件名（message_start/content_block_delta/...），
// data 为该事件 data 行的原始 JSON 字节。首个事件回调前不向客户端写任何字节（延迟开流由调用方保证）。
// 返回的 Result.Usage 恒为 nil（端点无 credits usage），Result.Error 携带分类后错误。
func (tp *ToolsetsProvider) StreamChat(ctx context.Context, acc *store.Account, raw []byte, clientModel string, egressAttempt int, emit func(event string, data []byte) error) *Result {
	res := &Result{}
	tokens, err := tp.base.GetTokens(acc)
	if err != nil {
		res.Error = err.Error()
		res.AuthFailed = true
		return res
	}
	body, err := rewriteModelForUpstream(raw, clientModel)
	if err != nil {
		res.Error = err.Error()
		res.RequestRejected = true
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err, _ := tp.doWithRetry(ctx, acc, tokens, body, egressAttempt)
	if err != nil {
		res.Error = err.Error()
		if strings.Contains(res.Error, "(429)") || strings.Contains(res.Error, "(503)") {
			res.RateLimited = true
		}
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		res.Error = fmt.Sprintf("toolsets auth failed (%d)", resp.StatusCode)
		res.AuthFailed = true
		return res
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		res.Error = fmt.Sprintf("toolsets error (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode < 500 {
			res.RequestRejected = true
		}
		return res
	}
	// 流式透传：解析 SSE 帧（event + data），原样回调。非 event/data 行忽略。
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		Trace(ctx, "toolsets.sse", map[string]interface{}{"line": line, "account_id": acc.ID})
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || event == "" {
			continue
		}
		if err := emit(event, []byte(data)); err != nil {
			res.Error = ErrClientDisconnected
			return res
		}
		event = ""
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		res.Error = "toolsets stream read error: " + err.Error()
		return res
	}
	res.Success = true
	return res
}

// estimateAnthropicOutputTokens 从上游响应 usage 粗提 output_tokens（仅用于日志/面板展示；
// toolsets 无 credits 计费，这个数不参与 creditsConsumed）。
func estimateAnthropicOutputTokens(rawJSON []byte) int {
	var u struct {
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(rawJSON, &u) == nil {
		return u.Usage.OutputTokens
	}
	return 0
}
