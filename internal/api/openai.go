// openai.go —— OpenAI 协议端点（POST /v1/chat/completions）：请求处理、
// 流式转发（streamOpenAI）与上游响应到 OpenAI 格式的转换。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"ps2api/internal/provider"
)

func (s *Server) openAI(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var req provider.ChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		jsonError(w, 400, err.Error(), "invalid_request")
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		jsonError(w, 400, "model and messages are required", "invalid_request")
		return
	}
	if name, ok := provider.UnsupportedToolResult(req.Messages); ok {
		provider.Trace(r.Context(), "client.tool_loop_blocked", map[string]interface{}{"tool": name, "reason": "unsupported custom tool call"})
		jsonError(w, 400, fmt.Sprintf("tool %q was not executed by the client; register a handler for this tool before retrying", name), "tool_execution_error")
		return
	}
	req.Endpoint = "openai"
	if req.Stream {
		s.streamOpenAI(w, r, &req)
		return
	}
	res, _, err := s.Router.Chat(r.Context(), &req)
	if err != nil {
		jsonError(w, 503, err.Error(), "provider_error")
		return
	}
	jsonWrite(w, 200, openAIResponse(res, req.Model))
}
func (s *Server) streamOpenAI(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) {
	fl, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, 500, "stream unsupported", "internal_error")
		return
	}
	id := newID("chatcmpl-")
	created := nowUnix()
	// started：延迟提交 SSE 响应头到首个增量到达。失败若发生在任何输出之前，可回退为干净的
	// HTTP 503 JSON 错误，而不是一个空的 SSE 流，便于调用方明确识别失败。
	started := false
	ensureStarted := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	}
	emit := func(d provider.Delta) error {
		ensureStarted()
		chunk := map[string]interface{}{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []interface{}{map[string]interface{}{"index": 0, "delta": deltaMap(d), "finish_reason": nil}}}
		if d.HasFinish {
			chunk["choices"] = []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{}, "finish_reason": d.FinishReason}}
		}
		return sse(w, fl, chunk)
	}
	_, _, err := s.Router.Stream(r.Context(), req, emit)
	if err != nil && !started {
		jsonError(w, 503, err.Error(), "provider_error")
		return
	}
	if err != nil {
		_ = sse(w, fl, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	fl.Flush()
}
func deltaMap(d provider.Delta) map[string]interface{} {
	m := map[string]interface{}{}
	if d.Content != "" {
		m["content"] = d.Content
	}
	if d.ReasoningContent != "" {
		m["reasoning_content"] = d.ReasoningContent
	}
	if len(d.ToolCalls) > 0 {
		m["tool_calls"] = d.ToolCalls
	}
	return m
}
func openAIResponse(res *provider.Result, model string) map[string]interface{} {
	// OpenAI 规范：存在 tool_calls 时 content 必须为 null。
	msg := map[string]interface{}{"role": "assistant"}
	if len(res.ToolCalls) == 0 {
		msg["content"] = res.Content
	} else {
		msg["content"] = nil
	}
	if res.ReasoningContent != "" {
		msg["reasoning_content"] = res.ReasoningContent
	}
	if len(res.ToolCalls) > 0 {
		msg["tool_calls"] = res.ToolCalls
	}
	finish := "stop"
	if len(res.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	return map[string]interface{}{"id": newID("chatcmpl-"), "object": "chat.completion", "created": nowUnix(), "model": model, "choices": []interface{}{map[string]interface{}{"index": 0, "message": msg, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": res.PromptTokens, "completion_tokens": res.CompletionTokens, "total_tokens": res.PromptTokens + res.CompletionTokens}}
}
