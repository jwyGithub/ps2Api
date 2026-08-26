package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type traceContextKey struct{}
type traceEndpointKey struct{}

var traceMu sync.Mutex

func TraceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GATEWAY_TRACE_LOG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// endpoint 用作日志子目录（如 openai/anthropic），按调用方式分开存储。
func NewTraceContext(ctx context.Context, endpoint string) (context.Context, string) {
	// 始终生成 trace_id 作为整条请求链路的关联 id（控制台链路日志恒可用）；
	// 是否额外落 jsonl 文件深追踪，仍由 GATEWAY_TRACE_LOG 控制（见 Trace）。
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ctx, ""
	}
	id := hex.EncodeToString(raw[:])
	ctx = context.WithValue(ctx, traceContextKey{}, id)
	ctx = context.WithValue(ctx, traceEndpointKey{}, endpoint)
	return ctx, id
}

func Trace(ctx context.Context, event string, data interface{}) {
	id, _ := ctx.Value(traceContextKey{}).(string)
	if id == "" {
		return
	}
	clean := sanitizeTrace(data)
	// 控制台链路 sink：默认全量、单行、带 trace_id 前缀，便于实时溯源与 grep。
	traceConsole(id, event, clean)
	// 文件深追踪（完整请求/响应体）仍按需开启，避免默认每请求写盘。
	if !TraceEnabled() {
		return
	}
	record := map[string]interface{}{
		"time":     time.Now().Format(time.RFC3339Nano),
		"trace_id": id,
		"event":    event,
		"data":     clean,
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	dir := strings.TrimSpace(os.Getenv("GATEWAY_TRACE_DIR"))
	if dir == "" {
		dir = "./data/traces"
	}
	// 按调用方式分目录。endpoint 来自内部常量，filepath.Base 兜底防路径穿越。
	if ep, _ := ctx.Value(traceEndpointKey{}).(string); ep != "" {
		dir = filepath.Join(dir, filepath.Base(ep))
	}
	dateDir := filepath.Join(dir, time.Now().Format("2006-01-02"))
	traceMu.Lock()
	defer traceMu.Unlock()
	if err := os.MkdirAll(dateDir, 0o700); err != nil {
		log.Printf("WARN: 创建追踪日志目录失败: %v", err)
		return
	}
	path := filepath.Join(dateDir, id+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("WARN: 打开追踪日志失败: %v", err)
		return
	}
	_, err = file.Write(append(line, '\n'))
	_ = file.Close()
	if err != nil {
		log.Printf("WARN: 写入追踪日志失败: %v", err)
	}
}

func sanitizeTrace(value interface{}) interface{} {
	raw, err := json.Marshal(value)
	if err != nil {
		return "[unserializable]"
	}
	var decoded interface{}
	if json.Unmarshal(raw, &decoded) != nil {
		return "[unserializable]"
	}
	return sanitizeTraceValue(decoded)
}

func sanitizeTraceValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, item := range v {
			if sensitiveTraceKey(key) {
				v[key] = "[REDACTED]"
			} else {
				v[key] = sanitizeTraceValue(item)
			}
		}
		return v
	case []interface{}:
		for i := range v {
			v[i] = sanitizeTraceValue(v[i])
		}
		return v
	case string:
		return sanitizeTraceString(v)
	default:
		return value
	}
}

func sensitiveTraceKey(key string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
	switch normalized {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "password",
		"accesstoken", "postmansid", "apikey", "xapikey", "xaccesstoken":
		return true
	default:
		return false
	}
}

