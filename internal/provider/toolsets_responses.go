// toolsets_responses.go —— Postman「工具集」OpenAI Responses 端点透传
// （POST https://gateway.postman.com/toolsets/v1/responses，2026-10-14 12.32.2 逆向+实测）。
// 挂 gpt-6-sol / gpt-6.1-sol / gpt-6-luna / gpt-5.6-* / gpt-5.5 等 openai/openai-responses
// 路由模型。请求/响应均为标准 OpenAI Responses 协议（AI SDK createOpenAI 形态）：
//   - 入站实测支持 instructions / input(字符串或 item 数组) / tools / tool_choice /
//     reasoning.effort / stream；max_output_tokens 硬上限 8096（超限 400，与 Anthropic 端
//     max_tokens 同值——上游同一 quota 门）。
//   - 流式为标准 Responses SSE：response.created → response.output_item.added →
//     response.output_text.delta / response.function_call_arguments.delta → response.completed。
//   - 限频与 Anthropic 端点同一套（429 rate_limited / upstream_unavailable，退避重试同参数）。
// 复用 ToolsetsProvider 的出口基础设施与工具名改写映射（toolNameMap 与 Anthropic 端共享）。

package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ps2api/internal/store"
)

// ToolsetsResponsesMaxTokens 上游对 max_output_tokens 的硬上限（实测 32000 报
// "must be less than or equal to 8096"）。
const ToolsetsResponsesMaxTokens = 8096

// ToolsetsResponsesModelMap 对外模型名 → 上游三段式路由名（openai/openai-responses 协议）。
// 刻意只收 /chat 白名单外的模型（gpt-6 系）：gpt-5.5/5.6-* 等在 PostmanModelMap 里走
// /chat 管线（Agent Mode），那条链路承载 codex custom tool 桥接等特性，不能被劫持。
var ToolsetsResponsesModelMap = map[string]string{
	"gpt-6-sol":   "gpt-6-sol/openai/openai-responses",
	"gpt-6.1-sol": "gpt-6.1-sol/openai/openai-responses",
	"gpt-6-luna":  "gpt-6-luna/openai/openai-responses",
}

// IsToolsetsResponsesModel 判断模型名是否走 Responses 端点透传。
func IsToolsetsResponsesModel(model string) bool {
	_, ok := ToolsetsResponsesModelMap[strings.ToLower(strings.TrimSpace(model))]
	return ok
}

// toolsetsResponsesAllowedFields 是 Responses 出站白名单（上游严格校验器，白名单外
// 顶层字段报 invalid_request_error——与 Anthropic 端 2026-09-29 实测同坑）。
var toolsetsResponsesAllowedFields = map[string]bool{
	"model": true, "input": true, "instructions": true, "tools": true,
	"tool_choice": true, "parallel_tool_calls": true, "reasoning": true,
	"max_output_tokens": true, "stream": true, "temperature": true, "top_p": true,
	"metadata": true, "store": true, "previous_response_id": true, "truncation": true,
}

// responsesEndpoint 返回 Responses 端点 URL。
func (tp *ToolsetsProvider) responsesEndpoint(tokens *Tokens) string {
	return "https://" + tp.host(tokens) + "/toolsets/v1/responses"
}

// rewriteResponsesForUpstream 把客户端 Responses body 适配为上游形状：
// model → 三段式路由名；max_output_tokens 夹紧；tools[].name 改写（复用 Anthropic 端
// 映射，两端共享 toolNameMap——同客户端同工具名，回复回写一致）。
func (tp *ToolsetsProvider) rewriteResponsesForUpstream(raw []byte, clientModel string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("bad responses body: %w", err)
	}
	for k := range m {
		if !toolsetsResponsesAllowedFields[k] {
			delete(m, k)
		}
	}
	route, ok := ToolsetsResponsesModelMap[strings.ToLower(strings.TrimSpace(clientModel))]
	if !ok {
		return nil, fmt.Errorf("model %q is not a toolsets responses model", clientModel)
	}
	m["model"], _ = json.Marshal(route)
	if mtRaw, ok := m["max_output_tokens"]; ok {
		var mt int
		if json.Unmarshal(mtRaw, &mt) == nil && mt > ToolsetsResponsesMaxTokens {
			m["max_output_tokens"], _ = json.Marshal(ToolsetsResponsesMaxTokens)
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
	return json.Marshal(m)
}

// newUpstreamResponsesRequest 构造 Responses 端点请求（头与 messages 端点同形态）。
func (tp *ToolsetsProvider) newUpstreamResponsesRequest(ctx context.Context, tokens *Tokens, body []byte) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, "POST", tp.responsesEndpoint(tokens), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	h := tp.buildHeaders(tokens)
	// Responses 端无 anthropic-version 头（OpenAI 协议），去掉避免指纹突兀。
	h.Del("anthropic-version")
	r.Header = h
	return r, nil
}

