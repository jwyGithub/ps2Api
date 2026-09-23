package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"ps2api/internal/provider"
)

// Codex 客户端 exec custom 工具的双向翻译逻辑见 codex_exec.go
// (codexExecDeclared / execMappable / execInputFor / execInputToArgs 等)。

// Responses API 适配层。Codex 新版只支持 wire_api="responses"(/v1/responses),
// 不再支持 chat/completions。本文件把 Responses 请求转成内部 ChatRequest 走同一条
// Router 管道,再把结果以 Responses SSE 事件流回写——与 anthropic 适配层同构,
// 复用 tool-group 回传机制(call_id -> groupID),让 desktop 分支的 executeShellCommand
// 能经 Codex 的 MCP 执行并闭环。
//
// 实现 Codex 实际使用的子集:function 工具 + 文本 + function_call 往返 + reasoning 输出。
// gpt-5.6-sol 的思考内容(Delta.ReasoningContent)以 reasoning 输出项的 summary 形式回写,
// 在 Codex 里可见。回写的 reasoning 项不带 encrypted_content,所以 Codex 不会把它带回下一轮
// ——无妨:Postman 侧靠 conversationId 维持思考上下文,不依赖客户端回传。入站的 reasoning 项直接丢弃。
// 未实现:图片输入(input_image)——Codex 缺它仍能工作,需要时再补。

type ResponsesReq struct {
	Model             string                   `json:"model"`
	Instructions      string                   `json:"instructions"`
	Input             json.RawMessage          `json:"input"`
	Tools             []map[string]interface{} `json:"tools"`
	ToolChoice        interface{}              `json:"tool_choice"`
	ParallelToolCalls *bool                    `json:"parallel_tool_calls"`
	Stream            bool                     `json:"stream"`
	// OutputConfig 承载 output_config.effort（思考强度 high/medium/low），透传给 devModeOptions.thinkingLevel。
	OutputConfig map[string]interface{} `json:"output_config"`
	// Reasoning 是 OpenAI Responses 标准的思考强度字段（reasoning.effort），output_config 缺省时回退到它。
	Reasoning map[string]interface{} `json:"reasoning"`
}

