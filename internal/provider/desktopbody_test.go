package provider

import (
	"net/http"
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
	if got := input["product"]; got != "api_catalog" {
		t.Fatalf("product = %v, want api_catalog", got)
	}
	if _, ok := input["startedFrom"]; ok {
		t.Fatal("startedFrom should not be sent by the current web client")
	}
	if got := body["mandatoryContext"].(map[string]interface{}); len(got) != 0 {
		t.Fatalf("mandatoryContext = %#v, want empty object", got)
	}
	kb := body["clientKBTerms"].(map[string]interface{})
	if got := kb["nativeTermsHash"]; got != nil {
		t.Fatalf("nativeTermsHash = %v, want null", got)
	}
	if got := kb["excludedKBTerms"].([]string); len(got) != 0 {
		t.Fatalf("excludedKBTerms = %#v, want empty list", got)
	}
	if got := body["clientTools"].(map[string]interface{})["nativeToolsHash"]; got != WebToolsHash {
		t.Fatalf("nativeToolsHash = %v, want %s", got, WebToolsHash)
	}
}

func TestWebSessionWinsWhenBothTokensArePresent(t *testing.T) {
	tokens := &Tokens{AccessToken: "access", PostmanSID: "sid", UserID: "u", WorkspaceID: "team", WorkspaceSubdomain: "sub"}
	if tokens.IsDesktop() {
		t.Fatal("postman.sid session should use the Web Agent Mode route")
	}
	h := New().buildHeaders(tokens)
	if got := h.Get("x-app-version"); got != WebAppVersion {
		t.Fatalf("x-app-version = %q, want %q", got, WebAppVersion)
	}
	if got := h.Get("Cookie"); got != "postman.sid=sid" {
		t.Fatalf("Cookie = %q, want postman.sid cookie", got)
	}
	if got := h.Get("x-access-token"); got != "access" {
		t.Fatalf("x-access-token = %q, want access token", got)
	}
}

func TestPostmanIdentityErrorDetectsSuccessfulHTTPAuthFailure(t *testing.T) {
	h := http.Header{}
	h.Set("X-Pm-Error-1", "identity_status: sessions returned 401")
	h.Set("X-Pm-Error-2", "guest_unusable: Jwt is missing")
	if got := postmanIdentityError(h); got == "" {
		t.Fatal("expected Postman identity failure from X-Pm-Error headers")
	}
}
