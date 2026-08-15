package provider

import (
	"context"
	"encoding/json"
	"time"

	"ps2api/internal/store"
)

// ProbeTimeout 单账号额度探测的请求超时（远小于正常对话的 300s）。
const ProbeTimeout = 60 * time.Second

// ProbeQuota 向 Postman 发起一次最小探测请求（query="ping"，haiku 模型），
// 从流里取 usage 事件拉取该账号真实额度（limit/usage/overage/userType）。
// 探测会真实消耗极少量额度，可用于新导入账号的首次采集或存量账号的复核；失败时 Result 里
// 会携带 Error/AuthFailed/QuotaExhausted 等状态，由调用方决定如何处理。
// 注意：探测不经过 Router.Chat，不写 request_logs、不改变号池状态。
func (p *Provider) ProbeQuota(ctx context.Context, acc *store.Account) *Result {
	res := &Result{}
	tokens, err := p.GetTokens(acc)
	if err != nil {
		res.Error = err.Error()
		res.AuthFailed = true
		return res
	}
	req := &ChatRequest{
		Model:    "claude-haiku-4-5",
		Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"ping"`)}},
	}
	postmanModel, _ := ResolvePostmanModel(req.Model)
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	// 探测只关心 usage 事件，忽略内容增量；streamInternal 会把
	// Usage/Error/AuthFailed/RateLimited/QuotaExhausted 填进 res。
	_ = p.streamInternal(ctx, acc, req, tokens, postmanModel, func(Delta) error { return nil }, res)
	return res
}
