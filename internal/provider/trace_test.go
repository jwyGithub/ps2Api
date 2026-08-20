package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceWritesJSONLAndRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GATEWAY_TRACE_LOG", "1")
	t.Setenv("GATEWAY_TRACE_DIR", dir)
	ctx, id := NewTraceContext(context.Background(), "openai")
	if id == "" {
		t.Fatal("trace id was not created")
	}
	Trace(ctx, "test", map[string]interface{}{
		"headers": map[string]interface{}{"Authorization": "Bearer secret-key", "Cookie": "postman.sid=session-secret"},
		"body":    `{"password":"password-secret","tokens":{"access_token":"token-secret"},"message":"keep me"}`,
	})
	files, err := filepath.Glob(filepath.Join(dir, "openai", "*", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("trace files = %v, %v", files, err)
	}
	if filepath.Base(files[0]) != id+".jsonl" {
		t.Fatalf("trace file does not match trace id: %s", files[0])
	}
	content, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, secret := range []string{"secret-key", "session-secret", "password-secret", "token-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("trace leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, id) || !strings.Contains(text, "keep me") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("unexpected trace: %s", text)
	}
}

// 关闭 GATEWAY_TRACE_LOG 时：仍生成 trace_id（供控制台链路日志关联），
// 但不落 jsonl 文件深追踪。
func TestTraceDisabledStillMintsIDButWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GATEWAY_TRACE_LOG", "0")
	t.Setenv("GATEWAY_TRACE_DIR", dir)
	ctx, id := NewTraceContext(context.Background(), "openai")
	if id == "" {
		t.Fatal("trace id should always be created for console correlation")
	}
	Trace(ctx, "test", map[string]interface{}{"body": "hello"})
	files, err := filepath.Glob(filepath.Join(dir, "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("file tracing disabled but jsonl written: %v", files)
	}
}

func TestEachTraceUsesItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GATEWAY_TRACE_LOG", "1")
	t.Setenv("GATEWAY_TRACE_DIR", dir)
	ctx1, id1 := NewTraceContext(context.Background(), "openai")
	ctx2, id2 := NewTraceContext(context.Background(), "anthropic")
	Trace(ctx1, "first", nil)
	Trace(ctx2, "second", nil)
	files, err := filepath.Glob(filepath.Join(dir, "*", "*", "*.jsonl"))
	if err != nil || len(files) != 2 {
		t.Fatalf("trace files = %v, %v", files, err)
	}
	seen := map[string]bool{}
	for _, file := range files {
		seen[filepath.Base(file)] = true
	}
	if !seen[id1+".jsonl"] || !seen[id2+".jsonl"] {
		t.Fatalf("trace ids do not have separate files: %v", files)
	}
	// 按调用方式分目录：openai/anthropic 各自落在自己的子目录下。
	if _, err := os.Stat(filepath.Join(dir, "openai")); err != nil {
		t.Fatalf("openai trace dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "anthropic")); err != nil {
		t.Fatalf("anthropic trace dir missing: %v", err)
	}
}
