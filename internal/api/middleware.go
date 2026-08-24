// middleware.go —— 对话类端点的 Trace 中间件：包装 handler 记录请求/响应、
// 通过 traceResponseWriter 捕获状态码与响应体供日志与告警使用。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"ps2api/internal/provider"
)

const maxChatBody = 16 << 20

func traceChat(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		endpoint := "openai"
		if strings.HasSuffix(r.URL.Path, "/messages") {
			endpoint = "anthropic"
		}
		ctx, traceID := provider.NewTraceContext(r.Context(), endpoint)
		if traceID == "" {
			next(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxChatBody+1))
		if err != nil {
			provider.Trace(ctx, "client.request.error", map[string]interface{}{"error": err.Error()})
			jsonError(w, 400, err.Error(), "invalid_request")
			return
		}
		if len(body) > maxChatBody {
			provider.Trace(ctx, "client.request.error", map[string]interface{}{"error": "request body too large", "bytes": len(body)})
			jsonError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request")
			return
		}
		var loggedBody interface{} = string(body)
		if json.Valid(body) {
			loggedBody = json.RawMessage(body)
		}
		r = r.WithContext(ctx)
		r.Body = io.NopCloser(bytes.NewReader(body))
		w.Header().Set("X-Postman2API-Trace-ID", traceID)
		provider.Trace(ctx, "client.request", map[string]interface{}{
			"method": r.Method, "path": r.URL.RequestURI(), "remote_addr": r.RemoteAddr,
			"headers": r.Header, "body": loggedBody,
		})
		next(&traceResponseWriter{ResponseWriter: w, ctx: ctx}, r)
	}
}

type traceResponseWriter struct {
	http.ResponseWriter
	ctx    context.Context
	status int
}

func (w *traceResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	provider.Trace(w.ctx, "client.response.headers", map[string]interface{}{"status": status, "headers": w.Header()})
	w.ResponseWriter.WriteHeader(status)
}

func (w *traceResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	provider.Trace(w.ctx, "client.response.body", map[string]interface{}{"body": string(body)})
	return w.ResponseWriter.Write(body)
}

func (w *traceResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
