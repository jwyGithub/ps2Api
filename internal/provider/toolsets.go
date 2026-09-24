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
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"ps2api/internal/store"
)

const (
	// ToolsetsAppVersion 对齐 12.29.x 桌面端（toolsets 功能上线版本）。
	ToolsetsAppVersion = "12.29.2-260923-0231"
	// toolsetsRateLimitRetries 是 429 / upstream_unavailable 的退避重试次数（含首发的总尝试 = 1+retries）。
	toolsetsRateLimitRetries = 2
	// ToolsetsMaxTokens 上游对 max_tokens 的硬上限（抓包实测 8096，超限 400 invalid_request_error）。
	// 客户端合法但更大的值（如 Claude Code 的 32000）在此夹紧，避免整请求被拒。
	ToolsetsMaxTokens = 8096
)

// 上游对工具名的格式校验：1-64 个字母/数字/下划线/连字符。
var toolsetsToolNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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

// errToolsetsBusy 是限频/上游波动的分类错误（429/503 重试耗尽），供调用方判
// res.RateLimited——取代此前对错误消息文本 "(429)" 的字符串匹配。
var errToolsetsBusy = errors.New("toolsets upstream busy")

// ToolsetsProvider 复用 PostmanProvider 的出口基础设施（代理池/cookie jar/指纹 Transport），
// 但不走 streamInternal（协议不同、无会话粘性、无 credits usage）。
type ToolsetsProvider struct {
	base *Provider
	// toolNameMap 是「上游改写名 → 客户端原名」的映射（出站改写时写入、响应回写时查）。
	// 进程重启后丢失——届时上游名原样透传（见 unmapToolName），续聊 tool_result 的
	// tool_use_id 仍可配对（Anthropic 按 id 不按 name 配对）。sync.Map 应对并发请求。
	toolNameMap sync.Map
}

// NewToolsetsProvider 基于现有 Provider 的出口设施构造。
func NewToolsetsProvider(base *Provider) *ToolsetsProvider {
	return &ToolsetsProvider{base: base}
}

// buildHeaders 构造 toolsets 出站头。缺 x-pstmn-req-service: ai-toolsets 一律 404。
// 桌面双 token 与 web cookie（postman.sid）两种登录态都支持（抓包实测 cookie 亦通）。
// 浏览器指纹头（sec-ch-ua*/sec-fetch-*）与 /chat 的 buildHeaders 同口径——UA 自称
// Chromium/Electron 却不发这些是 Cloudflare Bot Management 的机器人信号。
func (tp *ToolsetsProvider) buildHeaders(tokens *Tokens) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("x-pstmn-req-service", "ai-toolsets")
	if tokens.AccessToken != "" {
		h.Set("x-access-token", tokens.AccessToken)
		if tokens.MultiLoginToken != "" {
			h.Set("x-multi-login-token", tokens.MultiLoginToken)
		}
	} else if tokens.PostmanSID != "" {
		h.Set("Cookie", "postman.sid="+tokens.PostmanSID)
	}
	h.Set("x-entity-team-id", tokens.WorkspaceID)
	h.Set("x-app-version", ToolsetsAppVersion)
	h.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Postman/"+ToolsetsAppVersion+" Electron/37.10.3 Safari/537.36")
	h.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138"`)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"macOS"`)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
	h.Set("Origin", "https://"+tp.host(tokens)+"/")
	return h
}

// host 返回出站域名（不含 scheme）。web 会话必须回到自己的子域（cookie 域绑定），桌面 token 回退 go。
func (tp *ToolsetsProvider) host(tokens *Tokens) string {
	sub := tokens.WorkspaceSubdomain
	if sub == "" {
		sub = "go"
	}
	return sub + ".postman.co"
}

func (tp *ToolsetsProvider) endpoint(tokens *Tokens) string {
	return "https://" + tp.host(tokens) + "/_gw/toolsets/v1/messages"
}

