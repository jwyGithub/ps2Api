package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"ps2api/internal/store"
)

// collectToolCalls 按 index 升序聚合工具调用，避免上游索引跳号导致调用丢失。
func collectToolCalls(toolAcc map[int]*ToolCall) []ToolCall {
	idx := make([]int, 0, len(toolAcc))
	for i := range toolAcc {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]ToolCall, 0, len(idx))
	for _, i := range idx {
		if tc, ok := toolAcc[i]; ok {
			out = append(out, *tc)
		}
	}
	return out
}

func appendToolArguments(current, next string) string {
	if current == "" {
		return next
	}
	if next == "" {
		return current
	}
	combined := current + next
	if json.Valid([]byte(combined)) {
		return combined
	}
	var left, right map[string]interface{}
	if json.Unmarshal([]byte(current), &left) == nil && json.Unmarshal([]byte(next), &right) == nil {
		for key, value := range right {
			left[key] = value
		}
		if b, err := json.Marshal(left); err == nil {
			return string(b)
		}
	}
	return combined
}

func (p *Provider) streamInternal(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, postmanModel string, emit EmitFunc, res *Result) error {
	started := time.Now()
	defer func() {
		Trace(ctx, "upstream.complete", map[string]interface{}{
			"account_id": acc.ID, "duration_ms": time.Since(started).Milliseconds(),
			"success": res.Success, "error": res.Error, "conversation_id": res.ConversationID,
		})
	}()
	body, plan := p.buildBody(req, tokens, postmanModel, acc.ID)

	// 分片续传：仅当 USER_QUERY 的完整 query 超过上游硬上限时启用。把大 query 按 rune 切成多片，
	// 顺序喂进同一个 conversationId（前置片只求收下并回 ACK，回复丢弃、只取 convID），最后一片
	// 带真正指令并作为真实流式响应吐给客户端。分片数超过 MaxQueryChunks 则回退到单发 + 中段截断
	// （capUpstreamQuery），绝不无限膨胀成几十次上游往返。
	queryLimit := MaxUpstreamQueryRunes - 100
	if plan.chunkable && len([]rune(plan.fullQuery)) > queryLimit {
		chunks := splitQueryIntoChunks(plan.fullQuery, queryLimit-QueryChunkWrapperReserve)
		if len(chunks) >= 2 && len(chunks) <= MaxQueryChunks {
			return p.streamChunked(ctx, acc, req, tokens, body, plan, chunks, emit, res)
		}
		Trace(ctx, "upstream.chunking.skipped", map[string]interface{}{
			"account_id": acc.ID, "chunks": len(chunks), "max_chunks": MaxQueryChunks,
			"query_runes": len([]rune(plan.fullQuery)),
		})
	}
	return p.sendOnce(ctx, acc, req, tokens, body, emit, res)
}

