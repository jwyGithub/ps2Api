package router

import (
	"context"

	"ps2api/internal/provider"
)

// TestAccount 对指定 ID 的账号发起一次连通性测试（直连 / 网关），返回完整现场结果。
// 供号池页每行「测试」按钮调用。账号不存在时返回错误。
//
// 说明：测试不经过 Router.Chat 的号池选号与重试，固定测试指定账号本身；
// 不写 request_logs、不改号池状态与额度快照——纯诊断用途。
func (r *Router) TestAccount(ctx context.Context, id int64, mode provider.AccountTestMode, model, prompt string) (*provider.AccountTestResult, error) {
	acc, err := r.Store.GetAccount(id)
	if err != nil {
		return nil, err
	}
	return r.Provider.TestAccount(ctx, acc, mode, model, prompt), nil
}

// StreamTestAccount 与 TestAccount 一致，但支持流式回调：onMeta 在请求发出前触发一次，
// onLine 在读取上游响应时逐行触发，供上层（HTTP handler）实时把现场推给前端。
func (r *Router) StreamTestAccount(ctx context.Context, id int64, mode provider.AccountTestMode, model, prompt string, onMeta func(*provider.AccountTestResult), onLine func(string)) (*provider.AccountTestResult, error) {
	acc, err := r.Store.GetAccount(id)
	if err != nil {
		return nil, err
	}
	return r.Provider.StreamTestAccount(ctx, acc, mode, model, prompt, onMeta, onLine), nil
}
