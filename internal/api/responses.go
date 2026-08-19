package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"ps2api/internal/provider"
)

// codexLocalShell 开启后,把 Postman 的 executeShellCommand 工具调用翻译成 Responses API 的
// 内建 local_shell_call 项类型。前面所有路径都撞在「Codex 不执行自定义 provider 透传的
// function_call/MCP 工具」这堵墙;而 local_shell_call 是 Codex CLI 原生自带、自己执行的项类型
// (见 OpenAI docs: Local shell tool "designed to work with Codex CLI"),它不走自定义工具的
// handler 查找表,理论上能被 Codex 内建 shell 执行器直接跑。默认关(空 env),不影响其他客户端。
var codexLocalShell = os.Getenv("PS2API_CODEX_LOCAL_SHELL") != ""

// toLocalShellAction 把 executeShellCommand 的参数转成 local_shell_call 的 action 对象。
// command 用 bash -lc 包裹(对齐 Codex CLI 自身惯例),保留管道/重定向等 shell 语义。
func toLocalShellAction(argsJSON string) (map[string]interface{}, bool) {
	var a struct {
		ProjectPath  string `json:"projectPath"`
		Command      string `json:"command"`
		BlockUntilMs int    `json:"blockUntilMs"`
	}
	if json.Unmarshal([]byte(argsJSON), &a) != nil || a.Command == "" {
		return nil, false
	}
	action := map[string]interface{}{
		"type":    "exec",
		"command": []string{"bash", "-lc", a.Command},
	}
	if a.ProjectPath != "" {
		action["working_directory"] = a.ProjectPath
	}
	if a.BlockUntilMs > 0 {
		action["timeout_ms"] = a.BlockUntilMs
	}
	return action, true
}

// localShellActionToArgs 把回显的 local_shell_call.action 还原成 executeShellCommand 参数,
// 供入站历史重建(内部管道只认 executeShellCommand)。best-effort:bash -lc <cmd> 取 <cmd>,
// 否则 argv 空格拼接。精确度不影响续期——nativeToolResponse 靠 call_id→groupID 闭环,
// LookupConversation 在 assistant 消息之前的前缀即可命中。
func localShellActionToArgs(rawAction json.RawMessage) string {
	var act struct {
		Command          []string `json:"command"`
		WorkingDirectory string   `json:"working_directory"`
		TimeoutMs        int      `json:"timeout_ms"`
	}
	_ = json.Unmarshal(rawAction, &act)
	cmd := ""
	if n := len(act.Command); n >= 3 && (act.Command[0] == "bash" || act.Command[0] == "sh" || act.Command[0] == "/bin/sh") && (act.Command[1] == "-lc" || act.Command[1] == "-c") {
		cmd = act.Command[n-1]
	} else if n > 0 {
		cmd = strings.Join(act.Command, " ")
	}
	out := map[string]interface{}{"command": cmd}
	if act.WorkingDirectory != "" {
		out["projectPath"] = act.WorkingDirectory
	}
	if act.TimeoutMs > 0 {
		out["blockUntilMs"] = act.TimeoutMs
	}
	b, _ := json.Marshal(out)
	return string(b)
}

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
}

// respInputItem 是 input 数组里的一项(message / function_call / function_call_output / reasoning)。
type respInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"` // local_shell_call 的 exec 动作
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var rr ResponsesReq
	if err := json.NewDecoder(io.LimitReader(r.Body, maxChatBody)).Decode(&rr); err != nil {
		jsonError(w, 400, err.Error(), "invalid_request")
		return
	}
	if rr.Model == "" {
		jsonError(w, 400, "model is required", "invalid_request")
		return
	}
	req := responsesToOpenAI(rr)
	req.Endpoint = "openai"
	if len(req.Messages) == 0 {
		jsonError(w, 400, "input is required", "invalid_request")
		return
	}
	if name, ok := provider.UnsupportedToolResult(req.Messages); ok {
		provider.Trace(r.Context(), "client.tool_loop_blocked", map[string]interface{}{"tool": name, "reason": "unsupported custom tool call"})
		jsonError(w, 400, fmt.Sprintf("tool %q was not executed by the client; register a handler for this tool before retrying", name), "tool_execution_error")
		return
	}
	if rr.Stream {
		s.streamResponses(w, r, &req)
		return
	}
	res, _, err := s.Router.Chat(r.Context(), &req)
	if err != nil {
		jsonError(w, 503, err.Error(), "provider_error")
		return
	}
	jsonWrite(w, 200, responsesObject(res, req.Model, "completed"))
}

