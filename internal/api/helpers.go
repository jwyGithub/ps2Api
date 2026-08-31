// helpers.go —— 包内共享的小工具：JSON 写出（jsonWrite/jsonError）、
// 协议专属错误体（anthropicError/openAIError/protoError）、
// SSE 帧写出（sse）、mustJSON、ID 生成（newID）与时间戳（nowUnix）。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ps2api/internal/router"
)

// routeErrorStatus 把 router 返回的错误映射为对外 HTTP 状态码与 Anthropic 错误类型。
// 会话/请求内容类错误(RouteError.Rejected：坏请求、工具名冲突、TOOL_CALL_NOT_FOUND 等)
// 返回 400 invalid_request_error——重试/换号都不会成功，让客户端 SDK 立即停止；其余上游失败
// 沿用 529 overloaded_error(表达「暂时不可用」，SDK 会退避重试)。这样修正的是过度宽泛的
// 529 映射，不推翻「暂时不可用才用 529」的协议意图。
func routeErrorStatus(err error) (int, string) {
	var re *router.RouteError
	if errors.As(err, &re) && re.Rejected {
		return 400, "invalid_request_error"
	}
	return 529, "overloaded_error"
}

// upstreamErrorStatus 为对话端点的上游失败挑选 HTTP 状态码：网关错误（上游 Cloudflare
// 风控 403，RouteError.GatewayBlocked）返回 529（表达「上游暂时不可用」，且不再重试）；
// 其余上游失败沿用 503。
func upstreamErrorStatus(err error) int {
	var re *router.RouteError
	if errors.As(err, &re) && re.GatewayBlocked {
		return 529
	}
	return 503
}

func jsonWrite(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonError 是管理面板端点（/api/*）的错误体格式。对话类端点（/v1/*）不要用它——
// 那里必须按各自协议规范走 anthropicError / openAIError，否则客户端 SDK 解析不出错误。
func jsonError(w http.ResponseWriter, status int, msg, typ string) {
	jsonWrite(w, status, map[string]interface{}{"error": map[string]string{"message": msg, "type": typ}})
}

// anthropicError 按 Anthropic 规范写出错误：顶层必须带 type:"error"，
// 缺了它客户端 SDK 无法把响应识别为错误对象。typ 必须取自 Anthropic 的错误类型
// 枚举（invalid_request_error / authentication_error / permission_error /
// not_found_error / request_too_large / rate_limit_error / api_error /
// overloaded_error）——枚举外的值会让类型化 SDK 解析失败。
func anthropicError(w http.ResponseWriter, status int, msg, typ string) {
	jsonWrite(w, status, map[string]interface{}{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}

// openAIError 按 OpenAI 规范写出错误：error 对象固定带 param 与 code 两个键
// （无值时为 null）。code 可选，只有需要机器可读细分原因时才传（如 invalid_api_key）。
func openAIError(w http.ResponseWriter, status int, msg, typ string, code ...string) {
	errObj := map[string]interface{}{"message": msg, "type": typ, "param": nil, "code": nil}
	if len(code) > 0 && code[0] != "" {
		errObj["code"] = code[0]
	}
	jsonWrite(w, status, map[string]interface{}{"error": errObj})
}

// unsupportedMediaMessage 是三个对话端点共用的「上游不接受该媒体类型」错误文案。
// 上游 Postman /chat 只有一个纯文本 query 字段，没有任何图片/附件通道
// （判定依据见 provider.unsupportedMediaKinds 的注释与 provider/imgprobe_test.go）。
// 用 400 而不是 5xx：这是请求内容与上游能力不匹配，重试永远不会成功，客户端 SDK
// 见到 4xx 会立即停止而不是自动重试。文案要能直接指导调用方改请求。
func unsupportedMediaMessage(kind string) string {
	return kind + " input is not supported: the upstream provider accepts text only and has no " +
		"attachment channel. Remove the " + kind + " from the request, or paste the relevant " +
		"content as text instead."
}

// protoError 供被两种协议共享的代码（auth、traceChat）使用：按请求路径选择错误体格式。
// 两家的错误体结构与类型枚举都不同，共用一种会让其中一方的 SDK 解析失败。
// 路径判定与 traceChat 里的 endpoint 判定保持一致。
func protoError(w http.ResponseWriter, r *http.Request, status int, msg, anthropicType, openAIType string, openAICode ...string) {
	if strings.HasSuffix(r.URL.Path, "/messages") {
		anthropicError(w, status, msg, anthropicType)
		return
	}
	openAIError(w, status, msg, openAIType, openAICode...)
}
func sse(w http.ResponseWriter, fl http.Flusher, v interface{}) error {
	_, e := fmt.Fprintf(w, "data: %s\n\n", mustJSON(v))
	fl.Flush()
	return e
}
func mustJSON(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

// inboundHeadersJSON 把客户端入站请求头序列化为 JSON 原文，供请求日志排查。
// 单值头拍平为字符串，多值头保留数组；空头返回空串。
func inboundHeadersJSON(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	flat := make(map[string]interface{}, len(h))
	for k, v := range h {
		if len(v) == 1 {
			flat[k] = v[0]
		} else {
			flat[k] = v
		}
	}
	b, err := json.Marshal(flat)
	if err != nil {
		return ""
	}
	return string(b)
}
func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
func nowUnix() int64 { return time.Now().Unix() }
