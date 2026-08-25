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
	body := p.buildBody(req, tokens, postmanModel, acc.ID)
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
		return fmt.Errorf("%s", res.Error)
	}

	if reader.ConversationID != "" {
		res.ConversationID = reader.ConversationID
	}
	res.ActualModel = reader.ActualModel
	res.Usage = reader.Usage

	for _, d := range reader.Finish() {
		if err := emit(d); err != nil {
			res.Error = ErrClientDisconnected
			return err
		}
	}
	res.Success = true
	return nil
}
