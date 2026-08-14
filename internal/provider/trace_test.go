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
	ctx, id := NewTraceContext(context.Background())
	if id == "" {
		t.Fatal("trace id was not created")
	}
	Trace(ctx, "test", map[string]interface{}{
		"headers": map[string]interface{}{"Authorization": "Bearer secret-key", "Cookie": "postman.sid=session-secret"},
		"body":    `{"password":"password-secret","tokens":{"access_token":"token-secret"},"message":"keep me"}`,
	})
	files, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
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

func TestTraceDisabledDoesNotCreateID(t *testing.T) {
	t.Setenv("GATEWAY_TRACE_LOG", "0")
	_, id := NewTraceContext(context.Background())
	if id != "" {
		t.Fatalf("disabled trace created id %q", id)
	}
}

func TestEachTraceUsesItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GATEWAY_TRACE_LOG", "1")
	t.Setenv("GATEWAY_TRACE_DIR", dir)
	ctx1, id1 := NewTraceContext(context.Background())
	ctx2, id2 := NewTraceContext(context.Background())
	Trace(ctx1, "first", nil)
	Trace(ctx2, "second", nil)
	files, err := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
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
}
