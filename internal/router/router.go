package router

import (
	"ps2api/internal/pool"
	"ps2api/internal/provider"
	"ps2api/internal/store"
)

type Router struct {
	Pool     *pool.Pool
	Provider *provider.Provider
	Store    *store.Store
	shadow   shadowProbe
}

func New(s *store.Store) *Router {
	r := &Router{Pool: pool.New(s), Provider: provider.New(), Store: s, shadow: shadowProbe{inflight: map[string]int{}}}
	// 出口代理池：仅当 proxy_enabled=true 且配置了 proxy_urls 时启用，否则返回 nil → 走本机直连。
	// 每次请求实时读设置，面板改动即时生效、无需重启。
	r.Provider.SetProxyList(func() []string {
		if on, _ := r.Store.GetSetting("proxy_enabled"); on != "true" {
			return nil
		}
		v, _ := r.Store.GetSetting("proxy_urls")
		if v == "" {
			return nil
		}
		return []string{v}
	})
	// 代理全挂兜底直连：仅当显式开启时，代理出口不可达才回退本机直连重试；默认关闭（严格走代理）。
	r.Provider.SetProxyFallbackDirect(func() bool {
		on, _ := r.Store.GetSetting("proxy_fallback_direct")
		return on == "true"
	})
	return r
}

type RouteError struct {
	Message string
	// GatewayBlocked 标记该失败源于上游网关(Cloudflare)风控拦截(403)且所有尝试账号均被拦。
	// 供 HTTP 层选择合适的状态码/文案,让 agent 终端能明确「上游拦截、非本地错误」。
	GatewayBlocked bool
}

func (e *RouteError) Error() string { return e.Message }
