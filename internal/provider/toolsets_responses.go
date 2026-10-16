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

// responsesAllowedItemContentTypes 是 Responses input item content 各 role 的合法类型
// （2026-10-15 gateway 实测）：user 只认 input_text/input_image；assistant 只认
// output_text —— assistant 带任何 input_text（含空文本）整请求 503 upstream_unavailable
// （不是 400！），是「发布后 503/429」的元凶。B4/B8/B12 用例矩阵验证。
var responsesAssistantContentTextTypes = map[string]string{
	"input_text": "output_text", // 客户端（自研 CLI 等）把历史 assistant 回复按 input_text 回传
}

// responsesInvalidItemError 是上游对「assistant 消息带 input_text」的伪装错误体：
// 503 + upstream_unavailable。按内容错误处理（换号无用），不进限频重试。
const responsesInvalidItemHint = "upstream_unavailable"

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

// sanitizeResponsesInput 把 input[] message item 清洗为上游校验器接受的形状
// （2026-10-15 gateway 实测矩阵）：
//   - assistant content 的 input_text → output_text（assistant+input_text 回 503
//     upstream_unavailable 伪装错误，含空文本）
//   - message item 的 id 剥掉（客户端回传的 msg_01a13e55-... 服务器 id 上游不认，
//     "input" does not match any of the allowed types；响应里的 id 上游自己生成）
//   - internal_chat_message_metadata_passthrough（Codex 私有字段）剥掉，同样毒
//   - status 字段剥掉（上游 "input[N]" does not match any of the allowed types）
//
// developer role 与 content 的 input_text（user/developer）实测可过，不动。
func sanitizeResponsesInput(raw json.RawMessage) (json.RawMessage, bool, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw, false, nil // input 是字符串等非 item 形态，无需处理
	}
	changed := false
	for _, it := range items {
		// message item 毒字段：id / internal_chat_message_metadata_passthrough / status / phase。
		// 上游校验器对 message item 的未知字段极苛刻（G1/D2/F1/F2 矩阵实测：id 与
		// passthrough 各自单独都能炸掉整个 input；phase="final_answer" 是 Codex 标记
		// 最终回答的字段，2026-10-15 15:29 日志 8309-8311 实测同样 400）。
		for _, poison := range []string{"id", "internal_chat_message_metadata_passthrough", "status", "phase"} {
			if _, ok := it[poison]; ok {
				delete(it, poison)
				changed = true
			}
		}
		roleRaw, ok := it["role"]
		if !ok {
			continue
		}
		var role string
		if json.Unmarshal(roleRaw, &role) != nil || role != "assistant" {
			continue
		}
		cRaw, ok := it["content"]
		if !ok {
			continue
		}
		// content 为纯字符串：easy-input 形态，实测可过，不动。
		var parts []map[string]json.RawMessage
		if json.Unmarshal(cRaw, &parts) != nil {
			continue
		}
		itemChanged := false
		for i, p := range parts {
			typRaw, ok := p["type"]
			if !ok {
				continue
			}
			var typ string
			if json.Unmarshal(typRaw, &typ) != nil {
				continue
			}
			if mapped, hit := responsesAssistantContentTextTypes[typ]; hit {
				p["type"], _ = json.Marshal(mapped)
				parts[i] = p
				itemChanged = true
			}
		}
		if itemChanged {
			it["content"], _ = json.Marshal(parts)
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(items)
	if err != nil {
		return raw, false, err
	}
	return out, true, nil
}

// flattenNamespaceTools 把 tools[] 里的 namespace 工具组（Codex multi_agent / MCP 服务
// 分组形态 {type:"namespace",name,tools:[function...]}）拍平为顶层 function：
// name = <namespace>__<tool>（与 Codex 平铺 MCP 命名 mcp__server__tool 同风格，上游实测
// 200 接受）。namespace 类型本身上游不认（"tools[N]" does not match any of the allowed
// types），直接剥会让子代理/MCP 功能静默消失，拍平零功能损失。
// web_search 类型同样上游不认（external_web_access=false 本来就禁外网，剥掉无损失）。
// 返回 (新 tools JSON, 是否有改动, 错误)。
func flattenNamespaceTools(raw json.RawMessage) (json.RawMessage, bool, error) {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw, false, nil // tools 非数组等异常形态，交上游报错
	}
	hasSpecial := false
	for _, t := range tools {
		typRaw, ok := t["type"]
		if !ok {
			continue
		}
		var typ string
		if json.Unmarshal(typRaw, &typ) == nil && (typ == "namespace" || typ == "web_search") {
			hasSpecial = true
			break
		}
	}
	if !hasSpecial {
		return raw, false, nil
	}
	var out []map[string]interface{}
	for _, t := range tools {
		typRaw, _ := t["type"]
		var typ string
		_ = json.Unmarshal(typRaw, &typ)
		switch typ {
		case "namespace":
			var nsName string
			_ = json.Unmarshal(t["name"], &nsName)
			var inner []map[string]json.RawMessage
			if json.Unmarshal(t["tools"], &inner) == nil {
				for _, st := range inner {
					fn := mustLooseMap(st)
					if fn == nil {
						continue
					}
					var subName string
					_ = json.Unmarshal(st["name"], &subName)
					var desc string
					_ = json.Unmarshal(st["description"], &desc)
					fn["name"] = nsName + "__" + subName
					fn["description"] = "[" + nsName + "] " + desc
					out = append(out, fn)
				}
			}
		case "web_search":
			// 剥掉：上游无此类型；external_web_access=false 时本就无外网能力。
		default:
			out = append(out, mustLooseMap(t))
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw, false, err
	}
	return b, true, nil
}

// mustLooseMap 把 RawMessage map 转成 interface{} map（拍平重组 tools 需要可变形状）。
func mustLooseMap(m map[string]json.RawMessage) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		var val interface{}
		if err := json.Unmarshal(v, &val); err != nil {
			return nil
		}
		out[k] = val
	}
	return out
}