func sanitizeTraceString(value string) interface{} {
	trimmed := strings.TrimSpace(value)
	if json.Valid([]byte(trimmed)) {
		var decoded interface{}
		if json.Unmarshal([]byte(trimmed), &decoded) == nil {
			return sanitizeTraceValue(decoded)
		}
	}
	if !strings.Contains(value, "data: ") {
		return value
	}
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		prefix := "data: "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		payload := strings.TrimPrefix(line, prefix)
		if !json.Valid([]byte(payload)) {
			continue
		}
		var decoded interface{}
		if json.Unmarshal([]byte(payload), &decoded) == nil {
			var buf bytes.Buffer
			_ = json.NewEncoder(&buf).Encode(sanitizeTraceValue(decoded))
			lines[i] = prefix + strings.TrimSpace(buf.String())
		}
	}
	return strings.Join(lines, "\n")
}

// ————————————————————————————————————————————————————————————————
// 控制台链路 sink：把关键链路事件实时打到控制台（stderr，经 log），
// 单行、带短 trace_id 前缀，可 grep 出完整请求链路。完整请求/响应体
// 仍只进 jsonl 文件（GATEWAY_TRACE_LOG），控制台只打摘要。
// 详细度由 GATEWAY_LOG_LEVEL 控制：debug|info|warn|error|off，默认 debug（全量）。

const (
	logDebug = iota
	logInfo
	logWarn
	logError
	logOff
)

const (
	consoleValueMaxRunes = 200 // 单个字段值最长
	consoleLineMaxRunes  = 500 // 整行最长
)

// consoleLevel 返回控制台最低输出级别，默认 debug（全量到控制台）。
func consoleLevel() int {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GATEWAY_LOG_LEVEL"))) {
	case "info":
		return logInfo
	case "warn", "warning":
		return logWarn
	case "error", "err":
		return logError
	case "off", "none", "silent":
		return logOff
	default: // debug 或未设置
		return logDebug
	}
}

// eventLevel 把链路事件映射到日志级别，方便按严重程度着色/过滤。
func eventLevel(event string) int {
	switch event {
	case "router.error", "client.request.error", "vision.error", "ocr.error":
		return logError
	case "router.failure", "router.gateway_blocked", "router.gateway_sticky_retry",
		"router.request_rejected", "upstream.request.oversize":
		return logWarn
	default:
		return logInfo
	}
}

func levelName(l int) string {
	switch l {
	case logError:
		return "ERROR"
	case logWarn:
		return "WARN"
	case logDebug:
		return "DEBUG"
	default:
		return "INFO"
	}
}

func traceConsole(id, event string, clean interface{}) {
	// 流式响应体按 write 逐块上报，进控制台会刷屏；只留文件里的完整体，
	// 控制台靠 upstream.complete / provider.result / 访问日志行给出结论。
	if event == "client.response.body" {
		return
	}
	lvl := eventLevel(event)
	if lvl < consoleLevel() {
		return
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	log.Printf("[%s] %-5s %-26s %s", short, levelName(lvl), event, consoleKV(clean))
}

// consoleKV 把事件 data（已脱敏）压成单行 key=val；body/headers/content 这类
// 大字段只显示字节数，避免刷屏。
func consoleKV(clean interface{}) string {
	m, ok := clean.(map[string]interface{})
	if !ok {
		raw, _ := json.Marshal(clean)
		return truncateRunes(strings.ReplaceAll(string(raw), "\n", " "), consoleLineMaxRunes)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		var vs string
		switch strings.ToLower(k) {
		case "body", "headers", "content", "messages":
			vs = strconv.Itoa(consoleValueSize(m[k])) + "b"
		default:
			vs = consoleValue(m[k])
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(vs)
	}
	return truncateRunes(b.String(), consoleLineMaxRunes)
}

func consoleValue(v interface{}) string {
	switch t := v.(type) {
	case string:
		return truncateRunes(strings.ReplaceAll(t, "\n", " "), consoleValueMaxRunes)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return "null"
	default:
		raw, _ := json.Marshal(t)
		return truncateRunes(strings.ReplaceAll(string(raw), "\n", " "), consoleValueMaxRunes)
	}
}

func consoleValueSize(v interface{}) int {
	if s, ok := v.(string); ok {
		return len(s)
	}
	raw, _ := json.Marshal(v)
	return len(raw)
}
