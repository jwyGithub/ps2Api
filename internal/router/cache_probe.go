package router

import (
	"sync"

	"ps2api/internal/provider"
)

// shadowProbe 影子缓存探针：只度量、不改变任何返回值。
// 持久信号（重复命中率）落在 store.cache_probe；并发撞车数（single-flight 潜在收益）
// 是运行时量，内存计数即可——重启归零、多实例各计各的（会低估，见 README）。
type shadowProbe struct {
	mu         sync.Mutex
	inflight   map[string]int
	collisions int64
}

// probe 在请求入口记录一次探针，返回出口回调（用于并发在途计数递减）。
// 非可缓存请求或探针关闭时为空操作。
func (r *Router) probe(req *provider.ChatRequest) func() {
	if !r.cacheProbeEnabled() || !provider.IsCacheable(req) {
		return func() {}
	}
	key := provider.CacheKey(req)
	_ = r.Store.RecordCacheProbe(key)
	r.shadow.mu.Lock()
	r.shadow.inflight[key]++
	if r.shadow.inflight[key] > 1 {
		r.shadow.collisions++ // 同一指纹并发在途 = single-flight 本可省一次上游调用
	}
	r.shadow.mu.Unlock()
	return func() {
		r.shadow.mu.Lock()
		if r.shadow.inflight[key]--; r.shadow.inflight[key] <= 0 {
			delete(r.shadow.inflight, key)
		}
		r.shadow.mu.Unlock()
	}
}

// CacheProbeStats 汇总探针数据供只读端点展示。
func (r *Router) CacheProbeStats() map[string]interface{} {
	distinct, repeats, _ := r.Store.CacheProbeStats()
	total := distinct + repeats
	var rate float64
	if total > 0 {
		rate = float64(repeats) / float64(total)
	}
	r.shadow.mu.Lock()
	collisions := r.shadow.collisions
	r.shadow.mu.Unlock()
	return map[string]interface{}{
		"enabled":           r.cacheProbeEnabled(),
		"cacheableRequests": total,
		"distinctRequests":  distinct,
		"potentialHits":     repeats,
		"potentialHitRate":  rate,
		"singleflightSaved": collisions,
	}
}