// respInputItem 是 input 数组里的一项(message / function_call / function_call_output / reasoning)。
type respInputItem struct {
	Type      string                   `json:"type"`
	Role      string                   `json:"role"`
	Content   json.RawMessage          `json:"content"`
	CallID    string                   `json:"call_id"`
	Name      string                   `json:"name"`
	Arguments string                   `json:"arguments"`
	Output    json.RawMessage          `json:"output"`
	Input     string                   `json:"input"` // custom_tool_call 的原始 input(自由文本)
	Tools     []map[string]interface{} `json:"tools"` // additional_tools 项携带的工具声明(namespace 嵌套)
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxChatBody))
	if err != nil {
		openAIError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	var rr ResponsesReq
	if err := json.Unmarshal(raw, &rr); err != nil {
		openAIError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	if rr.Model == "" {
		openAIError(w, 400, "model is required", "invalid_request_error")
		return
	}
	// 必须扫原始 rr.Input：responsesToOpenAI 走 extractResponsesText 只取 .text 字段，
	// input_image / input_file 在转换那一步就被丢掉了，转成 ChatMessage 之后查不到。
	if s.Vision.Enabled() {
		if resolved, changed, err := s.Vision.ResolveMedia(r.Context(), rr.Input); err != nil {
			provider.Trace(r.Context(), "client.vision_failed", map[string]interface{}{"error": err.Error()})
			openAIError(w, 400, "图片识别失败: "+err.Error(), "invalid_request_error")
			return
		} else if changed {
			rr.Input = resolved
		}
	}
	if kind, ok := provider.UnsupportedMediaInJSON(rr.Input); ok {
		provider.Trace(r.Context(), "client.unsupported_media", map[string]interface{}{"kind": kind})
		openAIError(w, 400, unsupportedMediaMessage(kind), "invalid_request_error")
		return
	}
	req, customNames := responsesToOpenAI(rr)
	req.Endpoint = "openai"
	req.ClientPath = r.URL.Path
	req.ClientBody = string(raw)
	req.ClientHeaders = inboundHeadersJSON(r.Header)
	if len(req.Messages) == 0 {
		openAIError(w, 400, "input is required", "invalid_request_error")
		return
	}
	if name, ok := provider.UnsupportedToolResult(req.Messages); ok {
		provider.Trace(r.Context(), "client.tool_loop_blocked", map[string]interface{}{"tool": name, "reason": "unsupported custom tool call"})
		openAIError(w, 400, fmt.Sprintf("tool %q was not executed by the client; register a handler for this tool before retrying", name), "invalid_request_error")
		return
	}
	// exec custom tool 探测:客户端声明了 exec(type:custom)才把原生工具翻译成 custom_tool_call。
	execMode := codexExecDeclared(rr.Tools, rr.Input) || codexExecForce
	if rr.Stream {
		s.streamResponses(w, r, &req, execMode, customNames)
		return
	}
	res, _, err := s.Router.Chat(r.Context(), &req)
	s.chargeKey(r.Context(), res) // API Key 用量回写（credits×倍率）
	if err != nil {
		openAIError(w, upstreamErrorStatus(err), err.Error(), "service_unavailable")
		return
	}
	// 上游回吐的工具名可能被 thirdParty 机制加了 namespace 前缀，渲染前先纠回裸名。
	registered := registeredToolNames(req.Tools)
	for i := range res.ToolCalls {
		res.ToolCalls[i].Function.Name = stripUpstreamToolPrefix(res.ToolCalls[i].Function.Name, registered)
	}
	jsonWrite(w, 200, responsesObject(res, req.Model, "completed", execMode, customNames))
}

// responsesToOpenAI 把 Responses 请求转为内部 ChatRequest，并返回 custom(自由文本)工具名集合。
func responsesToOpenAI(rr ResponsesReq) (provider.ChatRequest, map[string]bool) {
	var msgs []provider.ChatMessage
	if rr.Instructions != "" {
		b, _ := json.Marshal(rr.Instructions)
		msgs = append(msgs, provider.ChatMessage{Role: "system", Content: b})
	}
	// input 可以是纯字符串,或 item 数组。
	var asString string
	var items []respInputItem
	if json.Unmarshal(rr.Input, &asString) == nil {
		b, _ := json.Marshal(asString)
		msgs = append(msgs, provider.ChatMessage{Role: "user", Content: b})
	} else {
		_ = json.Unmarshal(rr.Input, &items)
		for _, it := range items {
			if m, ok := respItemToMessage(it); ok {
				msgs = append(msgs, m)
			}
		}
	}
	// 合并顶层 tools 与 additional_tools 项（codex 的工具声明所在地），custom 工具桥接成 function 形状。
	tools, customNames := flattenResponsesTools(rr.Tools, items)
	req := provider.ChatRequest{
		Model:             normalizeModel(rr.Model),
		Messages:          msgs,
		Tools:             tools,
		ToolChoice:        rr.ToolChoice,
		ParallelToolCalls: rr.ParallelToolCalls,
		OutputConfig:      rr.OutputConfig,
	}
	// output_config 缺省时回退到标准的 reasoning.effort。
	if req.OutputConfig == nil && rr.Reasoning != nil {
		req.OutputConfig = rr.Reasoning
	}
	return req, customNames
}

func respItemToMessage(it respInputItem) (provider.ChatMessage, bool) {
	switch it.Type {
	case "", "message":
		role := it.Role
		if role == "developer" {
			role = "system"
		}
		text := extractResponsesText(it.Content)
		b, _ := json.Marshal(text)
		return provider.ChatMessage{Role: role, Content: b}, role != ""
	case "function_call":
		call := provider.ToolCall{ID: it.CallID, Type: "function"}
		call.Function.Name = it.Name
		call.Function.Arguments = it.Arguments
		if call.Function.Arguments == "" {
			call.Function.Arguments = "{}"
		}
		tc, _ := json.Marshal([]provider.ToolCall{call})
		empty, _ := json.Marshal("")
		return provider.ChatMessage{Role: "assistant", Content: empty, ToolCalls: tc}, it.CallID != ""
	case "function_call_output":
		// output 可能是字符串或结构化;统一取文本作为 tool 结果内容。
		content := extractResponsesText(it.Output)
		b, _ := json.Marshal(content)
		return provider.ChatMessage{Role: "tool", ToolCallID: it.CallID, Content: b}, it.CallID != ""
	case "custom_tool_call":
		// 客户端回显的自由文本工具调用 → 按原名还原 assistant tool_call（对齐 raycast2api：
		// arguments = JSON 编码的 input）。闭环靠 call_id→groupID(nativeToolResponse 只看 ID)，
		// 名字只影响折叠历史的可读性；LookupConversation 靠裸 user 前缀兜底命中，不受影响。
		call := provider.ToolCall{ID: it.CallID, Type: "function"}
		call.Function.Name = it.Name
		b, _ := json.Marshal(it.Input)
		call.Function.Arguments = string(b)
		tc, _ := json.Marshal([]provider.ToolCall{call})
		empty, _ := json.Marshal("")
		return provider.ChatMessage{Role: "assistant", Content: empty, ToolCalls: tc}, it.CallID != ""
	case "custom_tool_call_output":
		// 客户端执行 exec 的结果 → tool 结果消息,按 call_id 走 nativeToolResponse 回传。
		content := extractResponsesText(it.Output)
		b, _ := json.Marshal(content)
		return provider.ChatMessage{Role: "tool", ToolCallID: it.CallID, Content: b}, it.CallID != ""
	default:
		// reasoning 等未支持项:跳过。
		return provider.ChatMessage{}, false
	}
}

// extractResponsesText 从 Responses 的 content/output 字段抽出纯文本。
// 支持:纯字符串;[{type:"input_text"/"output_text"/"text", text}] 数组;[{...}] 里的字符串。
func extractResponsesText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var out string
		for _, b := range blocks {
			out += b.Text
		}
		return out
	}
	return ""
}