// streamChunked 编排分片续传：前 n-1 片作为「前置片」顺序发出（noop emit，丢弃模型 ACK 回复，
// 只取回 conversationId 用于续接下一片），最后一片带真实指令、正常 emit 给客户端。任一前置片
// 失败即中止并把失败信息透传到 res，绝不向客户端 emit 半截内容。res.ConversationID 最终落在
// 最后一片返回的真实会话 ID 上，外层 RememberConversation 据此把消息指纹映射到它，续聊无缝衔接。
//
// 前提假设：上游对 USER_QUERY 也按 conversationId 跨轮保留上下文（与真实客户端续聊行为一致）。
// 若某前置片没能返回 conversationId，后续片只能各自新开会话、前文丢失——此为已知降级，仍不比
// 单发中段截断更差；这里记 trace 但不中止，最后一片照常产出答复并为后续轮沉淀一个会话 ID。
func (p *Provider) streamChunked(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, body map[string]interface{}, plan outboundPlan, chunks []string, emit EmitFunc, res *Result) error {
	n := len(chunks)
	convID := plan.convID
	Trace(ctx, "upstream.chunking.begin", map[string]interface{}{
		"account_id": acc.ID, "chunks": n, "start_conversation_id": convID,
	})
	for i := 0; i < n-1; i++ {
		setChunkInput(plan.input, capUpstreamQuery(wrapPrimingChunk(chunks[i], i+1, n)), convID)
		primeRes := &Result{}
		err := p.sendOnce(ctx, acc, req, tokens, body, noopEmit, primeRes)
		if err != nil || !primeRes.Success {
			// 前置片失败：把诊断 / 路由相关字段整体透传给外层 res，按普通失败上抛，绝不 emit。
			*res = *primeRes
			if err == nil {
				err = fmt.Errorf("%s", primeRes.Error)
			}
			return err
		}
		if primeRes.ConversationID != "" {
			convID = primeRes.ConversationID
		} else {
			Trace(ctx, "upstream.chunking.no_conversation_id", map[string]interface{}{
				"account_id": acc.ID, "chunk": i + 1, "of": n,
			})
		}
		Trace(ctx, "upstream.chunking.primed", map[string]interface{}{
			"account_id": acc.ID, "chunk": i + 1, "of": n, "conversation_id": convID,
		})
	}
	// 最后一片：带真实指令，正常流式 emit 给客户端。
	setChunkInput(plan.input, capUpstreamQuery(wrapFinalChunk(chunks[n-1], n)), convID)
	return p.sendOnce(ctx, acc, req, tokens, body, emit, res)
}

// setChunkInput 就地改写 body 内 input 的 query 与 conversationId（convID 为空时置 null，
// 与 buildBody 冷启动保持一致），使同一请求信封可复用于下一片。
func setChunkInput(input map[string]interface{}, query, convID string) {
	input["query"] = query
	if convID != "" {
		input["conversationId"] = convID
	} else {
		input["conversationId"] = nil
	}
}

// noopEmit 丢弃前置分片的流式增量（只关心其返回的 conversationId）。
func noopEmit(Delta) error { return nil }

