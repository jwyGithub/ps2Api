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
	"strings"
	"sync"
	"time"
)

type traceContextKey struct{}

var traceMu sync.Mutex

func TraceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GATEWAY_TRACE_LOG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func NewTraceContext(ctx context.Context) (context.Context, string) {
	if !TraceEnabled() {
		return ctx, ""
	}
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ctx, ""
	}
	id := hex.EncodeToString(raw[:])
	return context.WithValue(ctx, traceContextKey{}, id), id
}

func Trace(ctx context.Context, event string, data interface{}) {
	id, _ := ctx.Value(traceContextKey{}).(string)
	if id == "" {
		return
	}
	record := map[string]interface{}{
		"time":     time.Now().Format(time.RFC3339Nano),
		"trace_id": id,
		"event":    event,
		"data":     sanitizeTrace(data),
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	dir := strings.TrimSpace(os.Getenv("GATEWAY_TRACE_DIR"))
	if dir == "" {
		dir = "./data/traces"
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
