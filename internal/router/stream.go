package router

import (
	"context"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// Stream runs a streaming completion through the shared retry/failover driver.
// A tracked emit callback records whether any delta has been flushed; once it
// has, the driver fails hard rather than retrying (retrying would duplicate
// output). While no delta has been sent it retries/fails over exactly like Chat.
func (r *Router) Stream(ctx context.Context, req *provider.ChatRequest, emit provider.EmitFunc) (*provider.Result, *store.Account, error) {
	defer r.probe(req)()
	if res := r.cacheGet(req); res != nil {
		// 缓存回放：单帧正文 + 结束帧。emit 闭包按各协议自行开流/映射
		// （Anthropic 侧忽略 FinishReason，OpenAI 侧落到 finish_reason=stop）。
		if res.ReasoningContent != "" {
			if err := emit(provider.Delta{ReasoningContent: res.ReasoningContent}); err != nil {
				return nil, nil, err
			}
		}
		if err := emit(provider.Delta{Content: res.Content}); err != nil {
			return nil, nil, err
		}
		if err := emit(provider.Delta{HasFinish: true, FinishReason: "stop"}); err != nil {
			return nil, nil, err
		}
		return res, nil, nil
	}
	emitted := false
	trackedEmit := func(d provider.Delta) error {
		emitted = true
		return emit(d)
	}
	res, acc, err := r.runAttempts(ctx, req, attemptPlan{
		stream: true,
		invoke: func(acc *store.Account) *provider.Result {
			return r.Provider.StreamChat(ctx, acc, req, trackedEmit)
		},
		emitted: func() bool { return emitted },
	})
	if err == nil {
		r.cachePut(req, res)
	}
	return res, acc, err
}