// sendOnce 执行一次完整的上游往返：marshal body → 发送（含代理出口选择与直连兜底）→ 逐状态码
// 判定 → 读 SSE 流并把增量经 emit 吐出，同时把会话 ID / 实际模型 / 用量 / 各类错误标记写回 res。
// 单发路径与分片续传的每一片都复用它。
func (p *Provider) sendOnce(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, body map[string]interface{}, emit EmitFunc, res *Result) error {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		res.Error = err.Error()
		return err
	}
	res.RequestBytes = len(bodyBytes) // 记录出站体积，供 403 与请求体大小相关性分析
	// 出站 body 体检：超大 payload 是触发 Cloudflare WAF 403 的常见诱因，
	// 提前告警以便定位（如历史工具原文、超大 schema 未压缩等）。仅告警不阻断。
	if len(bodyBytes) > MaxRequestBodyWarnBytes {
		Trace(ctx, "upstream.request.oversize", map[string]interface{}{
			"account_id":      acc.ID,
			"body_bytes":      len(bodyBytes),
			"threshold_bytes": MaxRequestBodyWarnBytes,
		})
	}

	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	// newReq 每次构造一个全新的出站请求（body reader 一经 Do 即被消费，兜底重试需重建）。
	newReq := func() (*http.Request, error) {
		r, e := http.NewRequestWithContext(ctx, "POST", p.chatURL(tokens), strings.NewReader(string(bodyBytes)))
		if e != nil {
			return nil, e
		}
		r.Header = p.buildHeaders(tokens)
		return r, nil
	}
	httpReq, err := newReq()
	if err != nil {
		res.Error = err.Error()
		return err
	}
	// 出口选择：默认按账号粘性走同一代理出口；遇 Cloudflare 403 重试（EgressAttempt 递增）
	// 切下一个出口 IP；仅当未配置任何代理时才回退本机直连（启用即全量走代理）。
	client, egress, viaProxy := p.proxies.selectFor(acc.ID, req.EgressAttempt)
	if !viaProxy {
		client, egress = p.Client, "direct"
	}
	p.applyCookies(acc.ID, httpReq, egress)
	Trace(ctx, "upstream.request", map[string]interface{}{
		"method": httpReq.Method, "url": httpReq.URL.String(), "headers": httpReq.Header,
		"body": json.RawMessage(bodyBytes), "account_id": acc.ID, "egress": egress,
	})

	resp, err := client.Do(httpReq)
	// 代理全挂兜底直连：仅当经代理出站、传输层失败（ctx.Err()==nil，即拨号/CONNECT 失败而非
	// 上游超时或客户端断开）且开关开启时，改用本机直连重试一次。此处失败发生在流式响应开始前，
	// 尚未 emit 任何数据，故重试安全。开关关闭则维持严格代理（代理全挂即失败）。
	if err != nil && viaProxy && ctx.Err() == nil && p.proxies.fallbackDirectEnabled() {
		Trace(ctx, "upstream.proxy.fallback_direct", map[string]interface{}{
			"account_id": acc.ID, "egress": egress, "error": err.Error(),
		})
		if r, e := newReq(); e == nil {
			httpReq = r
			client, egress, viaProxy = p.Client, "direct", false
			p.applyCookies(acc.ID, httpReq, egress)
			resp, err = client.Do(httpReq)
		}
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			res.Error = "Upstream timeout"
		} else if strings.Contains(err.Error(), ErrClientDisconnected) || ctx.Err() == context.Canceled {
			res.Error = ErrClientDisconnected
		} else {
			res.Error = "Postman request failed: " + err.Error()
		}
		return err
	}
	defer resp.Body.Close()
	if p.cookies != nil {
		p.cookies.remember(acc.ID, httpReq.URL, egress, resp.Cookies())
	}
	Trace(ctx, "upstream.response.headers", map[string]interface{}{
		"status": resp.StatusCode, "headers": resp.Header, "account_id": acc.ID,
	})
	res.RateLimit = parseRateLimit(resp.Header, time.Now())
	if identityError := postmanIdentityError(resp.Header); identityError != "" {
		res.Error = "Postman authentication failed: " + identityError
		res.AuthFailed = true
		return fmt.Errorf("%s", res.Error)
	}
	if isCloudflareHTMLRejection(resp.StatusCode, resp.Header) {
		res.Error = "Postman gateway rejected request (403, Cloudflare)"
		// 这是 Cloudflare 边缘的安全/风控拦截(瞬时、按评分/速率判定),不是请求内容错误也不是
		// 账号损坏——标记 GatewayBlocked 让 router 退避重试,而非当作 RequestRejected 直接返回。
		res.GatewayBlocked = true
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		Trace(ctx, "upstream.response.body", map[string]interface{}{"body": string(body), "account_id": acc.ID})
		res.RejectionDetail = cloudflareRejectionDetail(resp.StatusCode, resp.Header, string(body), len(bodyBytes)) + "\n出口: " + egress
		return fmt.Errorf("%s", res.Error)
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		res.Error = fmt.Sprintf("Postman auth failed (%d)", resp.StatusCode)
		res.AuthFailed = true
		return fmt.Errorf("%s", res.Error)
	}
	if resp.StatusCode == 429 {
		res.Error = "Postman rate limited"
		res.RateLimited = true
		return fmt.Errorf("%s", res.Error)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		Trace(ctx, "upstream.response.body", map[string]interface{}{"body": string(b), "account_id": acc.ID})
		res.Error = fmt.Sprintf("Postman API error (%d): %s", resp.StatusCode, string(b))
		// 4xx(除已处理的 401/403/429)是请求内容问题——坏请求、工具名冲突等,
		// 换账号重试无用,标记为 RequestRejected 让 router 直接返回、不污染账号。
		if resp.StatusCode < 500 {
			res.RequestRejected = true
		}
		return fmt.Errorf("%s", res.Error)
	}

	reader := NewStreamReader()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)

	firstPayloadLine := true
	for scanner.Scan() {
		line := scanner.Text()
		Trace(ctx, "upstream.response.sse", map[string]interface{}{"line": line, "account_id": acc.ID})
		// 首包内容探测：某些 Cloudflare 拦截会以 200/无 text/html 头返回 HTML 挑战页，
		// 头部判定(isCloudflareHTMLRejection)漏掉，此处按流式首个非空行内容兜底。
		if firstPayloadLine && strings.TrimSpace(line) != "" {
			firstPayloadLine = false
			if looksLikeHTML(line) {
				res.Error = "Postman gateway rejected request (Cloudflare HTML in stream)"
				res.RequestRejected = true
				res.RejectionDetail = cloudflareRejectionDetail(resp.StatusCode, resp.Header, line, len(bodyBytes)) + "\n出口: " + egress
				return fmt.Errorf("%s", res.Error)
			}
		}
		for _, d := range reader.Feed(line) {
			if err := emit(d); err != nil {
				res.Error = ErrClientDisconnected
				return err
			}
		}
		if reader.QuotaExceeded {
			break
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		res.Error = "stream read error: " + err.Error()
		return err
	}

	if reader.QuotaExceeded {
		res.Error = "Postman AI quota exceeded"
		res.QuotaExhausted = true
		res.Usage = reader.Usage
		return fmt.Errorf("%s", res.Error)
	}
	if reader.Err != "" {
		res.Error = reader.Err
		res.Usage = reader.Usage
		// 上游自己调模型失败(Policy Error 等):账号健康、请求也没问题,router 据此不标记账号、
		// 续聊也不换号(换号会丢服务端会话上下文)。见 isUpstreamModelFailure。
		res.UpstreamFailure = reader.UpstreamFailure
		// 工具相关的 failure(工具名冲突、无可用工具等)是请求内容问题,不是账号故障——
		// 换账号重试无用,标记 RequestRejected 让 router 直接返回、不把账号踢出池。
		if reader.RequestRejected || isRequestRejectionMessage(reader.Err) {
			res.RequestRejected = true
		}
		res.SessionCorrupt = reader.SessionCorrupt
		return fmt.Errorf("%s", res.Error)
	}

	if reader.ConversationID != "" {
		res.ConversationID = reader.ConversationID
	}
	res.ActualModel = reader.ActualModel
	res.Usage = reader.Usage

	// 上游干净结束(EOF)但既没有 [DONE] 收尾、也没有吐出任何正文/工具调用(failure/quota 已在
	// 上面各自返回)：这是一次不完整的空流——典型为 Postman 已收下 TOOL_RESPONSE 但 Bedrock
	// 生成被掐断。绝不能当成功：那会向客户端发 end_turn(空回复)，并把已被服务端消费的 tool call
	// 会话固化成映射，导致后续续聊必然 TOOL_CALL_NOT_FOUND。标记为可重试的上游失败(账号健康,
	// 不 MarkError)，并置 SessionCorrupt 让上层失效该会话映射——续聊重试时降级为 USER_QUERY
	// 重建，而不是再交一次已消费的 toolCallId。对照：真正成功的空回复一定带 [DONE]。
	if !reader.sawDone && !reader.hadOutput() {
		res.Error = "Upstream returned an empty stream (no content and no completion marker)"
		res.UpstreamFailure = true
		res.SessionCorrupt = true
		return fmt.Errorf("%s", res.Error)
	}

	for _, d := range reader.Finish() {
		if err := emit(d); err != nil {
			res.Error = ErrClientDisconnected
			return err
		}
	}
	res.Success = true
	return nil
}
