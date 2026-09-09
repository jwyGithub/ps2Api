package provider

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrClientDisconnected 是「客户端在本次请求交付完成前就断开了」的错误串。
// router 见到它必须立即停止重试：已经没有接收方，换号只会在别的账号上白建一个 Postman
// 会话、消耗其额度，并把一个与账号无关的失败记成账号异常。
const ErrClientDisconnected = "Client disconnected"

// upstreamModelFailureTypes 是「上游自己调模型失败」的 failure.errorType：故障落在
// Postman → 模型(Bedrock)这一段，与本地账号健康、也与请求格式无关。典型上游原文：
//
//	{"errorType":"LLM_STREAM_ERROR","message":"LLM stream error: Failed after 3 attempts.
//	 Last error: AI_APICallError: Policy Error","userMessage":"That was unexpected :(…"}
//
// 这类失败有两条铁律（都由 router 的未分类兜底分支落实）：
//   - 不得把账号标记为 error：那会把号从 ActiveAccounts 里踢出去，一次上游抖动毁一批号；
//   - 续聊不得换号：换号让服务端 conversationId 失效，请求降级为无历史的 USER_QUERY，
//     失忆后必然再次失败，并把同样的错误逐个传染给后面的账号。
var upstreamModelFailureTypes = map[string]bool{
	"LLM_STREAM_ERROR": true,
}

func isUpstreamModelFailure(errorType string) bool {
	return upstreamModelFailureTypes[strings.ToUpper(strings.TrimSpace(errorType))]
}

func postmanIdentityError(headers http.Header) string {
	var errors []string
	for key, values := range headers {
		if !strings.HasPrefix(strings.ToLower(key), "x-pm-error-") {
			continue
		}
		errors = append(errors, values...)
	}
	if len(errors) == 0 {
		return ""
	}
	sort.Strings(errors)
	joined := strings.Join(errors, "; ")
	lower := strings.ToLower(joined)
	if strings.Contains(lower, "identity_status") ||
		strings.Contains(lower, "guest_unusable") ||
		strings.Contains(lower, "jwt is missing") {
		return joined
	}
	return ""
}

// ---------- 结果类型 ----------

func parseRateLimit(headers http.Header, now time.Time) *RateLimit {
	limit, _ := strconv.Atoi(strings.TrimSpace(headers.Get("X-RateLimit-Limit")))
	remaining, _ := strconv.Atoi(strings.TrimSpace(headers.Get("X-RateLimit-Remaining")))
	rate := &RateLimit{Limit: limit, Remaining: remaining}
	for _, part := range strings.Split(headers.Get("RateLimit-Policy"), ";") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(part), "w="); ok {
			rate.WindowSeconds, _ = strconv.Atoi(value)
		}
	}
	if value, err := strconv.ParseInt(strings.TrimSpace(headers.Get("X-RateLimit-Reset")), 10, 64); err == nil && value > 0 {
		var reset time.Time
		switch {
		case value >= 1_000_000_000_000:
			reset = time.UnixMilli(value)
		case value >= 1_000_000_000:
			reset = time.Unix(value, 0)
		default:
			reset = now.Add(time.Duration(value) * time.Second)
		}
		rate.ResetAt = &reset
	}
	if rate.Limit == 0 && rate.Remaining == 0 && rate.WindowSeconds == 0 && rate.ResetAt == nil {
		return nil
	}
	return rate
}

// wafSignatureProbes 是出站请求体中可能命中 Cloudflare WAF 托管内容规则（XSS/HTML 注入
// 规则族）的子串特征，全小写、大小写不敏感计数。用于 403 取证：验证「前端源码里的
// HTML/JS 标记文本触发拦截」假设（前端项目 100% 被拦、Java/Rust/Go 项目从不被拦，
// 唯一稳定变量就是 tool_result 回传的文件内容形状）。
var wafSignatureProbes = []string{
	"<script", "<iframe", "<svg", "<template", "<!doctype",
	"onerror=", "onload=", "onclick=", "onchange=", "javascript:", "v-on:", "@click",
}

// normalizeWafBody 做小写化并还原 Go json.Marshal 对 < > & 的六字符 unicode 转义，
// 供出站体签名计数使用。
func normalizeWafBody(outboundBody string) string {
	normalized := strings.ToLower(outboundBody)
	normalized = strings.ReplaceAll(normalized, "\\u003c", "<")
	normalized = strings.ReplaceAll(normalized, "\\u003e", ">")
	return strings.ReplaceAll(normalized, "\\u0026", "&")
}

