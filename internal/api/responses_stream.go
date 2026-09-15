package api

import (
	"fmt"
	"net/http"

	"ps2api/internal/provider"
)

// 本文件从 responses.go 拆出,专管 Responses API 的流式路径:把内部 Router.Stream 的
// Delta 增量流转成 Responses SSE 事件序列(reasoning -> message -> function_call/custom_tool_call)。

// streamResponses 把内部 Delta 流转成 Responses SSE 事件。
// execMode 开启后,可映射的原生工具翻译成 exec custom_tool_call(见 codex_exec.go);
// customNames 里的自由文本工具直接渲染成 custom_tool_call(原名,对齐 raycast2api)。
func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, execMode bool, customNames map[string]bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		openAIError(w, 500, "stream unsupported", "server_error")
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
		execTool               bool // 该调用翻译成 exec custom_tool_call(原生工具兜底)
		custom                 bool // 该调用是客户端声明的 custom(自由文本)工具
	}
	tools := map[int]*toolAcc{}
	var toolOrder []int
	// 上游回吐的工具名可能被 thirdParty 机制加 namespace 前缀,渲染前先纠回裸名。
	registered := registeredToolNames(req.Tools)

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

	res, _, err := s.Router.Stream(r.Context(), req, func(d provider.Delta) error {
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
				name := stripUpstreamToolPrefix(tc.Function.Name, registered)
				if customNames[name] {
					acc.custom = true // 客户端声明的 custom 工具 → custom_tool_call(原名)
				} else if execMode && execMappable(name) {
					acc.execTool = true // 翻译成 exec custom_tool_call,在收尾时一次性发出
				}
				acc.name = name
			}
			// custom_tool_call 的 input 在收尾时按整段文本一次性发出;此处只累积不 emit。
			isCustom := acc.execTool || acc.custom
			if !exists && !isCustom {
				emit("response.output_item.added", map[string]interface{}{"output_index": acc.index, "item": map[string]interface{}{"id": acc.id, "type": "function_call", "status": "in_progress", "call_id": acc.callID, "name": acc.name, "arguments": ""}})
			}
			if tc.Function != nil && tc.Function.Arguments != "" {
				acc.args += tc.Function.Arguments
				if !isCustom {
					emit("response.function_call_arguments.delta", map[string]interface{}{"item_id": acc.id, "output_index": acc.index, "delta": tc.Function.Arguments})
				}
			}
		}
		return nil
	})
	s.chargeKey(r.Context(), res) // API Key 用量回写（tokens×倍率）

	closeReasoning() // 纯思考、无正文/工具时的兜底收尾
	closeText()
	for _, idx := range toolOrder {
		acc := tools[idx]
		if acc.args == "" {
			acc.args = "{}"
		}
		if acc.custom || acc.execTool {
			name, input := acc.name, customInputOf(acc.args)
			if acc.execTool {
				translated, ok := execInputFor(acc.name, acc.args)
				if !ok {
					// 参数解析失败:退回普通 function_call(裸名),至少不丢调用。
					emit("response.output_item.added", map[string]interface{}{"output_index": acc.index, "item": map[string]interface{}{"id": acc.id, "type": "function_call", "status": "in_progress", "call_id": acc.callID, "name": acc.name, "arguments": ""}})
					emit("response.function_call_arguments.done", map[string]interface{}{"item_id": acc.id, "output_index": acc.index, "arguments": acc.args})
					item := map[string]interface{}{"id": acc.id, "type": "function_call", "status": "completed", "call_id": acc.callID, "name": acc.name, "arguments": acc.args}
					emit("response.output_item.done", map[string]interface{}{"output_index": acc.index, "item": item})
					output = append(output, item)
					continue
				}
				name, input = codexExecName, translated
			}
			// custom_tool_call 的完整事件序列(对齐 raycast2api/codex 期望):
			// added(in_progress) → input.delta → input.done → output_item.done(completed)。
			emit("response.output_item.added", map[string]interface{}{"output_index": acc.index, "item": map[string]interface{}{"id": acc.id, "type": "custom_tool_call", "status": "in_progress", "call_id": acc.callID, "name": name, "input": ""}})
			emit("response.custom_tool_call_input.delta", map[string]interface{}{"item_id": acc.id, "output_index": acc.index, "delta": input})
			emit("response.custom_tool_call_input.done", map[string]interface{}{"item_id": acc.id, "output_index": acc.index, "input": input})
			item := map[string]interface{}{"id": acc.id, "type": "custom_tool_call", "status": "completed", "call_id": acc.callID, "name": name, "input": input}
			emit("response.output_item.done", map[string]interface{}{"output_index": acc.index, "item": item})
			output = append(output, item)
			continue
		}
		emit("response.function_call_arguments.done", map[string]interface{}{"item_id": acc.id, "output_index": acc.index, "arguments": acc.args})
		item := map[string]interface{}{"id": acc.id, "type": "function_call", "status": "completed", "call_id": acc.callID, "name": acc.name, "arguments": acc.args}
		emit("response.output_item.done", map[string]interface{}{"output_index": acc.index, "item": item})
		output = append(output, item)
	}

	if err != nil {
		// 产出任何输出前就失败：还没开流，回退为干净的 HTTP 错误，调用方可明确停止任务。
		if !started {
			openAIError(w, upstreamErrorStatus(err), err.Error(), "service_unavailable")
			return
		}
		// 已开流后失败：发 response.failed 作为终止事件，让流干净收尾。
		emit("response.failed", map[string]interface{}{"response": map[string]interface{}{"id": respID, "object": "response", "status": "failed", "model": req.Model, "error": map[string]string{"code": "provider_error", "message": err.Error()}}})
		return
	}
	emit("response.completed", map[string]interface{}{"response": skeleton("completed", output)})
}