// responsesToOpenAI 把 Responses 请求转为内部 ChatRequest。
func responsesToOpenAI(rr ResponsesReq) provider.ChatRequest {
	var msgs []provider.ChatMessage
	if rr.Instructions != "" {
		b, _ := json.Marshal(rr.Instructions)
		msgs = append(msgs, provider.ChatMessage{Role: "system", Content: b})
	}
	// input 可以是纯字符串,或 item 数组。
	var asString string
	if json.Unmarshal(rr.Input, &asString) == nil {
		b, _ := json.Marshal(asString)
		msgs = append(msgs, provider.ChatMessage{Role: "user", Content: b})
	} else {
		var items []respInputItem
		_ = json.Unmarshal(rr.Input, &items)
		for _, it := range items {
			if m, ok := respItemToMessage(it); ok {
				msgs = append(msgs, m)
			}
		}
	}
	req := provider.ChatRequest{
		Model:             normalizeModel(rr.Model),
		Messages:          msgs,
		Tools:             mapsToInterfaces(rr.Tools),
		ToolChoice:        rr.ToolChoice,
		ParallelToolCalls: rr.ParallelToolCalls,
	}
	return req
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
	case "local_shell_call":
		// Codex 内建 shell 执行的调用回显 → 还原成内部的 executeShellCommand assistant tool_call,
		// 内部管道/nativeToolResponse 只认裸名 executeShellCommand。
		call := provider.ToolCall{ID: it.CallID, Type: "function"}
		call.Function.Name = "executeShellCommand"
		call.Function.Arguments = localShellActionToArgs(it.Action)
		tc, _ := json.Marshal([]provider.ToolCall{call})
		empty, _ := json.Marshal("")
		return provider.ChatMessage{Role: "assistant", Content: empty, ToolCalls: tc}, it.CallID != ""
	case "local_shell_call_output":
		// Codex 本机执行 shell 的结果 → tool 结果消息,按 call_id 走 nativeToolResponse 回传。
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

// ---- 流式:把内部 Delta 流转成 Responses SSE 事件 ----

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) {
	fl, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, 500, "stream unsupported", "internal_error")
		return
	}
	respID := newID("resp_")
	seq := 0
	emit := func(typ string, obj map[string]interface{}) {
		obj["type"] = typ
		obj["sequence_number"] = seq
		seq++
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, mustJSON(obj))
		fl.Flush()
	}
	skeleton := func(status string, output []interface{}) map[string]interface{} {
		return map[string]interface{}{"id": respID, "object": "response", "status": status, "model": req.Model, "output": output}
	}
	// started：延迟提交 SSE 响应头 + response.created 到首个增量到达。若产出任何输出前就失败，
	// 回退为干净的 HTTP 503 JSON 错误，避免半截流让调用方挂起。
	started := false
	ensureStarted := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		emit("response.created", map[string]interface{}{"response": skeleton("in_progress", []interface{}{})})
	}

	// 输出项累积:一个可选的 message 项 + 若干 function_call 项,按创建顺序排列。
	var output []interface{}
	nextIndex := 0

	msgID := newID("msg_")
	msgOpen := false
	msgIndex := 0
	var textBuf string

	// reasoning 输出项:思考内容以 summary_text 流式回写。reasoning 在 text/tool 之前收尾,
	// 保证 output 数组顺序为 reasoning -> message -> function_call(与真实 Responses 流一致)。
	rsID := newID("rs_")
	rsOpen := false
	rsIndex := 0
	var rsBuf string
	closeReasoning := func() {
		if !rsOpen {
			return
		}
		emit("response.reasoning_summary_text.done", map[string]interface{}{"item_id": rsID, "output_index": rsIndex, "summary_index": 0, "text": rsBuf})
		emit("response.reasoning_summary_part.done", map[string]interface{}{"item_id": rsID, "output_index": rsIndex, "summary_index": 0, "part": map[string]interface{}{"type": "summary_text", "text": rsBuf}})
		item := map[string]interface{}{"id": rsID, "type": "reasoning", "summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": rsBuf}}}
		emit("response.output_item.done", map[string]interface{}{"output_index": rsIndex, "item": item})
		output = append(output, item)
		rsOpen = false
	}

	type toolAcc struct {
		id, callID, name, args string
		index                  int
		localShell             bool // 该调用翻译成 Codex 内建 local_shell_call
	}
	tools := map[int]*toolAcc{}
	var toolOrder []int

	closeText := func() {
		if !msgOpen {
			return
		}
		emit("response.output_text.done", map[string]interface{}{"item_id": msgID, "output_index": msgIndex, "content_index": 0, "text": textBuf})
		emit("response.content_part.done", map[string]interface{}{"item_id": msgID, "output_index": msgIndex, "content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": textBuf}})
		item := map[string]interface{}{"id": msgID, "type": "message", "status": "completed", "role": "assistant", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": textBuf}}}
		emit("response.output_item.done", map[string]interface{}{"output_index": msgIndex, "item": item})
		output = append(output, item)
		msgOpen = false
	}

	_, _, err := s.Router.Stream(r.Context(), req, func(d provider.Delta) error {
		ensureStarted() // 首个增量到达才真正开流（提交 200 + response.created）
		if d.ReasoningContent != "" {
			if !rsOpen {
				rsIndex = nextIndex
				nextIndex++
				rsOpen = true
				emit("response.output_item.added", map[string]interface{}{"output_index": rsIndex, "item": map[string]interface{}{"id": rsID, "type": "reasoning", "summary": []interface{}{}}})
				emit("response.reasoning_summary_part.added", map[string]interface{}{"item_id": rsID, "output_index": rsIndex, "summary_index": 0, "part": map[string]interface{}{"type": "summary_text", "text": ""}})
			}
			rsBuf += d.ReasoningContent
			emit("response.reasoning_summary_text.delta", map[string]interface{}{"item_id": rsID, "output_index": rsIndex, "summary_index": 0, "delta": d.ReasoningContent})
		}
		if d.Content != "" {
			closeReasoning() // reasoning 在正文之前收尾
			if !msgOpen {
				msgIndex = nextIndex
				nextIndex++
				msgOpen = true
				emit("response.output_item.added", map[string]interface{}{"output_index": msgIndex, "item": map[string]interface{}{"id": msgID, "type": "message", "status": "in_progress", "role": "assistant", "content": []interface{}{}}})
				emit("response.content_part.added", map[string]interface{}{"item_id": msgID, "output_index": msgIndex, "content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": ""}})
			}
			textBuf += d.Content
			emit("response.output_text.delta", map[string]interface{}{"item_id": msgID, "output_index": msgIndex, "content_index": 0, "delta": d.Content})
		}
		for _, tc := range d.ToolCalls {
			acc, exists := tools[tc.Index]
			if !exists {
				closeReasoning() // reasoning/文本项在工具项之前收尾
				closeText()
				acc = &toolAcc{id: newID("fc_"), index: nextIndex}
				nextIndex++
				tools[tc.Index] = acc
				toolOrder = append(toolOrder, tc.Index)
			}
			if tc.ID != "" {
				acc.callID = tc.ID
			}
			if tc.Function != nil && tc.Function.Name != "" {
				if codexLocalShell && tc.Function.Name == "executeShellCommand" {
					acc.localShell = true // 翻译成 local_shell_call,在收尾时一次性发出
				}
				acc.name = tc.Function.Name
			}
			// local_shell_call 无增量 arguments 事件,整个 action 在收尾时发出;此处只累积不 emit。
			if !exists && !acc.localShell {
				emit("response.output_item.added", map[string]interface{}{"output_index": acc.index, "item": map[string]interface{}{"id": acc.id, "type": "function_call", "status": "in_progress", "call_id": acc.callID, "name": acc.name, "arguments": ""}})
			}
			if tc.Function != nil && tc.Function.Arguments != "" {
				acc.args += tc.Function.Arguments
				if !acc.localShell {
					emit("response.function_call_arguments.delta", map[string]interface{}{"item_id": acc.id, "output_index": acc.index, "delta": tc.Function.Arguments})
				}
			}
		}
		return nil
	})

	closeReasoning() // 纯思考、无正文/工具时的兜底收尾
	closeText()
	for _, idx := range toolOrder {
		acc := tools[idx]
		if acc.args == "" {
			acc.args = "{}"
		}
		if acc.localShell {
			if action, ok := toLocalShellAction(acc.args); ok {
				item := map[string]interface{}{"id": acc.id, "type": "local_shell_call", "status": "completed", "call_id": acc.callID, "action": action}
				emit("response.output_item.added", map[string]interface{}{"output_index": acc.index, "item": item})
				emit("response.output_item.done", map[string]interface{}{"output_index": acc.index, "item": item})
				output = append(output, item)
				continue
			}
			// 参数解析失败:退回普通 function_call(带 mcp 前缀名),至少不丢调用。
			emit("response.output_item.added", map[string]interface{}{"output_index": acc.index, "item": map[string]interface{}{"id": acc.id, "type": "function_call", "status": "in_progress", "call_id": acc.callID, "name": acc.name, "arguments": ""}})
		}
		emit("response.function_call_arguments.done", map[string]interface{}{"item_id": acc.id, "output_index": acc.index, "arguments": acc.args})
		item := map[string]interface{}{"id": acc.id, "type": "function_call", "status": "completed", "call_id": acc.callID, "name": acc.name, "arguments": acc.args}
		emit("response.output_item.done", map[string]interface{}{"output_index": acc.index, "item": item})
		output = append(output, item)
	}

	if err != nil {
		// 产出任何输出前就失败：还没开流，回退为干净的 HTTP 503 JSON 错误，调用方可明确停止任务。
		if !started {
			jsonError(w, 503, err.Error(), "provider_error")
			return
		}
		// 已开流后失败：发 response.failed 作为终止事件，让流干净收尾。
		emit("response.failed", map[string]interface{}{"response": map[string]interface{}{"id": respID, "object": "response", "status": "failed", "model": req.Model, "error": map[string]string{"code": "provider_error", "message": err.Error()}}})
		return
	}
	emit("response.completed", map[string]interface{}{"response": skeleton("completed", output)})
}

// responsesObject 构造非流式的 Responses 响应体(output 数组 + usage)。
func responsesObject(res *provider.Result, model, status string) map[string]interface{} {
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
		if codexLocalShell && tc.Function.Name == "executeShellCommand" {
			if action, ok := toLocalShellAction(args); ok {
				output = append(output, map[string]interface{}{
					"id": newID("fc_"), "type": "local_shell_call", "status": "completed",
					"call_id": tc.ID, "action": action,
				})
				continue
			}
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