// WafSignatureHitCount 统计出站请求体里 WAF 可疑特征的出现总数（0 = 出站体不含
// HTML/JS 注入类特征）。router 在网关 403 时据此区分两类拦截：>0 为内容型（内容
// 确定性命中 Cloudflare 托管规则，重试/换号/换 IP 必然复现，冷却账号只是白烧号池
// 容量）；0 为疑似风控型（评分/速率，账号维度的冷却仍有意义）。与 wafSignatureSummary
// 用同一特征表、同一归一化。
func WafSignatureHitCount(outboundBody string) int {
	if outboundBody == "" {
		return 0
	}
	normalized := normalizeWafBody(outboundBody)
	total := 0
	for _, probe := range wafSignatureProbes {
		total += strings.Count(normalized, probe)
	}
	return total
}

// wafSignatureSummary 统计出站请求体里各 WAF 可疑特征的出现次数，返回一行取证文本。
// 若 Cloudflare 是解码后匹配，被拦请求应大量出现特征；若零特征仍被拦，则说明拦截
// 另有诱因（IP/账号/速率），这正是取证要区分的问题。
func wafSignatureSummary(outboundBody string) string {
	if outboundBody == "" {
		return ""
	}
	normalized := normalizeWafBody(outboundBody)
	var hits []string
	total := 0
	for _, probe := range wafSignatureProbes {
		if n := strings.Count(normalized, probe); n > 0 {
			hits = append(hits, fmt.Sprintf("%s ×%d", probe, n))
			total += n
		}
	}
	if total == 0 {
		return "出站体特征: 未检出 HTML/JS 注入类特征——拦截诱因可能不是内容形状，建议排查 IP/账号/速率维度"
	}
	return "出站体特征(HTML/JS 注入类特征计数, 合计 " + strconv.Itoa(total) + "): " + strings.Join(hits, ", ")
}

// cloudflareRejectionDetail 汇总一条可读的 403 排查上下文：出站请求体大小、出站体里的
// HTML/JS 注入特征计数、Cloudflare Ray ID、命中的 WAF 规则头，以及拦截页正文里的关键行。
// 用于写入告警，让排查者不必翻日志就能判断诱因（前端源码标记触发内容规则、超大 body、
// 账号被封、规则误伤等）。outboundBody 是本次出站请求体（JSON 原文）。
func cloudflareRejectionDetail(status int, headers http.Header, respBody, outboundBody string) string {
	reqBodyBytes := len(outboundBody)
	var lines []string
	lines = append(lines, fmt.Sprintf("HTTP 状态: %d", status))
	lines = append(lines, fmt.Sprintf("出站请求体: %d 字节 (软告警阈值 %d 字节)", reqBodyBytes, MaxRequestBodyWarnBytes))
	if reqBodyBytes > MaxRequestBodyWarnBytes {
		lines = append(lines, "提示: 请求体超过软告警阈值，超大 payload 可能是触发 Cloudflare WAF 403 的加重因素之一（并非唯一诱因，需结合下方体积分布判断相关性）")
	}
	if sig := wafSignatureSummary(outboundBody); sig != "" {
		lines = append(lines, sig)
	}
	if ray := strings.TrimSpace(headers.Get("Cf-Ray")); ray != "" {
		lines = append(lines, "Cf-Ray: "+ray)
	}
	if mitigated := strings.TrimSpace(headers.Get("Cf-Mitigated")); mitigated != "" {
		lines = append(lines, "Cf-Mitigated: "+mitigated)
	}
	if snippet := cloudflareBodySnippet(respBody); snippet != "" {
		lines = append(lines, "响应体片段: "+snippet)
	}
	return strings.Join(lines, "\n")
}

// cloudflareBodySnippet 从 Cloudflare 拦截页/挑战页正文里提取最有信息量的一小段：
// 优先 <title>，否则截取首个非空文本行，控制在 300 字符内避免撑爆告警。
func cloudflareBodySnippet(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	lower := strings.ToLower(body)
	if i := strings.Index(lower, "<title>"); i >= 0 {
		if j := strings.Index(lower[i:], "</title>"); j >= 0 {
			title := strings.TrimSpace(body[i+len("<title>") : i+j])
			if title != "" {
				return truncateRunes(title, 300)
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateRunes(line, 300)
		}
	}
	return ""
}

func isCloudflareHTMLRejection(status int, headers http.Header) bool {
	return status == http.StatusForbidden &&
		strings.EqualFold(strings.TrimSpace(headers.Get("Server")), "cloudflare") &&
		strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/html")
}

// looksLikeHTML 判断一行流式内容是否是 HTML 文档开头。用于兜底识别未带
// text/html 头的 Cloudflare 拦截页（挑战/阻断），此时上游本应是 SSE(data: ...)。
func looksLikeHTML(line string) bool {
	s := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(s, "<!doctype html") ||
		strings.HasPrefix(s, "<html") ||
		strings.HasPrefix(s, "<head") ||
		strings.HasPrefix(s, "<!doctype")
}