// doWithRetryResponses 与 doWithRetry 同逻辑，仅请求目标不同（Responses 端点）。
func (tp *ToolsetsProvider) doWithRetryResponses(ctx context.Context, acc *store.Account, tokens *Tokens, body []byte, egressAttempt int) (*http.Response, error, string) {
	client, egress, viaProxy := tp.base.proxies.selectFor(acc.ID, egressAttempt)
	if !viaProxy {
		client, egress = tp.base.Client, "direct"
	}
	var lastErr error
	var lastResp *http.Response
	for attempt := 0; attempt <= toolsetsRateLimitRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * 8 * time.Second
			Trace(ctx, "toolsets-responses.retry", map[string]interface{}{"account_id": acc.ID, "attempt": attempt, "delay_s": delay.Seconds(), "last_status": lastRespStatus(lastResp)})
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err(), egress
			}
		}
		req, err := tp.newUpstreamResponsesRequest(ctx, tokens, body)
		if err != nil {
			return nil, err, egress
		}
		tp.base.applyCookies(acc.ID, req, egress)
		Trace(ctx, "toolsets-responses.upstream.request", map[string]interface{}{
			"method": req.Method, "url": req.URL.String(), "account_id": acc.ID, "egress": egress,
		})
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
			resp.Body.Close()
			lastResp = resp
			lastErr = fmt.Errorf("%w (%d): %s", errToolsetsBusy, resp.StatusCode, strings.TrimSpace(string(b)))
			continue
		}
		return resp, nil, egress
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w after retries", errToolsetsBusy)
	}
	return nil, lastErr, egress
}

// classifyResponsesErrorJSON 解析 Responses 端的 error JSON 并分类。是 error 返回 true。
func (tp *ToolsetsProvider) classifyResponsesErrorJSON(res *Result, b []byte) bool {
	var probe struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &probe) != nil || probe.Error == nil {
		return false
	}
	res.Error = "toolsets responses error: " + probe.Error.Code + ": " + probe.Error.Message
	switch probe.Error.Code {
	case "rate_limited":
		res.RateLimited = true
	case "not_found_error":
		res.RequestRejected = true
	}
	return true
}

// ChatResponses 非流式透传：raw 为客户端原始 OpenAI Responses 请求体，返回上游原始 JSON。
func (tp *ToolsetsProvider) ChatResponses(ctx context.Context, acc *store.Account, raw []byte, clientModel string, egressAttempt int) (*Result, []byte) {
	res := &Result{}
	tokens, err := tp.base.GetTokens(acc)
	if err != nil {
		res.Error = err.Error()
		res.AuthFailed = true
		return res, nil
	}
	body, err := tp.rewriteResponsesForUpstream(raw, clientModel)
	if err != nil {
		res.Error = err.Error()
		res.RequestRejected = true
		return res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err, egress := tp.doWithRetryResponses(ctx, acc, tokens, body, egressAttempt)
	res.Egress = egress
	res.RequestBytes = len(body)
	res.UpstreamBody = string(body)
	res.UpstreamURL = tp.responsesEndpoint(tokens)
	if err != nil {
		res.Error = err.Error()
		if errors.Is(err, errToolsetsBusy) {
			res.RateLimited = true
		}
		return res, nil
	}
	defer resp.Body.Close()
	res.UpstreamHeaders = headersToJSON(resp.Request.Header)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		res.Error = fmt.Sprintf("toolsets responses auth failed (%d)", resp.StatusCode)
		res.AuthFailed = true
		return res, nil
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		res.Error = fmt.Sprintf("toolsets responses error (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode < 500 {
			res.RequestRejected = true
		}
		return res, nil
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		res.Error = "toolsets responses read error: " + err.Error()
		return res, nil
	}
	if cls := tp.classifyResponsesErrorJSON(res, b); cls {
		return res, nil
	}
	res.Success = true
	res.CompletionTokens = estimateResponsesOutputTokens(b)
	return res, tp.rewriteModelInResponses(b, clientModel)
}

