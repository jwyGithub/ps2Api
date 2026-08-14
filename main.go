package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"postman2api-go/internal/api"
	"postman2api-go/internal/store"
)

func main() {
	// 端口只用专属变量 POSTMAN2API_PORT，避免通用 PORT 环境变量被其他程序占用时
	// 把服务代理到别的地方。
	port := env("POSTMAN2API_PORT", "1930")
	path := env("DATABASE_PATH", "./data/postman2api.db")
	key := env("API_KEY", "postman2api-secret-key")

	s, err := store.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	server := api.New(s, key)
	mux := http.NewServeMux()
	server.Register(mux)

	addr := ":" + port
	log.Printf("postman2api-go listening on http://localhost:%s", port)
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
		if r.URL.Path != "/health" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
