// anthropic.go —— Anthropic 协议端点（POST /v1/messages）：协议类型定义、
// 与上游格式的双向转换，以及流式转发（streamAnthropic）。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ps2api/internal/provider"
)

type AnthropicReq struct {
	Model       string                   `json:"model"`
	Messages    []AnthropicMsg           `json:"messages"`
	System      interface{}              `json:"system"`
	MaxTokens   int                      `json:"max_tokens"`
	Stream      bool                     `json:"stream"`
	Temperature float64                  `json:"temperature"`
	Tools       []map[string]interface{} `json:"tools"`
	ToolChoice  interface{}              `json:"tool_choice"`
}
type AnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (s *Server) anthropic(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var ar AnthropicReq
	// —— 临时：抓取 @ 文件引用的真实 wire 结构 ——
	// 设 PS2API_DUMP_BODY=<目录> 时，把原始请求体原样落盘一份，再从字节解码。
	// 排查完删除本段即可（decode 逻辑等价于原来的流式 Decode）。
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		anthropicError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	if dir := os.Getenv("PS2API_DUMP_BODY"); dir != "" {
		fname := filepath.Join(dir, fmt.Sprintf("anthropic-body-%s.json", time.Now().Format("20060102-150405.000")))
		if werr := os.WriteFile(fname, body, 0o644); werr != nil {
			provider.Trace(r.Context(), "client.body_dump_failed", map[string]interface{}{"error": werr.Error()})
		} else {
			provider.Trace(r.Context(), "client.body_dumped", map[string]interface{}{"file": fname, "bytes": len(body)})
		}
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		anthropicError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	if ar.Model == "" || len(ar.Messages) == 0 {
		anthropicError(w, 400, "model and messages are required", "invalid_request_error")
		return
	}
	req := anthropicToOpenAI(ar)
	req.Endpoint = "anthropic"
	if err := s.resolveVisionMessages(r.Context(), req.Messages); err != nil {
		provider.Trace(r.Context(), "client.vision_failed", map[string]interface{}{"error": err.Error()})
		anthropicError(w, 400, "图片识别失败: "+err.Error(), "invalid_request_error")
		return
	}
	if kind, ok := provider.UnsupportedMediaContent(req.Messages); ok {
		provider.Trace(r.Context(), "client.unsupported_media", map[string]interface{}{"kind": kind})
		anthropicError(w, 400, unsupportedMediaMessage(kind), "invalid_request_error")
		return
	}
	if name, ok := provider.UnsupportedToolResult(req.Messages); ok {
		provider.Trace(r.Context(), "client.tool_loop_blocked", map[string]interface{}{"tool": name, "reason": "unsupported custom tool call"})
		// tool_execution_error 不在 Anthropic 的错误类型枚举内，用 invalid_request_error。
		anthropicError(w, 400, fmt.Sprintf("tool %q was not executed by the client; register a handler for this tool before retrying", name), "invalid_request_error")
		return
	}
	if ar.Stream {
		req.Stream = true
		s.streamAnthropic(w, r, &req, ar)
		return
	}
	res, _, err := s.Router.Chat(r.Context(), &req)
	if err != nil {
		// 529 overloaded_error 是 Anthropic 协议表达「上游暂时不可用」的标准方式；
		// 503 不在其状态码枚举内(400/401/403/404/413/429/500/529)，客户端只能归为未知 5xx。
		// 但会话损坏/请求内容类错误(TOOL_CALL_NOT_FOUND 等)必须返 400——同一 payload 再发一定
		// 失败，529 只会让 SDK 反复退避重试，复现 "Repeated 529 Overloaded errors"。见 routeErrorStatus。
		status, typ := routeErrorStatus(err)
		anthropicError(w, status, err.Error(), typ)
		return
	}
	jsonWrite(w, 200, openAIToAnthropic(res, ar.Model))
}
func anthropicToOpenAI(a AnthropicReq) provider.ChatRequest {
	msgs := []provider.ChatMessage{}
	if a.System != nil {
		b, _ := json.Marshal(a.System)
		msgs = append(msgs, provider.ChatMessage{Role: "system", Content: b})
	}
	for _, m := range a.Messages {
		msgs = append(msgs, anthropicMessageToOpenAI(m))
	}
	req := provider.ChatRequest{Model: normalizeModel(a.Model), Messages: msgs, Tools: mapsToInterfaces(a.Tools), ToolChoice: a.ToolChoice}
	if choice, ok := a.ToolChoice.(map[string]interface{}); ok {
		if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
			parallel := !disabled
			req.ParallelToolCalls = &parallel
		}
	}
	return req
}

