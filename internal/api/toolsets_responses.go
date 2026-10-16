// toolsets_responses.go —— toolsets GPT 模型（gpt-6-sol/gpt-6.1-sol/gpt-6-luna/gpt-5.6-*/
// gpt-5.5 等，openai/openai-responses 路由）的 /v1/responses 原生透传。
// 客户端请求体与上游同为 OpenAI Responses 协议，直接透传零转换（仅 model 改写为三段式
// 路由名、max_output_tokens 夹紧、工具名压缩改写——见 provider/toolsets_responses.go）。
// 端点无 credits usage 计量 → 不参与 credits 计费，请求日志直写存储保持面板可见。
// 处理结构与 toolsets.go（Anthropic 端）同构：选号轮询 + 3 次换号重试 + 错误分类，
// 共享重试驱动见 toolsets_shared.go。

package api

import (
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
		s.runToolsetsNonStream(w, openaiToolsetsResponsesProto,
			func(acc *store.Account, attempt int) (*provider.Result, []byte) {
				return s.Router.Toolsets.ChatResponses(r.Context(), acc, raw, model, attempt)
			},
			func(acc *store.Account, res *provider.Result, started time.Time) {
				s.logToolsetsResponsesAttempt(acc, r, res, model, false, lc, started)
			})
		return
	}
	s.runToolsetsStream(w, openaiToolsetsResponsesProto,
		func(acc *store.Account, attempt int, emit func(event string, data []byte) error) *provider.Result {
			return s.Router.Toolsets.StreamChatResponses(r.Context(), acc, raw, model, attempt, emit)
		},
		func(acc *store.Account, res *provider.Result, started time.Time) {
			s.logToolsetsResponsesAttempt(acc, r, res, model, true, lc, started)
		})
}

// openaiToolsetsResponsesProto 把共享 toolsets 驱动适配到 OpenAI Responses /v1/responses 协议。
var openaiToolsetsResponsesProto = toolsetsProto{
	writeErr:          func(w http.ResponseWriter, status int, msg, typ string) { openAIError(w, status, msg, typ) },
	rateLimitedType:   "service_unavailable",
	rateLimitedStatus: 503,
	genericErrType:    "server_error",
	noAccountType:     "service_unavailable",
	streamUnsupType:   "server_error",
	busyStatus:        503,
	busyMsg:           "toolsets responses upstream busy after retries",
	busyType:          "service_unavailable",
	streamErrEvent: func(msg string) []byte {
		// 已开流无法回退 JSON 错误——发 Responses 标准 error 事件让 SDK 干净收尾（不发 completed）。
		return []byte(mustJSON(map[string]interface{}{
			"type":            "error",
			"code":            "service_unavailable",
			"message":         msg,
			"param":           nil,
			"sequence_number": -1,
		}))
	},
}

// logToolsetsResponsesAttempt 把 toolsets responses 请求直写面板请求日志（无 credits 计量，
// Credits=0）。Endpoint=openai-toolsets-responses 区分来源。
func (s *Server) logToolsetsResponsesAttempt(acc *store.Account, r *http.Request, res *provider.Result, model string, stream bool, lc toolsetsLogContext, started time.Time) {
	l := &store.RequestLog{
		AccountID:       &acc.ID,
		Credits:         0,
		Model:           model,
		Endpoint:        "openai-toolsets-responses",
		DurationMs:      time.Since(started).Milliseconds(),
		RequestBytes:    res.Diag.RequestBytes,
		Path:            r.URL.Path,
		Stream:          stream,
		ClientBody:      lc.clientBody,
		ClientHeaders:   lc.clientHeaders,
		UpstreamBody:    res.Diag.UpstreamBody,
		UpstreamHeaders: res.Diag.UpstreamHeaders,
		UpstreamURL:     res.Diag.UpstreamURL,
		Egress:          res.Diag.Egress,
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
