// toolsets_shared.go —— 两个「工具集」透传端点（Anthropic /v1/messages 与
// OpenAI-Responses /v1/responses）共享的重试/换号驱动。两端处理结构同构：
// 选号轮询 + 最多 3 次换号重试 + 同一套错误分类（RequestRejected 直接返回、
// AuthFailed 标离线摘号换号、其余排除换号），成功时上游 body 原样透传。
// 唯一的协议差异（错误体格式、状态码映射、已开流时的 error 事件）由 toolsetsProto 承载。
//
// 注意：toolsets 刻意不走主路由 router.runAttempts —— 它无 credits 计费、无会话粘性、
// 无出口轮换、无网关两级 failover，与主路径语义不同；强行合并会把简单透传与主路径的
// 精密 failover 耦合在一起。此处只消除两个 toolsets 端点之间的真实重复。
package api

import (
	"net/http"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// toolsetsProto 承载两个 toolsets 端点之间仅有的协议差异；重试循环本身完全一致。
type toolsetsProto struct {
	// writeErr 按协议规范写出 JSON 错误体（尚未开流时使用）。
	writeErr func(w http.ResponseWriter, status int, msg, typ string)
	// rateLimitedType/rateLimitedStatus 是限频/上游波动错误的协议错误类型与状态码
	// （Anthropic: overloaded_error/529；OpenAI: service_unavailable/503）。
	rateLimitedType   string
	rateLimitedStatus int
	// genericErrType 是兜底（未分类）错误的协议错误类型，状态码恒为 500。
	genericErrType string
	// noAccountType 是 503「无可用账号」错误的协议错误类型。
	noAccountType string
	// streamUnsupType 是 500「不支持流式」错误的协议错误类型。
	streamUnsupType string
	// busyStatus/busyMsg/busyType 是流式路径重试耗尽（且从未开流）时的兜底错误。
	busyStatus int
	busyMsg    string
	busyType   string
	// streamErrEvent 构造「已开流」后无法再回退 JSON 错误时发出的 SSE error 事件体。
	streamErrEvent func(msg string) []byte
}

// errorStatus 把失败的 Result 分类映射为协议的 (错误类型, HTTP 状态码)。
// AuthFailed/RequestRejected 两臂两协议逐字一致；仅限频与兜底(500)的
// 错误类型/状态码随协议而变，由 proto 的数据字段承载。
func (p toolsetsProto) errorStatus(res *provider.Result) (string, int) {
	switch {
	case res.Kind == provider.KindAuthFailed:
		return "authentication_error", 401
	case res.Kind == provider.KindRequestRejected:
		return "invalid_request_error", 400
	case res.Kind == provider.KindRateLimited:
		return p.rateLimitedType, p.rateLimitedStatus
	default:
		return p.genericErrType, 500
	}
}

// runToolsetsNonStream 驱动非流式 toolsets 透传：轮询选号 + 最多 3 次换号重试 + 错误分类。
// 成功时把上游 JSON body 原样透传。两个端点仅通过 p 与注入的 invoke/logAttempt 区分。
func (s *Server) runToolsetsNonStream(
	w http.ResponseWriter,
	p toolsetsProto,
	invoke func(acc *store.Account, attempt int) (*provider.Result, []byte),
	logAttempt func(acc *store.Account, res *provider.Result, started time.Time),
) {
	excluded := map[int64]bool{}
	var lastRes *provider.Result
	for attempt := 0; attempt < 3; attempt++ {
		acc, err := s.pickToolsetsAccount(excluded)
		if err != nil {
			p.writeErr(w, 503, "no available account: "+err.Error(), p.noAccountType)
			return
		}
		started := time.Now()
		res, body := invoke(acc, attempt)
		logAttempt(acc, res, started)
		if res.Success {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
		lastRes = res
		// 请求内容错误：换号无用，直接返回。
		if res.Kind == provider.KindRequestRejected {
			break
		}
		// session 失效属账号自身问题：标离线摘号（与主路由 AuthFailed 同口径），换下一个。
		if res.Kind == provider.KindAuthFailed {
			s.Router.MarkAccountOffline(acc, "session 失效: "+res.Error)
			excluded[acc.ID] = true
			continue
		}
		// 限频/上游波动：排除该号换下一个重试。
		excluded[acc.ID] = true
	}
	typ, code := p.errorStatus(lastRes)
	p.writeErr(w, code, lastRes.Error, typ)
}

// runToolsetsStream 驱动流式 toolsets 透传。延迟开流（首个上游事件前不落 200、不发事件），
// 使开流前的失败仍能回退 JSON 错误或重试；一旦已开流则改发协议标准 error 事件（p.streamErrEvent）
// 并终止循环。两个端点仅通过 p 与注入的 invoke/logAttempt 区分。
func (s *Server) runToolsetsStream(
	w http.ResponseWriter,
	p toolsetsProto,
	invoke func(acc *store.Account, attempt int, emit func(event string, data []byte) error) *provider.Result,
	logAttempt func(acc *store.Account, res *provider.Result, started time.Time),
) {
	fl, ok := w.(http.Flusher)
	if !ok {
		p.writeErr(w, 500, "stream unsupported", p.streamUnsupType)
		return
	}
	sw := newSSEWriter(w, fl, nil)

	excluded := map[int64]bool{}
	for attempt := 0; attempt < 3; attempt++ {
		acc, err := s.pickToolsetsAccount(excluded)
		if err != nil {
			p.writeErr(w, 503, "no available account: "+err.Error(), p.noAccountType)
			return
		}
		reqStart := time.Now()
		res := invoke(acc, attempt, func(event string, data []byte) error {
			sw.EventRaw(event, data)
			return nil
		})
		logAttempt(acc, res, reqStart)
		if res.Success {
			return
		}
		if sw.Started() {
			// 已向客户端开流，无法回退 JSON 错误——发协议标准 error 事件让 SDK 干净收尾。
			sw.EventRaw("error", p.streamErrEvent(res.Error))
			return
		}
		if res.Kind == provider.KindAuthFailed {
			// session 失效属账号自身问题：标离线摘号（与主路由 AuthFailed 同口径），换下一个。
			s.Router.MarkAccountOffline(acc, "session 失效: "+res.Error)
			excluded[acc.ID] = true
			continue
		}
		if res.Kind == provider.KindRequestRejected {
			typ, code := p.errorStatus(res)
			p.writeErr(w, code, res.Error, typ)
			return
		}
		// 限频/上游波动：排除该号换下一个。
		excluded[acc.ID] = true
	}
	p.writeErr(w, p.busyStatus, p.busyMsg, p.busyType)
}