// anthropicMessageToOpenAI preserves tool_use blocks in the internal
// tool_calls field so conversation fingerprints can reuse the Postman session
// when the next request carries the corresponding tool_result blocks.
func anthropicMessageToOpenAI(m AnthropicMsg) provider.ChatMessage {
	msg := provider.ChatMessage{Role: m.Role, Content: m.Content}
	if m.Role != "assistant" || len(m.Content) == 0 {
		return msg
	}
	var blocks []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Text  string          `json:"text"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return msg
	}
	var calls []provider.ToolCall
	var text []string
	for _, block := range blocks {
		switch block.Type {
		case "tool_use":
			if block.ID == "" || block.Name == "" {
				continue
			}
			args := "{}"
			if len(block.Input) > 0 && string(block.Input) != "null" {
				var input interface{}
				if json.Unmarshal(block.Input, &input) == nil {
					if b, err := json.Marshal(input); err == nil {
						args = string(b)
					}
				}
			}
			tc := provider.ToolCall{ID: block.ID, Type: "function"}
			tc.Function.Name = block.Name
			tc.Function.Arguments = args
			calls = append(calls, tc)
		case "text":
			if block.Text != "" {
				text = append(text, block.Text)
			}
		}
	}
	if len(calls) == 0 {
		return msg
	}
	if b, err := json.Marshal(calls); err == nil {
		msg.ToolCalls = b
	}
	textJSON, _ := json.Marshal(strings.Join(text, "\n"))
	msg.Content = textJSON
	return msg
}
func mapsToInterfaces(in []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}
func normalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if strings.HasPrefix(m, "claude-sonnet-4-20250514") {
		return "claude-sonnet-4-6"
	}
	if strings.HasPrefix(m, "claude-opus-4-20250514") {
		return "claude-opus-4-8"
	}
	if strings.HasPrefix(m, "claude-") && !strings.Contains(m, "4-") {
		return "claude-sonnet-4-6"
	}
	return m
}
func openAIToAnthropic(res *provider.Result, model string) map[string]interface{} {
	blocks := []map[string]interface{}{}
	if res.ReasoningContent != "" {
		blocks = append(blocks, map[string]interface{}{"type": "thinking", "thinking": res.ReasoningContent, "signature": ""})
	}
	if res.Content != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": res.Content})
	}
	for _, tc := range res.ToolCalls {
		var input interface{}
		if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
			input = map[string]interface{}{}
		}
		blocks = append(blocks, map[string]interface{}{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
	}
	stop := "end_turn"
	if len(res.ToolCalls) > 0 {
		stop = "tool_use"
	}
	return map[string]interface{}{"id": newID("msg_"), "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": res.PromptTokens, "output_tokens": res.CompletionTokens}}
}
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, ar AnthropicReq) {
	fl, ok := w.(http.Flusher)
	if !ok {
		anthropicError(w, 500, "stream unsupported", "api_error")
		return
	}
	id := newID("msg_")
	// started 表示是否已向客户端提交 SSE 响应头 + message_start。
	// 关键修复：延迟到「首个真实增量到达」时才开流，而不是联系上游前就乐观开流。
	// 这样若所有账号在产出任何输出前就被网关拦截(403)，可回退为一个干净的 HTTP 503 JSON 错误，
	// 客户端(agent 终端)据此明确停止任务；不会留下一个已发 message_start 却无 message_stop 的
	// 半截流导致终端永久挂起。
	started := false
	writeEvent := func(name string, v interface{}) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, mustJSON(v))
		fl.Flush()
	}
	ensureStarted := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		writeEvent("message_start", map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": id, "type": "message", "role": "assistant", "model": ar.Model, "content": []interface{}{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": provider.EstimateMessagesTokens(req.Messages), "output_tokens": 0}}})
	}
	thinkingOpen := false
	thinkingIndex := -1
	textOpen := false
	textIndex := -1
	nextIndex := 0
	sawTools := false
	toolIndexes := map[int]int{}
	toolOrder := []int{}
	closeThinking := func() {
		if thinkingOpen {
			writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": thinkingIndex, "delta": map[string]string{"type": "signature_delta", "signature": ""}})
			writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": thinkingIndex})
			thinkingOpen = false
		}
	}
	closeText := func() {
		if textOpen {
			writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": textIndex})
			textOpen = false
		}
	}
	res, _, err := s.Router.Stream(r.Context(), req, func(d provider.Delta) error {
		ensureStarted() // 首个增量到达才真正开流（提交 200 + message_start）
		if d.ReasoningContent != "" {
			closeText()
			if !thinkingOpen {
				thinkingIndex = nextIndex
				nextIndex++
				writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": thinkingIndex, "content_block": map[string]string{"type": "thinking", "thinking": "", "signature": ""}})
				thinkingOpen = true
			}
			writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": thinkingIndex, "delta": map[string]string{"type": "thinking_delta", "thinking": d.ReasoningContent}})
		}
		if d.Content != "" {
			closeThinking()
			if !textOpen {
				textIndex = nextIndex
				nextIndex++
				writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": textIndex, "content_block": map[string]string{"type": "text", "text": ""}})
				textOpen = true
			}
			writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": textIndex, "delta": map[string]string{"type": "text_delta", "text": d.Content}})
		}
		for _, tc := range d.ToolCalls {
			closeThinking()
			closeText()
			sawTools = true
			idx, exists := toolIndexes[tc.Index]
			if !exists {
				idx = nextIndex
				nextIndex++
				toolIndexes[tc.Index] = idx
				toolOrder = append(toolOrder, tc.Index)
			}
			name := ""
			args := ""
			if tc.Function != nil {
				name = tc.Function.Name
				args = tc.Function.Arguments
			}
			if !exists {
				writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": idx, "content_block": map[string]interface{}{"type": "tool_use", "id": tc.ID, "name": name, "input": map[string]interface{}{}}})
			}
			if args != "" {
				writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": idx, "delta": map[string]string{"type": "input_json_delta", "partial_json": args}})
			}
		}
		return nil
	})
	// 尚未产生任何输出就失败：这是「流式」请求，必须以 SSE 协议干净收尾。
	// 之前回退为 HTTP 503 JSON 的做法有缺陷——Anthropic 协议的 agent 终端已经开着
	// 流式连接在等 SSE 生命周期事件，并不把 503 JSON body 当作流终止信号，于是
	// 「一直在请求中」等不到 message_stop 而永久挂起（见 docs/403-gateway-block-failover.md §2.3 缺陷2）。
	// 修复：即便产出任何增量前就失败，也照常开流(ensureStarted 发 message_start)，
	// 随后统一走下方 error + message_delta + message_stop 的终止序列，保证终端必定收到 message_stop。
	if err != nil && !started {
		ensureStarted()
	}
	closeThinking()
	closeText()
	for _, toolIndex := range toolOrder {
		writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": toolIndexes[toolIndex]})
	}
	if err != nil {
		// 已开流后失败：补发 error 事件让流按 Anthropic 协议干净终止。
		// 认 error 事件的 SDK 会就地抛异常停止；随后的 message_stop 是给不认 error 的
		// 客户端兜底，避免等不到终止事件而永久挂起。
		//
		// 这里刻意不发 message_delta：其 stop_reason 只能取自 Anthropic 的枚举
		// (end_turn/max_tokens/stop_sequence/tool_use/pause_turn/refusal)，此前发的
		// "error" 是枚举外的值，会让类型化 SDK 解析失败；而发 end_turn 又会谎报成功完成。
		// 干脆省掉这一帧——error 事件已经把失败说清楚了。
		// 错误类型按 routeErrorStatus 分类：会话损坏/请求内容类错误发 invalid_request_error
		// 让 SDK 停止，其余上游失败沿用 overloaded_error。流式已开(200 已提交)，故只切 type。
		_, errType := routeErrorStatus(err)
		writeEvent("error", map[string]interface{}{"type": "error", "error": map[string]string{"type": errType, "message": err.Error()}})
		writeEvent("message_stop", map[string]string{"type": "message_stop"})
		return
	}
	stop := "end_turn"
	if sawTools {
		stop = "tool_use"
	}
	// output_tokens 取本次真实产出（此前硬编码 0，靠 usage 计费/展示的客户端会读到 0）。
	outputTokens := 0
	if res != nil {
		outputTokens = res.CompletionTokens
	}
	writeEvent("message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]string{"stop_reason": stop}, "usage": map[string]int{"output_tokens": outputTokens}})
	writeEvent("message_stop", map[string]string{"type": "message_stop"})
}
