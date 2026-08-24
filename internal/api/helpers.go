// helpers.go —— 包内共享的小工具：JSON 写出（jsonWrite/jsonError）、
// SSE 帧写出（sse）、mustJSON、ID 生成（newID）与时间戳（nowUnix）。
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func jsonWrite(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func jsonError(w http.ResponseWriter, status int, msg, typ string) {
	jsonWrite(w, status, map[string]interface{}{"error": map[string]string{"message": msg, "type": typ}})
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
