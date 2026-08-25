// helpers.go —— 包内共享的小工具：JSON 写出（jsonWrite/jsonError）、
// 协议专属错误体（anthropicError/openAIError/protoError）、
// SSE 帧写出（sse）、mustJSON、ID 生成（newID）与时间戳（nowUnix）。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

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
func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
func nowUnix() int64 { return time.Now().Unix() }
