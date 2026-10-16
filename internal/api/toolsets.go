// toolsets.go —— claude-opus-5 / claude-sonnet-5 的 /v1/messages 原生透传。
// 这些模型走 Postman「工具集」端点（Anthropic 原生协议，见 provider/toolsets.go），
// 客户端请求体与上游同构，直接透传零转换（thinking/signature/图片 blocks 原样保留）。
// 端点无 credits usage 事件 → 不参与 credits 计费（chargeKey 对 Credits<=0 是 no-op），
// 请求日志直写存储保持面板可见。
// 重试/换号循环与 OpenAI-Responses 端点同构，共享驱动见 toolsets_shared.go。

package api

import (
	"net/http"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// handleToolsetsMessages 是 /v1/messages 对 toolsets 模型（claude-opus-5/sonnet-5）的入口。
// raw 为客户端原始 body 字节（透传到上游，仅 model 字段改写为三段式路由名）。
func (s *Server) handleToolsetsMessages(w http.ResponseWriter, r *http.Request, raw []byte, ar AnthropicReq) {
	lc := toolsetsLogContext{clientBody: string(raw), clientHeaders: inboundHeadersJSON(r.Header)}
	if !ar.Stream {
		s.runToolsetsNonStream(w, anthropicToolsetsProto,
			func(acc *store.Account, attempt int) (*provider.Result, []byte) {
				return s.Router.Toolsets.Chat(r.Context(), acc, raw, ar.Model, attempt)
			},
			func(acc *store.Account, res *provider.Result, started time.Time) {
				s.logToolsetsAttempt(acc, r, res, ar, false, lc, started)
			})
		return
	}
	s.runToolsetsStream(w, anthropicToolsetsProto,
		func(acc *store.Account, attempt int, emit func(event string, data []byte) error) *provider.Result {
			return s.Router.Toolsets.StreamChat(r.Context(), acc, raw, ar.Model, attempt, emit)
		},
		func(acc *store.Account, res *provider.Result, started time.Time) {
			s.logToolsetsAttempt(acc, r, res, ar, true, lc, started)
		})
}

// anthropicToolsetsProto 把共享 toolsets 驱动适配到 Anthropic /v1/messages 协议
// （错误体格式、状态码映射、已开流时的 error 事件）。
var anthropicToolsetsProto = toolsetsProto{
	writeErr:        anthropicError,
	noAccountType:   "api_error",
	streamUnsupType: "api_error",
	// 529 overloaded_error 是 Anthropic 表达「上游暂时不可用」的标准方式（503 不在其枚举内）。
	rateLimitedType:   "overloaded_error",
	rateLimitedStatus: 529,
	genericErrType:    "api_error",
	busyStatus:        529,
	busyMsg:           "toolsets upstream busy after retries",
	busyType:          "overloaded_error",
	streamErrEvent: func(msg string) []byte {
		return []byte(mustJSON(map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": msg},
		}))
	},
}

// toolsetsLogContext 携带单次客户端请求的入站排查上下文（body/头），各 attempt 共用。
type toolsetsLogContext struct {
	clientBody    string
	clientHeaders string
}

// pickToolsetsAccount 从号池选一个可用账号（toolsets 无会话粘性，普通轮询）。
func (s *Server) pickToolsetsAccount(excluded map[int64]bool) (*store.Account, error) {
	// selectAccount(pinned=nil) 退化为 pickAccount 轮询；messages 传 nil（指纹粘性不适用）。
	acc, _, err := s.Router.SelectAccount(nil, excluded, nil, false)
	return acc, err
}

// logToolsetsAttempt 把 toolsets 请求直写面板请求日志（toolsets 无 credits 计量，
// Router.logAttempt 的 creditsConsumed 口径不适用，但可见性需要保持）。
// 入站 body/头来自 lc（各 attempt 共用），出站 body/头/URL/出口来自 res（provider 侧填充）。
func (s *Server) logToolsetsAttempt(acc *store.Account, r *http.Request, res *provider.Result, ar AnthropicReq, stream bool, lc toolsetsLogContext, started time.Time) {
	l := &store.RequestLog{
		AccountID:       &acc.ID,
		Credits:         0,
		Model:           ar.Model,
		Endpoint:        "anthropic-toolsets",
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
		l.UpstreamURL = "gateway.postman.com/toolsets/v1/messages"
	}
	if res.Success {
		l.Status = "success"
	} else {
		l.Status = "error"
		l.ErrorMessage = res.Error
	}
	_ = s.Store.LogRequest(l)
}
