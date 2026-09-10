package router

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"ps2api/internal/provider"
)

// 出站即失败：缓存命中路径不应发生任何上游调用。
func fatalTransport(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("cache hit should not reach upstream, got %s %s", req.Method, req.URL)
		return nil, nil
	})}
}

func enableCache(t *testing.T, r *Router) {
	t.Helper()
	if err := r.Store.SetSetting("cache_enabled", "true"); err != nil {
		t.Fatal(err)
	}
}

func TestResponseCachePutGetLRUAndTTL(t *testing.T) {
	c := newResponseCache()
	res := provider.Result{Success: true, Content: "hi"}
	c.put("k", res)
	if got, ok := c.get("k"); !ok || got.Content != "hi" {
		t.Fatalf("expected hit, got ok=%v content=%q", ok, got.Content)
	}
	if _, ok := c.get("missing"); ok {
		t.Fatal("missing key should miss")
	}
	hits, misses, size := c.stats()
	if hits != 1 || misses != 1 || size != 1 {
		t.Fatalf("stats = %d/%d/%d, want 1/1/1", hits, misses, size)
	}

	// TTL：把在存条目改成已过期，get 应 miss 并清掉。
	c.mu.Lock()
	c.entries["k"].Value.(*cacheEntry).exp = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if _, ok := c.get("k"); ok {
		t.Fatal("expired entry should miss")
	}
	if _, _, size := c.stats(); size != 0 {
		t.Fatalf("expired entry should be evicted, size=%d", size)
	}

	// LRU：超过 cacheMaxEntries 淘汰最旧。
	for i := 0; i < cacheMaxEntries+5; i++ {
		c.put(string(rune('a'+i%26))+string(rune('a'+i/26))+string(rune('0'+i%10)), res)
	}
	if _, _, size := c.stats(); size != cacheMaxEntries {
		t.Fatalf("LRU should cap at %d, size=%d", cacheMaxEntries, size)
	}
}

func TestCacheDisabledByDefault(t *testing.T) {
	r := newTestRouter(t)
	req := &provider.ChatRequest{Model: "claude-sonnet-4-6", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")}}
	r.cachePut(req, &provider.Result{Success: true, Content: "hi"})
	if res := r.cacheGet(req); res != nil {
		t.Fatalf("cache off by default, got %+v", res)
	}
}

func TestCachePutOnlyStoresSuccessfulTextResponses(t *testing.T) {
	r := newTestRouter(t)
	enableCache(t, r)
	req := &provider.ChatRequest{Model: "claude-sonnet-4-6", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")}}

	r.cachePut(req, &provider.Result{Success: false, Content: "failed"})
	if res := r.cacheGet(req); res != nil {
		t.Fatal("failed response must not be cached")
	}
	r.cachePut(req, &provider.Result{Success: true, ToolCalls: []provider.ToolCall{{ID: "tc1"}}})
	if res := r.cacheGet(req); res != nil {
		t.Fatal("tool_calls response must not be cached (replay would break tool-tail continuation)")
	}

	// tool-tail 请求（续聊）不可缓存：IsCacheable 为 false。
	tailReq := &provider.ChatRequest{Model: "claude-sonnet-4-6", Messages: []provider.ChatMessage{
		mustMsg(t, "user", "hello"),
		{Role: "tool", ToolCallID: "tc1", Content: mustRaw(t, `"result"`)},
	}}
	r.cachePut(tailReq, &provider.Result{Success: true, Content: "ok"})
	if res := r.cacheGet(tailReq); res != nil {
		t.Fatal("tool-tail request must not be cached")
	}

	r.cachePut(req, &provider.Result{Success: true, Content: "hi"})
	if res := r.cacheGet(req); res == nil || !res.Cached || res.Content != "hi" {
		t.Fatalf("expected cached hit, got %+v", res)
	}
}

func TestChatCacheHitAvoidsUpstream(t *testing.T) {
	r := newTestRouter(t)
	enableCache(t, r)
	r.Provider.Client = fatalTransport(t)
	req := &provider.ChatRequest{Model: "claude-sonnet-4-6", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")}}
	r.cachePut(req, &provider.Result{Success: true, Content: "hi", PromptTokens: 10, CompletionTokens: 2})

	res, acc, err := r.Chat(context.Background(), req)
	if err != nil || acc != nil {
		t.Fatalf("cache hit should return without account: acc=%v err=%v", acc, err)
	}
	if res == nil || !res.Cached || res.Content != "hi" {
		t.Fatalf("expected cached result, got %+v", res)
	}
	if hits, _, _ := r.cache.stats(); hits != 1 {
		t.Fatalf("expected 1 cache hit, got %d", hits)
	}
}

func TestStreamCacheReplayEmitsContentAndFinish(t *testing.T) {
	r := newTestRouter(t)
	enableCache(t, r)
	r.Provider.Client = fatalTransport(t)
	req := &provider.ChatRequest{Model: "claude-sonnet-4-6", Messages: []provider.ChatMessage{mustMsg(t, "user", "hello")}}
	r.cachePut(req, &provider.Result{Success: true, Content: "streamed answer", ReasoningContent: "thinking"})

	var contents []string
	var finishes int
	res, acc, err := r.Stream(context.Background(), req, func(d provider.Delta) error {
		if d.HasFinish {
			finishes++
		} else if d.Content != "" {
			contents = append(contents, d.Content)
		}
		return nil
	})
	if err != nil || acc != nil {
		t.Fatalf("cache replay should succeed: acc=%v err=%v", acc, err)
	}
	if res == nil || !res.Cached {
		t.Fatalf("expected cached result, got %+v", res)
	}
	if len(contents) != 1 || contents[0] != "streamed answer" {
		t.Fatalf("expected one content delta, got %v", contents)
	}
	if finishes != 1 {
		t.Fatalf("expected exactly one finish delta, got %d", finishes)
	}
	if !strings.Contains(res.Content, "answer") {
		t.Fatal("result content should be preserved")
	}
}
