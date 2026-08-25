package main

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"ps2api/internal/api"
	"ps2api/internal/store"
)

func main() {
	// 端口只用专属变量 GATEWAY_PORT，避免通用 PORT 环境变量被其他程序占用时
	// 把服务代理到别的地方。
	port := env("GATEWAY_PORT", "1930")
	path := env("DATABASE_PATH", "./data/gateway.db")

	s, err := store.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	// API Key 不再来自环境变量：改为面板设置并持久化到 SQLite，auth() 每次从库读取。
	server := api.New(s)
	mux := http.NewServeMux()
	server.Register(mux)

	addr := ":" + port
	log.Printf("ps2api listening on http://localhost:%s", port)
	log.Printf("会话存储: %s", server.Router.Provider.ConversationStorageMode())
	log.Printf("OpenAI: http://localhost:%s/v1/chat/completions", port)
	log.Printf("Anthropic: http://localhost:%s/v1/messages", port)
	if err := http.ListenAndServe(addr, logging(mux)); err != nil {
		log.Fatal(err)
	}
}

func env(k, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return fallback
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		rec := &logRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		dur := time.Since(started).Milliseconds()
		// 带上 trace_id 前缀，和链路日志（[<trace_id>] ...）对齐，一眼定位完整调用链。
		prefix := ""
		if tid := rec.Header().Get("X-PS2API-Trace-ID"); tid != "" {
			if len(tid) > 8 {
				tid = tid[:8]
			}
			prefix = "[" + tid + "] "
		}
		if rec.status >= 400 {
			// 失败时把响应体（JSON error 的 message）一并打出来，便于定位原因。
			log.Printf("%s%s %s -> %d (%dms) %s", prefix, r.Method, r.URL.Path, rec.status, dur, strings.TrimSpace(rec.errBody.String()))
		} else {
			log.Printf("%s%s %s -> %d (%dms)", prefix, r.Method, r.URL.Path, rec.status, dur)
		}
	})
}

// logRecorder 记录状态码，并在响应为错误（status>=400）时缓存前 2KB 响应体，
// 供 logging 中间件打印失败原因。透传 Flush 以保持 SSE 流式不被缓冲。
type logRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	errBody     bytes.Buffer
}

func (l *logRecorder) WriteHeader(status int) {
	if l.wroteHeader {
		return
	}
	l.wroteHeader = true
	l.status = status
	l.ResponseWriter.WriteHeader(status)
}

func (l *logRecorder) Write(b []byte) (int, error) {
	if !l.wroteHeader {
		l.WriteHeader(200)
	}
	if l.status >= 400 && l.errBody.Len() < 2048 {
		l.errBody.Write(b[:min(len(b), 2048-l.errBody.Len())])
	}
	return l.ResponseWriter.Write(b)
}

func (l *logRecorder) Flush() {
	if fl, ok := l.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}