// 流式实现见 responses_stream.go(streamResponses:把内部 Delta 流转成 Responses SSE 事件)。

// responsesObject 构造非流式的 Responses 响应体(output 数组 + usage)。
// customNames 是本轮声明的 custom(自由文本)工具名集合：模型调用这些名字时渲染成
// custom_tool_call，其余走 function_call；execMode 下 executeShellCommand/readFile
// 仍翻译成 exec custom_tool_call 兜底。
func responsesObject(res *provider.Result, model, status string, execMode bool, customNames map[string]bool) map[string]interface{} {
	var output []interface{}
	if res.ReasoningContent != "" {
		output = append(output, map[string]interface{}{
			"id": newID("rs_"), "type": "reasoning",
			"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": res.ReasoningContent}},
		})
	}
	if res.Content != "" {
		output = append(output, map[string]interface{}{
			"id": newID("msg_"), "type": "message", "status": "completed", "role": "assistant",
			"content": []interface{}{map[string]interface{}{"type": "output_text", "text": res.Content}},
		})
	}
	for _, tc := range res.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		if execMode && execMappable(tc.Function.Name) {
			if input, ok := execInputFor(tc.Function.Name, args); ok {
				output = append(output, map[string]interface{}{
					"id": newID("ctc_"), "type": "custom_tool_call", "status": "completed",
					"call_id": tc.ID, "name": codexExecName, "input": input,
				})
				continue
			}
		}
		if customNames[tc.Function.Name] {
			output = append(output, map[string]interface{}{
				"id": newID("ctc_"), "type": "custom_tool_call", "status": "completed",
				"call_id": tc.ID, "name": tc.Function.Name, "input": customInputOf(args),
			})
			continue
		}
		output = append(output, map[string]interface{}{
			"id": newID("fc_"), "type": "function_call", "status": "completed",
			"call_id": tc.ID, "name": tc.Function.Name, "arguments": args,
		})
	}
	return map[string]interface{}{
		"id": newID("resp_"), "object": "response", "status": status, "model": model,
		"output": output,
		"usage": map[string]int{
			"input_tokens":  res.PromptTokens,
			"output_tokens": res.CompletionTokens,
			"total_tokens":  res.PromptTokens + res.CompletionTokens,
		},
	}
}
