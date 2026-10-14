// toolsets_responses.go —— toolsets GPT 模型（gpt-6-sol/gpt-6.1-sol/gpt-6-luna/gpt-5.6-*/
// gpt-5.5 等，openai/openai-responses 路由）的 /v1/responses 原生透传。
// 客户端请求体与上游同为 OpenAI Responses 协议，直接透传零转换（仅 model 改写为三段式
// 路由名、max_output_tokens 夹紧、工具名压缩改写——见 provider/toolsets_responses.go）。
// 端点无 credits usage 计量 → 不参与 credits 计费，请求日志直写存储保持面板可见。
// 处理结构与 toolsets.go（Anthropic 端）同构：选号轮询 + 3 次换号重试 + 错误分类。

package api

import (
	"fmt"
	"net/http"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// handleToolsetsResponses 是 /v1/responses 对 toolsets GPT 模型的入口。
// raw 为客户端原始 body 字节（透传到上游）。
func (s *Server) handleToolsetsResponses(w http.ResponseWriter, r *http.Request, raw []byte, model string, stream bool) {
	lc := toolsetsLogContext{clientBody: string(raw), clientHeaders: inboundHeadersJSON(r.Header)}
	if !stream {
		s.toolsetsResponsesNonStream(w, r, raw, model, lc)
		return
	}
	s.toolsetsResponsesStream(w, r, raw, model, lc)
}

// toolsetsResponsesNonStream 非流式：透传上游 JSON 原样返回。
func (s *Server) toolsetsResponsesNonStream(w http.ResponseWriter, r *http.Request, raw []byte, model string, lc toolsetsLogContext) {
	excluded := map[int64]bool{}
	var lastRes *provider.Result
	for attempt := 0; attempt < 3; attempt++ {
		acc, err := s.pickToolsetsAccount(excluded)
		if err != nil {
			openAIError(w, 503, "no available account: "+err.Error(), "service_unavailable")
			return
		}
		started := time.Now()
		res, body := s.Router.Toolsets.ChatResponses(r.Context(), acc, raw, model, attempt)
		s.logToolsetsResponsesAttempt(acc, r, res, model, false, lc, started)
		if res.Success {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
		lastRes = res
		if res.RequestRejected {
			break
		}
		if res.AuthFailed {
			s.Router.MarkAccountOffline(acc, "session 失效: "+res.Error)
			excluded[acc.ID] = true
			continue
		}
		// 限频/上游波动：排除该号换下一个重试。
		excluded[acc.ID] = true
	}
	_, typ, code := toolsetsResponsesErrorStatus(lastRes)
	openAIError(w, code, lastRes.Error, typ)
}

// toolsetsResponsesStream 流式：上游 Responses SSE 事件逐条原样转发。
func (s *Server) toolsetsResponsesStream(w http.ResponseWriter, r *http.Request, raw []byte, model string, lc toolsetsLogContext) {
	fl, ok := w.(http.Flusher)
	if !ok {
		openAIError(w, 500, "stream unsupported", "server_error")
		return
	}
	started := false
	writeEvent := func(name string, data []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		fl.Flush()
	}
	ensureStarted := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	}

	excluded := map[int64]bool{}
	for attempt := 0; attempt < 3; attempt++ {
		acc, err := s.pickToolsetsAccount(excluded)
		if err != nil {
			openAIError(w, 503, "no available account: "+err.Error(), "service_unavailable")
			return
		}
		reqStart := time.Now()
		res := s.Router.Toolsets.StreamChatResponses(r.Context(), acc, raw, model, attempt, func(event string, data []byte) error {
			ensureStarted()
			writeEvent(event, data)
			return nil
		})
		s.logToolsetsResponsesAttempt(acc, r, res, model, true, lc, reqStart)
		if res.Success {
			return
		}
		if started {
			// 已开流无法回退 JSON 错误——发 Responses 标准 error 事件让 SDK 干净收尾（不发 completed）。
			writeEvent("error", []byte(mustJSON(map[string]interface{}{
				"type":            "error",
				"code":            "service_unavailable",
				"message":         res.Error,
				"param":           nil,
				"sequence_number": -1,
			})))
			return
		}
		if res.AuthFailed {
			s.Router.MarkAccountOffline(acc, "session 失效: "+res.Error)
			excluded[acc.ID] = true
			continue
		}
		if res.RequestRejected {
			_, typ, code := toolsetsResponsesErrorStatus(res)
			openAIError(w, code, res.Error, typ)
			return
		}
		// 限频/上游波动：排除该号换下一个。
		excluded[acc.ID] = true
	}
	openAIError(w, 503, "toolsets responses upstream busy after retries", "service_unavailable")
}

// toolsetsResponsesErrorStatus 把 Result 错误分类映射为 OpenAI 协议 (类型, 状态码)。
func toolsetsResponsesErrorStatus(res *provider.Result) (string, string, int) {
	switch {
	case res.AuthFailed:
		return "authentication_error", "authentication_error", 401
	case res.RequestRejected:
		return "invalid_request_error", "invalid_request_error", 400
	case res.RateLimited:
		return "service_unavailable", "service_unavailable", 503
	default:
		return "server_error", "server_error", 500
	}
}

// logToolsetsResponsesAttempt 把 toolsets responses 请求直写面板请求日志（无 credits 计量，
// Credits=0）。Endpoint=anthropic-toolsets 同款口径区分来源。
func (s *Server) logToolsetsResponsesAttempt(acc *store.Account, r *http.Request, res *provider.Result, model string, stream bool, lc toolsetsLogContext, started time.Time) {
	l := &store.RequestLog{
		AccountID:       &acc.ID,
		Credits:         0,
		Model:           model,
		Endpoint:        "openai-toolsets-responses",
		DurationMs:      time.Since(started).Milliseconds(),
		RequestBytes:    res.RequestBytes,
		Path:            r.URL.Path,
		Stream:          stream,
		ClientBody:      lc.clientBody,
		ClientHeaders:   lc.clientHeaders,
		UpstreamBody:    res.UpstreamBody,
		UpstreamHeaders: res.UpstreamHeaders,
		UpstreamURL:     res.UpstreamURL,
		Egress:          res.Egress,
		AccountEmail:    acc.Email,
	}
	if l.UpstreamURL == "" {
		l.UpstreamURL = "gateway.postman.com/toolsets/v1/responses"
	}
	if res.Success {
		l.Status = "success"
	} else {
		l.Status = "error"
		l.ErrorMessage = res.Error
	}
	_ = s.Store.LogRequest(l)
}