// rewriteResponsesForUpstream 把客户端 Responses body 适配为上游形状：
// model → 三段式路由名；max_output_tokens 夹紧；input[] 清洗（见 sanitizeResponsesInput）；
// tools[].name 改写 + namespace 工具组拍平（见 flattenNamespaceTools）。
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
		// namespace/web_search 工具组先拍平/剥除（上游不认这两种 type，见 flattenNamespaceTools），
		// 拍平产生的 <ns>__<tool> 名随后走同一套 toolsetsToolNameRe 改写管线。
		if flat, flChanged, err := flattenNamespaceTools(toolsRaw); err != nil {
			return nil, err
		} else if flChanged {
			toolsRaw = flat
			m["tools"] = flat
		}
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
	if inputRaw, ok := m["input"]; ok {
		fixed, changed, err := sanitizeResponsesInput(inputRaw)
		if err != nil {
			return nil, err
		}
		if changed {
			m["input"] = fixed
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
// 限频是出口 IP 维度（2026-10-15 回放实验实证）：每次重试递增 egressAttempt 轮换出口。
func (tp *ToolsetsProvider) doWithRetryResponses(ctx context.Context, acc *store.Account, tokens *Tokens, body []byte, egressAttempt int) (*http.Response, error, string) {
	var lastErr error
	var lastResp *http.Response
	for attempt := 0; attempt <= toolsetsRateLimitRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * 8 * time.Second
			Trace(ctx, "toolsets-responses.retry", map[string]interface{}{"account_id": acc.ID, "attempt": attempt, "delay_s": delay.Seconds(), "last_status": lastRespStatus(lastResp)})
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err(), ""
			}
		}
		client, egress, viaProxy := tp.base.proxies.selectFor(acc.ID, egressAttempt+attempt)
		if !viaProxy {
			client, egress = tp.base.Client, "direct"
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
		// CF cookie 保温（与 messages 端 doWithRetry 同口径）：toolsets 响应的 Set-Cookie
		// 按账号+出口记入 cookie jar，下次请求自动回带。
		tp.base.cookies.remember(acc.ID, req.URL, egress, resp.Cookies())
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
			resp.Body.Close()
			// 503 + upstream_unavailable 多为内容问题伪装（assistant+input_text，换号无用）——
			// 不进退避重试，直接把响应交上层按 body 分类（RequestRejected）。
			if resp.StatusCode == 503 && strings.Contains(string(b), responsesInvalidItemHint) {
				synth := &http.Response{StatusCode: resp.StatusCode, Body: io.NopCloser(strings.NewReader(string(b))), Request: req}
				return synth, nil, egress
			}
			lastResp = resp
			lastErr = fmt.Errorf("%w (%d): %s", errToolsetsBusy, resp.StatusCode, strings.TrimSpace(string(b)))
			continue
		}
		return resp, nil, egress
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w after retries", errToolsetsBusy)
	}
	return nil, lastErr, ""
}