// rewriteModelInResponses 把上游响应 model 字段回写客户端原名（并还原 function_call 的工具名）。
func (tp *ToolsetsProvider) rewriteModelInResponses(raw []byte, clientModel string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	m["model"], _ = json.Marshal(clientModel)
	if outRaw, ok := m["output"]; ok {
		var items []map[string]json.RawMessage
		if json.Unmarshal(outRaw, &items) == nil {
			changed := false
			for i, it := range items {
				typRaw, ok := it["type"]
				if !ok {
					continue
				}
				var typ string
				if json.Unmarshal(typRaw, &typ) != nil || typ != "function_call" {
					continue
				}
				nameRaw, ok := it["name"]
				if !ok {
					continue
				}
				var name string
				if json.Unmarshal(nameRaw, &name) != nil {
					continue
				}
				if orig := tp.unmapToolName(name); orig != name {
					it["name"], _ = json.Marshal(orig)
					items[i] = it
					changed = true
				}
			}
			if changed {
				m["output"], _ = json.Marshal(items)
			}
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// StreamChatResponses 流式透传：上游 Responses SSE 事件逐条经 emit 回调（event 名 + data JSON 字节）。
// function_call 事件里的改写工具名经 unmapResponsesToolNameInEvent 还原。Result.Usage 恒 nil
// （无 credits 计量），错误分类与 Anthropic 端同口径。
func (tp *ToolsetsProvider) StreamChatResponses(ctx context.Context, acc *store.Account, raw []byte, clientModel string, egressAttempt int, emit func(event string, data []byte) error) *Result {
	res := &Result{}
	tokens, err := tp.base.GetTokens(acc)
	if err != nil {
		res.Error = err.Error()
		res.AuthFailed = true
		return res
	}
	body, err := tp.rewriteResponsesForUpstream(raw, clientModel)
	if err != nil {
		res.Error = err.Error()
		res.RequestRejected = true
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err, egress := tp.doWithRetryResponses(ctx, acc, tokens, body, egressAttempt)
	res.Egress = egress
	res.RequestBytes = len(body)
	res.UpstreamBody = string(body)
	res.UpstreamURL = tp.responsesEndpoint(tokens)
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
		res.Error = fmt.Sprintf("toolsets responses auth failed (%d)", resp.StatusCode)
		res.AuthFailed = true
		return res
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		res.Error = fmt.Sprintf("toolsets responses error (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode < 500 {
			res.RequestRejected = true
		}
		return res
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	event := ""
	emitted := 0
	for scanner.Scan() {
		line := scanner.Text()
		Trace(ctx, "toolsets-responses.sse", map[string]interface{}{"line": line, "account_id": acc.ID})
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
		// Responses SSE 的 data 自带 type 字段且与 event 名一致；上游偶尔只发 data 行。
		// function_call 相关事件还原改写过的工具名。
		if event == "response.output_item.added" || event == "response.output_item.done" {
			data = tp.unmapResponsesToolNameInEvent(data)
		}
		if err := emit(event, []byte(data)); err != nil {
			res.Error = ErrClientDisconnected
			return res
		}
		emitted++
		event = ""
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		res.Error = "toolsets responses stream read error: " + err.Error()
		return res
	}
	if emitted == 0 {
		b, _ := io.ReadAll(resp.Body)
		if tp.classifyResponsesErrorJSON(res, b) {
			return res
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			res.Error = "toolsets responses upstream returned empty stream"
		} else {
			res.Error = "toolsets responses upstream returned non-SSE body: " + truncateRunes(string(b), 500)
		}
		res.RateLimited = true
		return res
	}
	res.Success = true
	return res
}

// unmapResponsesToolNameInEvent 还原 output_item 事件里 function_call 的改写工具名。
func (tp *ToolsetsProvider) unmapResponsesToolNameInEvent(data string) string {
	var ev struct {
		Item *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(data), &ev) != nil || ev.Item == nil || ev.Item.Type != "function_call" {
		return data
	}
	orig := tp.unmapToolName(ev.Item.Name)
	if orig == ev.Item.Name {
		return data
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &m) != nil {
		return data
	}
	item, ok := m["item"]
	if !ok {
		return data
	}
	var itemMap map[string]json.RawMessage
	if json.Unmarshal(item, &itemMap) != nil {
		return data
	}
	itemMap["name"], _ = json.Marshal(orig)
	m["item"], _ = json.Marshal(itemMap)
	b, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return string(b)
}

// estimateResponsesOutputTokens 从 Responses 响应 usage 粗提 output_tokens（仅日志/面板展示）。
func estimateResponsesOutputTokens(rawJSON []byte) int {
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
