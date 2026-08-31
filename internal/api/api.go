// Package api 承载 HTTP 服务的装配层：Server 定义、依赖注入（New）、
// 路由表（Register）与基础端点（鉴权、health、models）。
// 各业务 handler 按职责拆分到同包的其余文件：
//   - middleware.go  Trace 中间件
//   - openai.go      OpenAI 协议（/v1/chat/completions）
//   - anthropic.go   Anthropic 协议（/v1/messages）
//   - responses.go   OpenAI Responses 协议（/v1/responses）
//   - accounts.go    账号 CRUD、导入导出、额度刷新
//   - metrics.go     告警、设置、analytics、代理检查
//   - ops.go         运维只读端点与面板静态资源
//   - helpers.go     公共 helper（jsonWrite/sse/... 与协议专属错误体
//     anthropicError/openAIError/protoError；jsonError 只给面板端点用）
package api

import (
	"net/http"
	"strings"

	"ps2api/internal/provider"
	"ps2api/internal/router"
	"ps2api/internal/store"
)

type Server struct {
	Store  *store.Store
	Router *router.Router
	Vision *provider.MediaResolver
}

func New(s *store.Store) *Server {
	srv := &Server{Store: s, Router: router.New(s), Vision: provider.NewMediaResolver(s)}
	// 后台告警评估器：基于真实日志/额度统计定期落告警（见 metrics.go）
	go srv.runAlertEvaluator()
	return srv
}

func (s *Server) Register(mux *http.ServeMux) {
	// 健康检查
	mux.HandleFunc("GET /health", s.health)

	// 对话类端点（/v1/*）：对外暴露的模型推理协议（见 openai.go / responses.go / anthropic.go）
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", traceChat(s.openAI))
	mux.HandleFunc("POST /v1/responses", traceChat(s.responses))
	mux.HandleFunc("POST /v1/messages", traceChat(s.anthropic))

	// 管理类端点（/api/*）——账号管理（见 accounts.go）
	mux.HandleFunc("GET /api/accounts", s.accounts)
	mux.HandleFunc("POST /api/accounts", s.addAccount)
	mux.HandleFunc("GET /api/accounts/export", s.exportAccounts)
	mux.HandleFunc("POST /api/accounts/import", s.importAccounts)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("PATCH /api/accounts/{id}", s.toggleAccount)
	mux.HandleFunc("POST /api/accounts/{id}/refresh-quota", s.refreshAccountQuota)
	mux.HandleFunc("POST /api/accounts/{id}/test", s.testAccount)
	mux.HandleFunc("POST /api/refresh-quota", s.refreshQuota)

	// 管理类端点（/api/*）——设置、告警、analytics、代理检查（见 metrics.go）
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("POST /api/proxy-check", s.proxyCheck)
	mux.HandleFunc("POST /api/proxy-test", s.proxyTest)
	mux.HandleFunc("GET /api/analytics", s.analytics)
	mux.HandleFunc("GET /api/alerts", s.alerts)
	mux.HandleFunc("POST /api/alerts/{id}/resolve", s.resolveAlert)
	mux.HandleFunc("POST /api/alerts/resolve-all", s.resolveAllAlerts)

	// 管理类端点（/api/*）——运维只读与缓存探针（见 ops.go）
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/request-logs", s.requestLogs)
	mux.HandleFunc("GET /api/cache-probe", s.cacheProbe)
	mux.HandleFunc("DELETE /api/cache-probe", s.cacheProbeReset)

	// 面板静态资源（见 ops.go）
	mux.HandleFunc("GET /dashboard.js", s.dashboardAsset)
	mux.HandleFunc("GET /dashboard/", s.dashboardStatic)
	mux.HandleFunc("GET /", s.dashboard)
}

func (s *Server) auth(w http.ResponseWriter, r *http.Request) bool {
	key := s.apiKey()
	// 未设置 API Key 时不鉴权（首次进面板设置前的引导态）。
	if key == "" {
		return true
	}
	// 同时接受两种鉴权头：OpenAI 风格 Authorization: Bearer <key>，
	// 与 Anthropic 风格 x-api-key: <key>（Claude 客户端按此约定发送）。
	if r.Header.Get("Authorization") == "Bearer "+key || r.Header.Get("x-api-key") == key {
		return true
	}
	// 鉴权失败的错误体也要按调用方协议走：Anthropic 客户端期望 authentication_error，
	// OpenAI 客户端期望 invalid_request_error + code:invalid_api_key。面板端点(/api/*)
	// 不经过这里的协议分流也无妨——protoError 对非 /v1/messages 路径统一走 OpenAI 形状，
	// 面板前端只读 error.message。
	protoError(w, r, 401, "Invalid API key", "authentication_error", "invalid_request_error", "invalid_api_key")
	return false
}

// apiKey 从 SQLite settings 读取当前生效的客户端 Bearer Key（面板可动态修改，改后立即生效）。
func (s *Server) apiKey() string {
	v, _ := s.Store.GetSetting("api_key")
	return strings.TrimSpace(v)
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, 200, map[string]interface{}{"status": "ok"})
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"object": "list", "data": provider.PostmanModels})
}