// rewriteModelForUpstream 把客户端 body 适配为上游 toolsets 接受的形状。改三处：
//  1. model → 三段式路由名
//  2. max_tokens → 夹紧到 ToolsetsMaxTokens（上游硬上限，超限整请求 400）
//  3. tools[].name → 超出上游格式（1-64 字母/数字/_/-）的名字压缩改写（如 MCP 长工具名），
//     改写映射同时存入 ToolsetsProvider.toolNameMap，供响应回写原名
//
// Anthropic body 无顺序敏感字段，parse→改→marshal 安全（unknown 字段由 map 保留）。
func (tp *ToolsetsProvider) rewriteModelForUpstream(raw []byte, clientModel string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("bad anthropic body: %w", err)
	}
	route, ok := ToolsetsModelMap[strings.ToLower(strings.TrimSpace(clientModel))]
	if !ok {
		return nil, fmt.Errorf("model %q is not a toolsets model", clientModel)
	}
	m["model"], _ = json.Marshal(route)
	if mtRaw, ok := m["max_tokens"]; ok {
		var mt int
		if json.Unmarshal(mtRaw, &mt) == nil && mt > ToolsetsMaxTokens {
			m["max_tokens"], _ = json.Marshal(ToolsetsMaxTokens)
		}
	}
	if toolsRaw, ok := m["tools"]; ok {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(toolsRaw, &tools) == nil {
			changed := false
			for i, t := range tools {
				nameRaw, ok := t["name"]
				if !ok {
					continue
				}
				var name string
				if json.Unmarshal(nameRaw, &name) != nil {
					continue
				}
				if toolsetsToolNameRe.MatchString(name) {
					continue
				}
				mapped := tp.mapToolName(name)
				t["name"], _ = json.Marshal(mapped)
				tools[i] = t
				changed = true
			}
			if changed {
				m["tools"], _ = json.Marshal(tools)
			}
		}
	}
	// 续聊 messages 里回传的 tool_use / tool_result 历史同样带客户端原名，出站一并改写
	// （Anthropic 协议要求 tool_use.name 与 tool_result 的配对块 name 一致，仅单向改会 400）。
	if msgsRaw, ok := m["messages"]; ok {
		var msgs []map[string]json.RawMessage
		if json.Unmarshal(msgsRaw, &msgs) == nil {
			changed := false
			for i, msg := range msgs {
				if rewritten := tp.rewriteMsgToolNames(msg); rewritten != nil {
					msgs[i] = rewritten
					changed = true
				}
			}
			if changed {
				m["messages"], _ = json.Marshal(msgs)
			}
		}
	}
	return json.Marshal(m)
}

// rewriteMsgToolNames 改写单条 message content 里 tool_use/tool_result 块的 name。
// 无改动返回 nil（调用方据此跳过重新 marshal，保持字节级原样）。
func (tp *ToolsetsProvider) rewriteMsgToolNames(msg map[string]json.RawMessage) map[string]json.RawMessage {
	cRaw, ok := msg["content"]
	if !ok {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(cRaw, &blocks) != nil {
		return nil // content 为纯字符串等非块形态，无工具名
	}
	changed := false
	for i, b := range blocks {
		tpRaw, ok := b["type"]
		if !ok {
			continue
		}
		var typ string
		if json.Unmarshal(tpRaw, &typ) != nil || (typ != "tool_use" && typ != "tool_result") {
			continue
		}
		nameRaw, ok := b["name"]
		if !ok { // tool_result 只有 id 没有 name，配对走 id，无需改
			continue
		}
		var name string
		if json.Unmarshal(nameRaw, &name) != nil || toolsetsToolNameRe.MatchString(name) {
			continue
		}
		b["name"], _ = json.Marshal(tp.mapToolName(name))
		blocks[i] = b
		changed = true
	}
	if !changed {
		return nil
	}
	msg["content"], _ = json.Marshal(blocks)
	return msg
}

// mapToolName 把不合规工具名压缩为上游可接受的形状：保留可读前缀、非法字符替换为 _、
// 超长部分折叠为 fnv 短哈希后缀（保证唯一、可复现、且总长 ≤64）。同时记录双向映射。
func (tp *ToolsetsProvider) mapToolName(name string) string {
	// 非法字符（如 MCP 名里的点）替换为下划线。
	clean := regexp.MustCompile(`[^A-Za-z0-9_-]`).ReplaceAllString(name, "_")
	if len(clean) <= 64 {
		mapped := clean
		tp.toolNameMap.Store(mapped, name)
		return mapped
	}
	h := fnv.New64a()
	h.Write([]byte(name))
	suffix := fmt.Sprintf("_%x", h.Sum64()) // 16 hex 字符
	keep := 64 - len(suffix)
	mapped := clean[:keep] + suffix
	tp.toolNameMap.Store(mapped, name)
	return mapped
}

// unmapToolName 把上游响应里的（可能被改写过的）工具名还原为客户端原名。
// 命中映射返回原名，未命中原样返回（上游没改过或会话已重启映射丢失——原样最诚实）。
func (tp *ToolsetsProvider) unmapToolName(name string) string {
	if v, ok := tp.toolNameMap.Load(name); ok {
		return v.(string)
	}
	return name
}

// rewriteModelInResponse 把上游非流式响应回写为客户端视角：model 字段回写原名
// （不暴露三段式路由），content 里的 tool_use.name 回写出站时改写的原名。
func (tp *ToolsetsProvider) rewriteModelInResponse(raw []byte, clientModel string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	m["model"], _ = json.Marshal(clientModel)
	if cRaw, ok := m["content"]; ok {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(cRaw, &blocks) == nil {
			changed := false
			for i, b := range blocks {
				nameRaw, ok := b["name"]
				if !ok {
					continue
				}
				var name string
				if json.Unmarshal(nameRaw, &name) != nil {
					continue
				}
				if orig := tp.unmapToolName(name); orig != name {
					b["name"], _ = json.Marshal(orig)
					blocks[i] = b
					changed = true
				}
			}
			if changed {
				m["content"], _ = json.Marshal(blocks)
			}
		}
	}
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
		Trace(ctx, "toolsets.upstream.request", map[string]interface{}{
			"method": req.Method, "url": req.URL.String(), "headers": req.Header,
			"account_id": acc.ID, "egress": egress,
		})
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
			lastErr = fmt.Errorf("%w (%d): %s", errToolsetsBusy, resp.StatusCode, strings.TrimSpace(string(b)))
			// upstream_unavailable 也可能以 200 + error JSON 出现：由 Chat/StreamChat 按 body 判定。
			continue
		}
		return resp, nil, egress
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w after retries", errToolsetsBusy)
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
	body, err := tp.rewriteModelForUpstream(raw, clientModel)
	if err != nil {
		res.Error = err.Error()
		res.RequestRejected = true
		return res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err, egress := tp.doWithRetry(ctx, acc, tokens, body, egressAttempt)
	res.Egress = egress
	res.RequestBytes = len(body)
	res.UpstreamBody = string(body)
	res.UpstreamURL = tp.endpoint(tokens)
	if err != nil {
		res.Error = err.Error()
		// doWithRetry 内部已把 429/503 归为重试耗尽——按限频处理让 router 退避。
		if errors.Is(err, errToolsetsBusy) {
			res.RateLimited = true
		}
		return res, nil
	}
	defer resp.Body.Close()
	res.UpstreamHeaders = headersToJSON(resp.Request.Header)
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
	if cls := tp.classifyErrorJSON(res, b); cls {
		return res, nil
	}
	res.Success = true
	res.CompletionTokens = estimateAnthropicOutputTokens(b)
	return res, tp.rewriteModelInResponse(b, clientModel)
}

// classifyErrorJSON 尝试把 body 解析为 toolsets 的 error JSON 并分类写入 res。
// 是 error JSON 时返回 true（res 已带分类），否则 res 不变、返回 false。
func (tp *ToolsetsProvider) classifyErrorJSON(res *Result, b []byte) bool {
	var probe struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &probe) != nil || probe.Error == nil {
		return false
	}
	res.Error = "toolsets error: " + probe.Error.Code + ": " + probe.Error.Message
	switch probe.Error.Code {
	case "rate_limited":
		res.RateLimited = true
	case "not_found_error":
		res.RequestRejected = true
	}
	return true
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
	body, err := tp.rewriteModelForUpstream(raw, clientModel)
	if err != nil {
		res.Error = err.Error()
		res.RequestRejected = true
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err, egress := tp.doWithRetry(ctx, acc, tokens, body, egressAttempt)
	res.Egress = egress
	res.RequestBytes = len(body)
	res.UpstreamBody = string(body)
	res.UpstreamURL = tp.endpoint(tokens)
	if err != nil {
		res.Error = err.Error()
		if errors.Is(err, errToolsetsBusy) {
			res.RateLimited = true
		}
		return res
	}
	defer resp.Body.Close()
	res.UpstreamHeaders = headersToJSON(resp.Request.Header)
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
	emitted := 0
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
		if event == "content_block_start" {
			data = tp.unmapToolNameInEvent(data)
		}
		if err := emit(event, []byte(data)); err != nil {
			res.Error = ErrClientDisconnected
			return res
		}
		emitted++
		event = ""
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		res.Error = "toolsets stream read error: " + err.Error()
		return res
	}
	// 上游 200 也可能不带 SSE 而是 error JSON（upstream_unavailable），或直接返回空 body——
	// 两种都一个事件都没 emit。把整个 body 读回按错误 JSON 分类（rate_limited → RateLimited
	// 换号重试；其余 error code 归 RequestRejected/通用错误），否则空流对客户端表现为挂死。
	if emitted == 0 {
		b, _ := io.ReadAll(resp.Body)
		return tp.classifyNonSSEBody(res, b)
	}
	res.Success = true
	return res
}

