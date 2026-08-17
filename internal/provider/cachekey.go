package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// CacheKey 为一次请求算出稳定指纹，用于「影子缓存探针」度量重复率。
// 剥掉尾部由客户端追加的 <total_tokens> volatile 系统消息（复用
// isTrailingTokenMetadata），否则同一请求每轮 key 都变、命中率恒为 0 失真。
// 不含 Stream 标志：同一内容无论流式与否都算同一次调用。
func CacheKey(req *ChatRequest) string {
	msgs := req.Messages
	for len(msgs) > 0 && isTrailingTokenMetadata(msgs[len(msgs)-1]) {
		msgs = msgs[:len(msgs)-1]
	}
	h := sha256.New()
	// json.Encoder 对 map 键有序输出，指纹稳定；只编码影响上游响应的字段。
	_ = json.NewEncoder(h).Encode([]interface{}{
		req.Endpoint, req.Model, msgs, req.Tools, req.ToolChoice, req.ParallelToolCalls,
	})
	return hex.EncodeToString(h.Sum(nil))
}

// IsCacheable 只认「单发、无状态」请求：带工具结果回传（tool-tail）的轮次是有状态、
// 非幂等的，绝不能命中缓存或被去重，直接排除。多轮续聊的历史每轮都在变，key 天然
// 唯一、不会误命中，无需单独排除。
func IsCacheable(req *ChatRequest) bool {
	return !toolTail(req.Messages)
}
