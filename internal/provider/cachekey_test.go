package provider

import "testing"

func TestCacheKeyStableAndDistinct(t *testing.T) {
	base := &ChatRequest{Model: "gpt-5.6-sol", Endpoint: "openai",
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, "hello")}}}
	same := &ChatRequest{Model: "gpt-5.6-sol", Endpoint: "openai",
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, "hello")}}}
	diff := &ChatRequest{Model: "gpt-5.6-sol", Endpoint: "openai",
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, "world")}}}

	if CacheKey(base) != CacheKey(same) {
		t.Fatal("identical requests must share a key")
	}
	if CacheKey(base) == CacheKey(diff) {
		t.Fatal("different content must produce different keys")
	}
}

func TestCacheKeyIgnoresTrailingTokenMetadata(t *testing.T) {
	plain := &ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: rawText(t, "hi")}}}
	withMeta := &ChatRequest{Model: "m", Messages: []ChatMessage{
		{Role: "user", Content: rawText(t, "hi")},
		{Role: "system", Content: rawText(t, "<total_tokens>123</total_tokens>")},
	}}
	if CacheKey(plain) != CacheKey(withMeta) {
		t.Fatal("trailing <total_tokens> volatile message must not change the key")
	}
}

func TestIsCacheableExcludesToolTail(t *testing.T) {
	single := &ChatRequest{Messages: []ChatMessage{{Role: "user", Content: rawText(t, "hi")}}}
	if !IsCacheable(single) {
		t.Fatal("single-shot user request must be cacheable")
	}
	toolTailReq := &ChatRequest{Messages: []ChatMessage{
		{Role: "user", Content: rawText(t, "hi")},
		{Role: "tool", ToolCallID: "call_1", Content: rawText(t, "result")},
	}}
	if IsCacheable(toolTailReq) {
		t.Fatal("tool-result tail is stateful and must not be cacheable")
	}
}
