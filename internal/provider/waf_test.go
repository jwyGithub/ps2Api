package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// zwsp 是零宽空格 U+200B，用 rune 构造保持源码纯 ASCII（字面零宽字符会被
// 编辑器/评审/JSON 传输悄悄吞掉或污染）。wafNeutralize 的破坏点全部插这个字符。
var zwsp = string(rune(0x200b))

// TestWafNeutralize 钉住中和语义：四类特征（标签/事件处理器/URI/Vue 简写）各自在
// 破坏点插入零宽空格、大小写不敏感、无特征字符串零拷贝原样返回、中和后签名计数归零。
func TestWafNeutralize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<script>alert(1)</script>", "<" + zwsp + "script>alert(1)<" + zwsp + "/script>"},
		{"<SCRIPT SRC=x>", "<" + zwsp + "SCRIPT SRC=x>"},
		{"OnLoad=go()", "OnLoad" + zwsp + "=go()"},
		{"javascript:void(0)", "javascript" + zwsp + ":void(0)"},
		{"v-on:click=fn", "v-on" + zwsp + ":click=fn"},
		{"@click=fn", "@" + zwsp + "click=fn"},
		{"<!doctype html>", "<" + zwsp + "!doctype html>"},
		// 只有闭合标签的载荷：签名探测表认不出（"<script" 不匹配 "</script"），
		// 中和不得依赖探测门（实测 CF 对 "</script>" 单独在场即 403）。
		{"</script>", "<" + zwsp + "/script>"},
		// 转义形：wrap 的 json.Marshal / 客户端双重编码产生的 "<" 字面量转义序列。
		{"\\u003cscript\\u003ex\\u003c/script\\u003e",
			"\\u003c" + zwsp + "script\\u003ex\\u003c" + zwsp + "/script\\u003e"},
		// bin/cat 形（2026-09-11 实测）：cat 前缀词跟在 bin/ 后被 Cloudflare 当作
		// cat 命令执行路径（./bin/catpaw2api、/bin/cat、大写均 403；bin/ls、bin/sh、
		// bin/rm、bin/python、bin/curl、./cat、裸 cat 均放行——cat 是唯一触发命令）。
		{"./bin/catpaw2api -config config.json", "./bin/" + zwsp + "catpaw2api -config config.json"},
		{"/bin/cat /etc/passwd", "/bin/" + zwsp + "cat /etc/passwd"},
		{"./bin/Catpaw2api", "./bin/" + zwsp + "Catpaw2api"},
		{"catpaw2api -config config.json", "catpaw2api -config config.json"}, // 无 bin/ 前缀不误伤
		{"./bin/ls -la", "./bin/ls -la"},                                     // 非 cat 命令不误伤
	}
	for _, c := range cases {
		if got := wafNeutralize(c.in); got != c.want {
			t.Fatalf("wafNeutralize(%q) = %q, want %q", c.in, got, c.want)
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
// 出站体签名计数必须为 0（覆盖冷启动折叠路径），且中和形态对模型可读（零宽空格对
// tokenizer 基本不可见）。指纹不受影响由结构保证：中和只作用于出站副本，req.Messages
// 原文不被改写。
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
	if !strings.Contains(query, "<"+zwsp+"script src=x onerror"+zwsp+"=alert(1)") {
		t.Fatalf("neutralized form should insert ZWSP at signature boundaries, got: %q", query)
	}
	// req.Messages 原文必须保持未被中和（指纹层铁律）。
	if strings.Contains(string(req.Messages[0].Content), "<"+zwsp+"script") {
		t.Fatal("req.Messages must never be mutated by outbound neutralization")
	}
}

// TestNativeToolResponseNeutralizesWrappedPayload 钉住 wrap 路径的中和顺序：非 JSON 的
// tool result（前端源码）会被 json.Marshal 包一层 message，marshal 的 HTML 转义曾让
// "<" 字面量转义序列逃过裸文本正则（实测 tool result 的 403 泄漏根因），中和必须在
// wrap 之前完成。种入会话映射 + 工具组映射，驱动完整 nativeToolResponse 路径。
func TestNativeToolResponseNeutralizesWrappedPayload(t *testing.T) {
	p := New()
	messages := []ChatMessage{
		{Role: "user", Content: rawText(t, "修复这个组件")},
		{Role: "assistant", Content: rawText(t, ""), ToolCalls: rawText(t,
			`[{"id":"call_1","type":"function","function":{"name":"readFile","arguments":"{}"}}]`)},
		{Role: "tool", ToolCallID: "call_1", Content: rawText(t,
			"1\t<template><script setup lang=\"ts\">const x = 1</script></template>")},
	}
	p.convStore.PutConversation(1, conversationFingerprint(messages[:1]), "conv_1")
	p.convStore.PutToolGroup(1, "call_1", "group_1")

	resp, ok := p.nativeToolResponse(1, messages)
	if !ok {
		t.Fatal("nativeToolResponse should build with seeded conversation + tool group")
	}
	payload, _ := resp.responses[0]["content"].(string)
	if n := WafSignatureHitCount(payload); n != 0 {
		t.Fatalf("wrapped payload must be neutralized before marshal, got %d hits: %s", n, payload)
	}
	// wrap 会把 < 转成 "<" 字面量转义序列；破坏点应落在转义序列与 "script" 之间。
	if !strings.Contains(payload, "\\u003c"+zwsp+"script") {
		t.Fatalf("wrapped payload should contain ZWSP-broken script tag, got: %s", payload)
	}
}

// TestWafSignatureProbesAndCounts 钉住导出接口：列表与内部表一致（外部拿到的副本
// 不可被改写），逐特征计数只含命中项且对 json.Marshal 转义形归一化后计数。
func TestWafSignatureProbesAndCounts(t *testing.T) {
	probes := WafSignatureProbes()
	if len(probes) == 0 {
		t.Fatal("WafSignatureProbes should not be empty")
	}
	// 副本不可变：改外部切片不得影响内部表
	probes[0] = "mutated"
	if WafSignatureProbes()[0] == "mutated" {
		t.Fatal("WafSignatureProbes must return a copy")
	}
	// 计数：bin/cat ×1、onerror= ×1，其余不出现；转义形 <script 也计数
	body := `{"q":"./bin/catpaw2api -config x","h":"<img onerror=alert(1)>","e":"\\u003cscript\\u003e"}`
	got := WafSignatureCounts(body)
	if got["bin/cat"] != 1 {
		t.Fatalf("bin/cat count = %d, want 1: %v", got["bin/cat"], got)
	}
	if got["onerror="] != 1 {
		t.Fatalf("onerror= count = %d, want 1: %v", got["onerror="], got)
	}
	if got["<script"] != 1 {
		t.Fatalf("escaped <script should count as 1, got %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("only nonzero probes expected, got %v", got)
	}
	if n := len(WafSignatureCounts("干净文本，无特征")); n != 0 {
		t.Fatalf("clean body should give empty counts, got %d", n)
	}
	if n := len(WafSignatureCounts("")); n != 0 {
		t.Fatalf("empty body should give empty counts, got %d", n)
	}
	// 与既有总数口径一致：各特征计数之和 == WafSignatureHitCount
	if total := WafSignatureHitCount(body); got["bin/cat"]+got["onerror="]+got["<script"] != total {
		t.Fatalf("per-probe sum should equal WafSignatureHitCount: %v vs %d", got, total)
	}
}

// TestBuildBodyWafProbeBypass 钉住探针旁路：WafProbe 请求的出站 query 原样保留——
// 不中和（中和会掐灭待验证的已知特征，叶子轮必然假阴性）、不截断（二分切片必须
// padding 到与原叶子等长，截断破坏等长方法论）。非探针请求行为不变。
func TestBuildBodyWafProbeBypass(t *testing.T) {
	p := New()
	tokens := &Tokens{AccessToken: "tok", UserID: "u", WorkspaceID: "ws"}
	// 大于 10000 rune 验证截断旁路；含 <script> 验证中和旁路。
	long := "<script>alert(1)</script>" + strings.Repeat("x", 10100)

	probeReq := &ChatRequest{Model: "claude-haiku-4-5", WafProbe: true,
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, long)}}}
	probeQuery := p.buildBody(probeReq, tokens, "CLAUDE_HAIKU", 1)["input"].(map[string]interface{})["query"].(string)
	if probeQuery != long {
		t.Fatalf("probe query must be verbatim (no neutralize, no cap): got %d bytes, want %d", len(probeQuery), len(long))
	}

	normalReq := &ChatRequest{Model: "claude-haiku-4-5",
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, long)}}}
	normalQuery := p.buildBody(normalReq, tokens, "CLAUDE_HAIKU", 1)["input"].(map[string]interface{})["query"].(string)
	if len(normalQuery) >= len(long) {
		t.Fatalf("normal request should stay capped, got %d bytes", len(normalQuery))
	}
	if n := WafSignatureHitCount(normalQuery); n != 0 {
		t.Fatalf("normal request should stay neutralized, got %d hits", n)
	}
}
