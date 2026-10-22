package router

import (
	"context"
	"log"
	"time"

	"ps2api/internal/provider"
)

// model_refresh.go —— /chat 端模型清单（GET gateway.postman.com/config?platform=...，
// 见 provider/modelconfig.go）的定时刷新。客户端不升级程序也能看到新模型，就是靠
// 这份清单实时下发；网关同理：启动时拉一次对齐上游，之后每 6 小时兜底同步（面板
// POST /api/models/refresh 可随时手动触发）。失败静默——静态 PostmanModelMap 兜底，
// 不影响任何既有路由。
func (r *Router) StartModelRefresh(ctx context.Context) {
	refresh := func(why string) {
		acc, err := r.Pool.Next(nil)
		if err != nil {
			log.Printf("[model-refresh] %s 跳过: 无可用账号 (%v)", why, err)
			return
		}
		if _, err := r.Provider.FetchAgentModeModels(ctx, acc); err != nil {
			log.Printf("[model-refresh] %s 拉取失败（静默，静态映射兜底）: %v", why, err)
			return
		}
		keys := provider.DynamicModelKeys()
		log.Printf("[model-refresh] %s 成功，/chat 可用模型 %d 个", why, len(keys))
	}
	refresh("启动")
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh("定时")
		}
	}
}
