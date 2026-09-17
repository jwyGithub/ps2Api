package provider

import (
	"context"
	"os"

	"ps2api/internal/store"
)

// contextSeedEnabled 报告冷启动补种开关。默认开启；GATEWAY_CONTEXT_SEED=0 关闭。
func contextSeedEnabled() bool { return os.Getenv("GATEWAY_CONTEXT_SEED") != "0" }

// seedHistoryMinMessages 是触发补种的最小历史消息数（含最新消息）。
// 更短的会话折叠产物不超 10000 rune，补种白花一次上游配额。
const seedHistoryMinMessages = 7

// shouldSeed 报告本次请求是否应做上下文补种。四个条件全满足：
// 冷启动（无会话命中）、非 tool-tail 重放、历史足够长、开关开启。
// WafProbe 探针绝不补种（必须逐字复现可疑内容）。
func (p *Provider) shouldSeed(accID int64, req *ChatRequest) bool {
	if !contextSeedEnabled() || req.WafProbe {
		return false
	}
	if toolTail(req.Messages) {
		return false
	}
	if len(req.Messages) <= seedHistoryMinMessages {
		return false
	}
	return p.LookupConversation(accID, req.Messages) == ""
}

// seedConversation 发起补种轮：复用折叠路径把全部历史发往上游（conversationId=null），
// 模型按摘要指令复述任务状态后在服务端建立会话。成功后按「去掉最新消息的前缀指纹」
// 存映射——第二轮请求的 LookupConversation 前缀循环恰好命中它，恢复增量模式。
func (p *Provider) seedConversation(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, postmanModel string) *Result {
	seedReq := *req
	seedReq.ContextSeed = true
	seedRes := &Result{}
	_ = p.streamInternal(ctx, acc, &seedReq, tokens, postmanModel, func(Delta) error { return nil }, seedRes)
	if !seedRes.Success || seedRes.ConversationID == "" {
		return seedRes
	}
	// 前缀指纹：messages[:queryIdx]（去掉最新 user 消息）。第二轮完整 messages
	// 的 LookupConversation 循环在 i==queryIdx 时命中此键。
	queryIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && !isAnthropicToolResult(req.Messages[i]) {
			queryIdx = i
			break
		}
	}
	if queryIdx < 0 {
		return seedRes
	}
	fp := conversationFingerprint(req.Messages[:queryIdx])
	p.convStore.PutConversation(acc.ID, fp, seedRes.ConversationID)
	p.convStore.PutOwner(fp, acc.ID)
	return seedRes
}
