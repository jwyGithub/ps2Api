package provider

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

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

// cloudflareRejectionDetail 汇总一条可读的 403 排查上下文：出站请求体大小、
// Cloudflare Ray ID、命中的 WAF 规则头，以及拦截页正文里的关键行。用于写入告警，
// 让排查者不必翻日志就能判断诱因（如超大 body 触发 WAF、账号被封、规则误伤等）。
func cloudflareRejectionDetail(status int, headers http.Header, body string, reqBodyBytes int) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("HTTP 状态: %d", status))
	lines = append(lines, fmt.Sprintf("出站请求体: %d 字节 (软告警阈值 %d 字节)", reqBodyBytes, MaxRequestBodyWarnBytes))
	if reqBodyBytes > MaxRequestBodyWarnBytes {
		lines = append(lines, "提示: 请求体超过软告警阈值，超大 payload 可能是触发 Cloudflare WAF 403 的加重因素之一（并非唯一诱因，需结合下方体积分布判断相关性）")
	}
	if ray := strings.TrimSpace(headers.Get("Cf-Ray")); ray != "" {
		lines = append(lines, "Cf-Ray: "+ray)
	}
	if mitigated := strings.TrimSpace(headers.Get("Cf-Mitigated")); mitigated != "" {
		lines = append(lines, "Cf-Mitigated: "+mitigated)
	}
	if snippet := cloudflareBodySnippet(body); snippet != "" {
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