// classifyResponsesErrorJSON 是 OpenAI-Responses /v1/responses toolsets 端点的 error JSON
// 分类（见共享实现 classifyToolsetsErrorJSON）。upstream_unavailable 大多是内容问题伪装
// （assistant+input_text → 503 + 该 code，2026-10-15 实测矩阵）：内容错误换号无用，
// 归 RequestRejected 跳过限频重试；只有明确 rate_limited 才算限频。
func (tp *ToolsetsProvider) classifyResponsesErrorJSON(res *Result, b []byte) bool {
	return tp.classifyToolsetsErrorJSON(res, b, "toolsets responses error: ", true)
}

// ChatResponses 非流式透传：raw 为客户端原始 OpenAI Responses 请求体，返回上游原始 JSON。
func (tp *ToolsetsProvider) ChatResponses(ctx context.Context, acc *store.Account, raw []byte, clientModel string, egressAttempt int) (*Result, []byte) {
	res := &Result{}
	tokens, err := tp.base.GetTokens(acc)
	if err != nil {
		res.Error = err.Error()
		res.Kind = KindAuthFailed
		return res, nil
	}
	body, err := tp.rewriteResponsesForUpstream(raw, clientModel)
	if err != nil {
		res.Error = err.Error()
		res.Kind = KindRequestRejected
		return res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err, egress := tp.doWithRetryResponses(ctx, acc, tokens, body, egressAttempt)
	res.Diag.Egress = egress
	res.Diag.RequestBytes = len(body)
	res.Diag.UpstreamBody = string(body)
	res.Diag.UpstreamURL = tp.responsesEndpoint(tokens)
	if err != nil {
		res.Error = err.Error()
		if errors.Is(err, errToolsetsBusy) {
			res.Kind = KindRateLimited
		}
		return res, nil
	}
	defer resp.Body.Close()
	res.Diag.UpstreamHeaders = headersToJSON(resp.Request.Header)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		res.Error = fmt.Sprintf("toolsets responses auth failed (%d)", resp.StatusCode)
		res.Kind = KindAuthFailed
		return res, nil
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		// 上游把内容问题伪装成 503+upstream_unavailable（assistant+input_text）——先按
		// error JSON 分类（RequestRejected 跳过换号重试），兜不住再走通用错误路径。
		if tp.classifyResponsesErrorJSON(res, b) {
			return res, nil
		}
		res.Error = fmt.Sprintf("toolsets responses error (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode < 500 {
			res.Kind = KindRequestRejected
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
		res.Kind = KindAuthFailed
		return res
	}
	body, err := tp.rewriteResponsesForUpstream(raw, clientModel)
	if err != nil {
		res.Error = err.Error()
		res.Kind = KindRequestRejected
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err, egress := tp.doWithRetryResponses(ctx, acc, tokens, body, egressAttempt)
	res.Diag.Egress = egress
	res.Diag.RequestBytes = len(body)
	res.Diag.UpstreamBody = string(body)
	res.Diag.UpstreamURL = tp.responsesEndpoint(tokens)
	if err != nil {
		res.Error = err.Error()
		if errors.Is(err, errToolsetsBusy) {
			res.Kind = KindRateLimited
		}
		return res
	}
	defer resp.Body.Close()
	res.Diag.UpstreamHeaders = headersToJSON(resp.Request.Header)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		res.Error = fmt.Sprintf("toolsets responses auth failed (%d)", resp.StatusCode)
		res.Kind = KindAuthFailed
		return res
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		// 同 ChatResponses：503+upstream_unavailable 先按 error JSON 分类（内容问题 →
		// RequestRejected），避免当限频换号把号池打挂。
		if tp.classifyResponsesErrorJSON(res, b) {
			return res
		}
		res.Error = fmt.Sprintf("toolsets responses error (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
		if resp.StatusCode < 500 {
			res.Kind = KindRequestRejected
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
		res.Kind = KindRateLimited
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
