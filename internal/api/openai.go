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
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		openAIError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	var req provider.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		openAIError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	req.ClientPath = r.URL.Path
	req.ClientBody = string(raw)
	req.ClientHeaders = inboundHeadersJSON(r.Header)
	if req.Model == "" || len(req.Messages) == 0 {
		openAIError(w, 400, "model and messages are required", "invalid_request_error")
		return
	}
	if err := s.resolveVisionMessages(r.Context(), req.Messages); err != nil {
		provider.Trace(r.Context(), "client.vision_failed", map[string]interface{}{"error": err.Error()})
		openAIError(w, 400, "图片识别失败: "+err.Error(), "invalid_request_error")
		return
	}
	if kind, ok := provider.UnsupportedMediaContent(req.Messages); ok {
		provider.Trace(r.Context(), "client.unsupported_media", map[string]interface{}{"kind": kind})
		openAIError(w, 400, unsupportedMediaMessage(kind), "invalid_request_error")
		return
	}
	if name, ok := provider.UnsupportedToolResult(req.Messages); ok {
		provider.Trace(r.Context(), "client.tool_loop_blocked", map[string]interface{}{"tool": name, "reason": "unsupported custom tool call"})
		openAIError(w, 400, fmt.Sprintf("tool %q was not executed by the client; register a handler for this tool before retrying", name), "invalid_request_error")
		return
	}
	req.Endpoint = "openai"
	if req.Stream {
		s.streamOpenAI(w, r, &req)
		return
	}
	res, _, err := s.Router.Chat(r.Context(), &req)
	if err != nil {
		openAIError(w, upstreamErrorStatus(err), err.Error(), "service_unavailable")
		return
	}
	jsonWrite(w, 200, openAIResponse(res, req.Model))
}
func (s *Server) streamOpenAI(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) {
	fl, ok := w.(http.Flusher)
	if !ok {
		openAIError(w, 500, "stream unsupported", "server_error")
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
		openAIError(w, upstreamErrorStatus(err), err.Error(), "service_unavailable")
		return
	}
	if err != nil {
		// 已开流后失败：发一帧 error 后直接关闭连接，**不发 data: [DONE]**。
		// [DONE] 的语义是「流正常完成」——发了它，SDK 会把前面的 error 帧当作可忽略的
		// 中间数据、认定本轮成功但内容为空，于是 agent 不报错、直接进入下一轮。
		// 这正是「网关失败但客户端不停止」的直接原因。省掉 [DONE] 后，连接 EOF 即流终止，
		// SDK 读到 error 帧就抛异常停止。
		_ = sse(w, fl, map[string]interface{}{"error": map[string]interface{}{
			"message": err.Error(), "type": "service_unavailable", "param": nil, "code": nil,
		}})
		return
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
