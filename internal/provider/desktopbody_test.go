package provider

import (
	"strings"
	"testing"
)

// TestBuildBodyDesktopLocalModeShape 钉住 desktop 分支的出站请求形态:必须与真实抓包里
// 能跑 executeShellCommand 的 localmode 会话一致(product/platform/hash)。改回 win32 或
// 云端 workspace_v12 都会让这里失败——避免静默丢失本地工具能力。
func TestBuildBodyDesktopLocalModeShape(t *testing.T) {
	p := New()
	req := &ChatRequest{Model: "gpt-5.6-sol", Endpoint: "openai",
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, "hi")}}}
	tokens := &Tokens{AccessToken: "tok", UserID: "u", WorkspaceID: "ws"}

	body := p.buildBody(req, tokens, "GPT_56_SOL", 1)

	if got := body["platform"]; got != "DESKTOP_MACOS" {
		t.Fatalf("platform = %v, want DESKTOP_MACOS", got)
	}
	input := body["input"].(map[string]interface{})
	if got := input["product"]; got != "workspace_localmode_v12" {
		t.Fatalf("product = %v, want workspace_localmode_v12", got)
	}
	ct := body["clientTools"].(map[string]interface{})
	hash, _ := ct["nativeToolsHash"].(string)
	if !strings.Contains(hash, "localmode") || !strings.Contains(hash, "darwin") {
		t.Fatalf("nativeToolsHash = %q, want localmode+darwin", hash)
	}
	excl, _ := ct["excludedTools"].([]string)
	if len(excl) == 0 {
		t.Fatal("excludedTools 为空,应对齐抓包里的 localmode 隐藏清单")
	}
}

// TestBuildBodyWebShape 确认非桌面(仅 PostmanSID)token 仍走 web 分支,不误用 localmode。
func TestBuildBodyWebShape(t *testing.T) {
	p := New()
	req := &ChatRequest{Model: "gpt-5.6-sol", Endpoint: "openai",
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, "hi")}}}
	tokens := &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "ws", WorkspaceSubdomain: "sub"}

	body := p.buildBody(req, tokens, "GPT_56_SOL", 1)

	if got := body["platform"]; got != "WEB" {
		t.Fatalf("platform = %v, want WEB", got)
	}
	input := body["input"].(map[string]interface{})
	if got := input["product"]; got != "workspace_v12" {
		t.Fatalf("product = %v, want workspace_v12", got)
	}
}
