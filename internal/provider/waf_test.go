package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWafNeutralize 钉住中和语义：四类特征（标签/事件处理器/URI/Vue 简写）各自在
// 破坏点插入空格、大小写不敏感、无特征字符串零拷贝原样返回、中和后签名计数归零。
func TestWafNeutralize(t *testing.T) {
	cases := map[string]string{
		"<script>alert(1)</script>": "< script>alert(1)< /script>",
		"<SCRIPT SRC=x>":            "< SCRIPT SRC=x>",
		"OnLoad=go()":               "OnLoad =go()",
		"javascript:void(0)":        "javascript :void(0)",
		"v-on:click=fn":             "v-on :click=fn",
		"@click=fn":                 "@ click=fn",
		"<!doctype html>":           "< !doctype html>",
	}
	for in, want := range cases {
		if got := wafNeutralize(in); got != want {
			t.Fatalf("wafNeutralize(%q) = %q, want %q", in, got, want)
		}
	}
	// 无特征字符串必须原样返回（同一内容，避免白付一次拷贝）。
	clean := "func main() { fmt.Println(`hi`) }"
	if got := wafNeutralize(clean); got != clean {
		t.Fatalf("clean string should pass through, got %q", got)
	}
	// 中和作用于 marshal 之前的原始文本；转义发生在其后，由端到端测试覆盖。
	payload := `<div><script src=x onerror=alert(1)></div> <img onload=pwn> javascript: x @click=f`
	if n := WafSignatureHitCount(wafNeutralize(payload)); n != 0 {
		t.Fatalf("neutralized payload still has %d signature hits: %s", n, wafNeutralize(payload))
	}
}

func TestWafNeutralizeKillSwitch(t *testing.T) {
	t.Setenv("GATEWAY_DISABLE_WAF_NEUTRALIZE", "1")
	if wafNeutralizeEnabled() {
		t.Fatal("GATEWAY_DISABLE_WAF_NEUTRALIZE=1 should disable neutralization")
	}
	t.Setenv("GATEWAY_DISABLE_WAF_NEUTRALIZE", "")
	if !wafNeutralizeEnabled() {
		t.Fatal("neutralization should be enabled by default")
	}
}

// TestBuildBodyNeutralizesOutboundQuery 端到端钉住：带前端源码的消息经 buildBody 序列化后，
// 出站体签名计数必须为 0（覆盖冷启动折叠路径），且中和形态对模型可读（< script）。
// 指纹不受影响由结构保证：中和只作用于出站副本，req.Messages 原文不被改写。
func TestBuildBodyNeutralizesOutboundQuery(t *testing.T) {
	p := New()
	req := &ChatRequest{Model: "gpt-5.6-sol", Endpoint: "openai", Messages: []ChatMessage{
		{Role: "user", Content: rawText(t, "帮我修复这个组件\n<template><script src=x onerror=alert(1)></script><svg onload=pwn></svg></template>")},
	}}
	tokens := &Tokens{AccessToken: "tok", UserID: "u", WorkspaceID: "ws"}

	body := p.buildBody(req, tokens, "GPT_56_SOL", 1)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if n := WafSignatureHitCount(string(b)); n != 0 {
		t.Fatalf("outbound body should be WAF-neutralized, got %d signature hits", n)
	}
	query := body["input"].(map[string]interface{})["query"].(string)
	if !strings.Contains(query, "< script src=x onerror =alert(1)") {
		t.Fatalf("neutralized form should keep model-readable spacing, got: %s", query)
	}
	// req.Messages 原文必须保持未被中和（指纹层铁律）。
	if strings.Contains(string(req.Messages[0].Content), "< script") {
		t.Fatal("req.Messages must never be mutated by outbound neutralization")
	}
}
