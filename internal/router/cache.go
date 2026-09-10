package router

import (
	"container/list"
	"strconv"
	"sync"
	"time"

	"ps2api/internal/provider"
)

// 网关响应缓存：键复用探针指纹 provider.CacheKey，只缓存「单发无状态请求」的
// 「成功且无 tool_calls」响应。命中即回放，零上游调用——Postman 按次扣额度，
// 一次命中 = 省一整次上游调用。刻意不缓存带 tool_calls 的响应：回放后客户端的
// tool-tail 续接需要真实上游调用产生的会话映射(conversationId)，缓存回放给不出。
// ponytail: 内存 LRU + TTL，单实例；跨实例共享(Redis)等命中率数据证明不够再说。
const (
	cacheMaxEntries = 512
	cacheTTL        = 24 * time.Hour
)

type cacheEntry struct {
	key string
	res provider.Result
	exp time.Time
}

type responseCache struct {
	mu      sync.Mutex
	ll      *list.List // front = 最近使用；容量淘汰淘汰 back
	entries map[string]*list.Element
	hits    int64
	misses  int64
}

func newResponseCache() *responseCache {
	return &responseCache{ll: list.New(), entries: map[string]*list.Element{}}
}

func (c *responseCache) get(key string) (provider.Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		c.misses++
		return provider.Result{}, false
	}
	entry := el.Value.(*cacheEntry)
	if time.Now().After(entry.exp) {
		c.ll.Remove(el)
		delete(c.entries, key)
		c.misses++
		return provider.Result{}, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return entry.res, true
}

func (c *responseCache) put(key string, res provider.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value = &cacheEntry{key: key, res: res, exp: time.Now().Add(cacheTTL)}
		c.ll.MoveToFront(el)
		return
	}
	c.entries[key] = c.ll.PushFront(&cacheEntry{key: key, res: res, exp: time.Now().Add(cacheTTL)})
	for c.ll.Len() > cacheMaxEntries {
		old := c.ll.Back()
		if old == nil {
			break
		}
		c.ll.Remove(old)
		delete(c.entries, old.Value.(*cacheEntry).key)
	}
}

func (c *responseCache) stats() (hits, misses, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int(c.hits), int(c.misses), c.ll.Len()
}

// cacheEnabled 读持久化设置（默认关）。回放会让「续聊粘性/会话映射」对缓存命中的
// 轮次完全旁路，先由探针数据确认命中率再开启。
func (r *Router) cacheEnabled() bool {
	v, _ := r.Store.GetSetting("cache_enabled")
	on, _ := strconv.ParseBool(v)
	return on
}

// cacheGet 命中返回标记 Cached 的结果副本，未命中/未开启返回 nil。
func (r *Router) cacheGet(req *provider.ChatRequest) *provider.Result {
	if !r.cacheEnabled() || !provider.IsCacheable(req) {
		return nil
	}
	res, ok := r.cache.get(provider.CacheKey(req))
	if !ok {
		return nil
	}
	res.Cached = true
	return &res
}

// cachePut 只存成功且无 tool_calls 的响应（见文件头说明）。
func (r *Router) cachePut(req *provider.ChatRequest, res *provider.Result) {
	if res == nil || !res.Success || len(res.ToolCalls) > 0 || !provider.IsCacheable(req) {
		return
	}
	r.cache.put(provider.CacheKey(req), *res)
}
