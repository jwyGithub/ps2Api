package provider

import (
	"context"
	"os"

	"ps2api/internal/store"
)

// contextSeedEnabled 报告冷启动补种开关。默认开启；GATEWAY_CONTEXT_SEED=0 关闭。
func contextSeedEnabled() bool { return os.Getenv("GATEWAY_CONTEXT_SEED") != "0" }

// seedHistoryThresholdMessages 是触发补种的最小历史消息数（含最新消息），
// 严格大于才触发：len > 6 即 7 条起补种。
// 更短的会话折叠产物不超 10000 rune，补种白花一次上游配额。
const seedHistoryThresholdMessages = 7

// shouldSeed 报告本次请求是否应做上下文补种。条件全满足：
// 冷启动（无会话命中）、历史足够长、开关开启。tool-tail 重放也补种（2026-09-18 改）：
// 会话失效后的重放轮若不补种，128 条消息裸折叠进 10000 rune，中段省略会吞掉
// 已批准方案等关键上下文（2026-09-18 线上事故，见设计文档）。
// WafProbe 探针绝不补种（必须逐字复现可疑内容）。
func (p *Provider) shouldSeed(accID int64, req *ChatRequest) bool {
	// 补种轮自身（ContextSeed=true，由 seedConversation 递归调 streamInternal）绝不
	// 再补种——否则同一批消息会无限递归。
	if req.ContextSeed {
		return false
	}
	if !contextSeedEnabled() || req.WafProbe {
		return false
	}
	// 纯 user 历史（无 assistant/tool）在 LookupConversation 的 hasReusableHistory
	// 前置下第二轮映射永不命中，补种纯浪费一次上游配额，直接否决。
	if !hasReusableHistory(req.Messages) {
		return false
	}
	if len(req.Messages) < seedHistoryThresholdMessages {
		return false
	}
	return p.LookupConversation(accID, req.Messages) == ""
}

// seedConversation 发起补种轮：复用折叠路径把全部历史发往上游（conversationId=null），
// 模型按摘要指令复述任务状态后在服务端建立会话。成功后【仅对普通续聊】按「去掉最新 user
// 消息」的前缀指纹存映射——第二轮请求的 LookupConversation 前缀循环恰好命中它，恢复增量
// 模式。tool-tail 补种绝不存指纹（见下方 v0.0.76 回归说明）：补种只用于让模型在服务端
// 拿到上下文，重放仍走折叠 USER_QUERY，绝不翻转增量。
func (p *Provider) seedConversation(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, postmanModel string) *Result {
	seedReq := *req
	seedReq.ContextSeed = true
	seedRes := &Result{}
	_ = p.streamInternal(ctx, acc, &seedReq, tokens, postmanModel, func(Delta) error { return nil }, seedRes)
	if !seedRes.Success || seedRes.ConversationID == "" {
		return seedRes
	}
	// tool-tail 补种绝不存前缀指纹（2026-09-18 v0.0.76 回归修复，方案 1）。
	// 补种轮把全历史作为纯文本 USER_QUERY 发往上游、在服务端建立的是「文本会话」，
	// 服务端并无挂起的 tool_use。若为 tool-tail 存了前缀指纹，则本轮/重放的
	// LookupConversation 会命中它；而 thirdParty/proxy-tools 的待处理调用是「已知无组」
	// （tailToolCallsUntracked），buildBody 不会把 convID 清空，于是对这个文本会话走
	// 增量 TOOL_RESPONSE —— 上游找不到对应 tool_use，报 "No tool call found" /
	// "Upstream returned an empty completion"（见事故 4023-4027）。不存指纹即让
	// tool-tail 重放继续走折叠 USER_QUERY（把待处理 tool_result 折进 query 文本）。
	if toolTailIndex(req.Messages) >= 0 {
		return seedRes
	}
	// 普通续聊：按「去掉最新 user 消息」的前缀指纹存映射——第二轮完整 messages 的
	// LookupConversation 前缀循环在 i==queryIdx 处命中此键，恢复增量模式。
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
