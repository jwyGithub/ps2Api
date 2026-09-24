// toolsets.go —— claude-opus-5 / claude-sonnet-5 的 /v1/messages 原生透传。
// 这些模型走 Postman「工具集」端点（Anthropic 原生协议，见 provider/toolsets.go），
// 客户端请求体与上游同构，直接透传零转换（thinking/signature/图片 blocks 原样保留）。
// 端点无 credits usage 事件 → 不参与 credits 计费（chargeKey 对 Credits<=0 是 no-op），
// 请求日志直写存储保持面板可见。

package api

import (
	"fmt"
	"net/http"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// handleToolsetsMessages 是 /v1/messages 对 toolsets 模型（claude-opus-5/sonnet-5）的入口。
// raw 为客户端原始 body 字节（透传到上游，仅 model 字段改写为三段式路由名）。
func (s *Server) handleToolsetsMessages(w http.ResponseWriter, r *http.Request, raw []byte, ar AnthropicReq) {
	if !ar.Stream {
		s.toolsetsNonStream(w, r, raw, ar)
		return
	}
	s.toolsetsStream(w, r, raw, ar)
}

// pickToolsetsAccount 从号池选一个可用账号（toolsets 无会话粘性，普通轮询）。
func (s *Server) pickToolsetsAccount(excluded map[int64]bool) (*store.Account, error) {
	// selectAccount(pinned=nil) 退化为 pickAccount 轮询；messages 传 nil（指纹粘性不适用）。
	acc, _, err := s.Router.SelectAccount(nil, excluded, nil, false)
	return acc, err
}

func (s *Server) toolsetsNonStream(w http.ResponseWriter, r *http.Request, raw []byte, ar AnthropicReq) {
	excluded := map[int64]bool{}
	var lastRes *provider.Result
	for attempt := 0; attempt < 3; attempt++ {
		acc, err := s.pickToolsetsAccount(excluded)
		if err != nil {
			anthropicError(w, 503, "no available account: "+err.Error(), "api_error")
			return
		}
		res, body := s.Router.Toolsets.Chat(r.Context(), acc, raw, ar.Model, attempt)
		s.logToolsetsAttempt(acc, r, res, ar, false)
		if res.Success {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
		lastRes = res
		// 请求内容错误：换号无用，直接返回。
		if res.RequestRejected {
			break
		}
		// session 失效属账号自身问题：标离线摘号（与主路由 AuthFailed 同口径），换下一个。
		if res.AuthFailed {
			s.Router.MarkAccountOffline(acc, "session 失效: "+res.Error)
			excluded[acc.ID] = true
			continue
		}
		// 限频/上游波动：排除该号换下一个重试。
		excluded[acc.ID] = true
	}
	_, typ, code := toolsetsErrorStatus(lastRes)
	anthropicError(w, code, lastRes.Error, typ)
}

func (s *Server) toolsetsStream(w http.ResponseWriter, r *http.Request, raw []byte, ar AnthropicReq) {
	fl, ok := w.(http.Flusher)
	if !ok {
		anthropicError(w, 500, "stream unsupported", "api_error")
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
			anthropicError(w, 503, "no available account: "+err.Error(), "api_error")
			return
		}
		res := s.Router.Toolsets.StreamChat(r.Context(), acc, raw, ar.Model, attempt, func(event string, data []byte) error {
			ensureStarted()
			writeEvent(event, data)
			return nil
		})
		s.logToolsetsAttempt(acc, r, res, ar, true)
		if res.Success {
			return
		}
		if started {
			// 已向客户端开流，无法回退 JSON 错误——发 Anthropic 标准 error 事件让 SDK 干净收尾。
			writeEvent("error", []byte(mustJSON(map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": res.Error},
			})))
			return
		}
		if res.AuthFailed {
			// session 失效属账号自身问题：标离线摘号（与主路由 AuthFailed 同口径），换下一个。
			s.Router.MarkAccountOffline(acc, "session 失效: "+res.Error)
			excluded[acc.ID] = true
			continue
		}
		if res.RequestRejected {
			_, typ, code := toolsetsErrorStatus(res)
			anthropicError(w, code, res.Error, typ)
			return
		}
		// 限频/上游波动：排除该号换下一个。
		excluded[acc.ID] = true
	}
	anthropicError(w, 529, "toolsets upstream busy after retries", "overloaded_error")
}

// toolsetsErrorStatus 把 Result 的错误分类映射为 Anthropic 协议的 (错误类型, 状态码)。
func toolsetsErrorStatus(res *provider.Result) (string, string, int) {
	switch {
	case res.AuthFailed:
		return "authentication_error", "authentication_error", 401
	case res.RequestRejected:
		return "invalid_request_error", "invalid_request_error", 400
	case res.RateLimited:
		// 529 overloaded_error 是 Anthropic 表达「上游暂时不可用」的标准方式（503 不在其枚举内）。
		return "overloaded_error", "overloaded_error", 529
	default:
		return "api_error", "api_error", 500
	}
}

// logToolsetsAttempt 把 toolsets 请求直写面板请求日志（toolsets 无 credits 计量，
// Router.logAttempt 的 creditsConsumed 口径不适用，但可见性需要保持）。
func (s *Server) logToolsetsAttempt(acc *store.Account, r *http.Request, res *provider.Result, ar AnthropicReq, stream bool) {
	l := &store.RequestLog{
		AccountID:     &acc.ID,
		Credits:       0,
		Model:         ar.Model,
		Endpoint:      "anthropic-toolsets",
		Path:          r.URL.Path,
		Stream:        stream,
		UpstreamBody:  truncateStr(res.Error),
		UpstreamURL:   "_gw/toolsets/v1/messages",
		AccountEmail:  acc.Email,
	}
	if res.Success {
		l.Status = "success"
	} else {
		l.Status = "error"
		l.ErrorMessage = res.Error
	}
	_ = s.Store.LogRequest(l)
}