// unmapToolNameInEvent 把流式 content_block_start 事件里 tool_use 的（可能改写过的）
// name 还原为客户端原名。非 tool_use 事件或未命中映射时原样返回。
func (tp *ToolsetsProvider) unmapToolNameInEvent(data string) string {
	var ev struct {
		ContentBlock *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if json.Unmarshal([]byte(data), &ev) != nil || ev.ContentBlock == nil || ev.ContentBlock.Type != "tool_use" {
		return data
	}
	orig := tp.unmapToolName(ev.ContentBlock.Name)
	if orig == ev.ContentBlock.Name {
		return data
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &m) != nil {
		return data
	}
	cb, ok := m["content_block"]
	if !ok {
		return data
	}
	var cbMap map[string]json.RawMessage
	if json.Unmarshal(cb, &cbMap) != nil {
		return data
	}
	cbMap["name"], _ = json.Marshal(orig)
	m["content_block"], _ = json.Marshal(cbMap)
	b, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return string(b)
}

// classifyNonSSEBody 对 200 但非 SSE（error JSON 或空）的响应做错误分类。返回前覆写 res。
func (tp *ToolsetsProvider) classifyNonSSEBody(res *Result, b []byte) *Result {
	if tp.classifyErrorJSON(res, b) {
		return res
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		res.Error = "toolsets upstream returned empty stream"
	} else {
		res.Error = "toolsets upstream returned non-SSE body: " + truncateRunes(string(b), 500)
	}
	res.RateLimited = true // 空流/未知 body 视为上游波动，让调用方换号重试
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
