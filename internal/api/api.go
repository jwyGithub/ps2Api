// Package api 承载 HTTP 服务的装配层：Server 定义、依赖注入（New）、
// 路由表（Register）与基础端点（鉴权、health、models）。
// 各业务 handler 按职责拆分到同包的其余文件：
//   - middleware.go  Trace 中间件
//   - openai.go      OpenAI 协议（/v1/chat/completions）
//   - anthropic.go   Anthropic 协议（/v1/messages）
//   - responses.go   OpenAI Responses 协议（/v1/responses）
//   - accounts.go    账号 CRUD、导入导出、额度刷新
//   - metrics.go     设置、analytics、代理检查
//   - ops.go         运维只读端点与面板静态资源
//   - waf.go         WAF 检测（签名表暴露、对照候选、离线分析）
//   - waf_probe.go  WAF 在线探针二分（后台 job：发起/进度/中止）
//   - helpers.go     公共 helper（jsonWrite/sse/... 与协议专属错误体
//     anthropicError/openAIError/protoError；jsonError 只给面板端点用）
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/router"
	"ps2api/internal/store"
)

type Server struct {
	Store  *store.Store
	Router *router.Router
	Vision *provider.MediaResolver
	// probe 是「WAF 检测」在线探针的 job 管理器（内存态、单飞，见 waf_probe.go）。
	probe probeManager
	// slots 是每 API Key 的进程内并发计数器（见 apikeys.go），traceChat 里 acquire/release。
	slots keySlots
	// svcTokens 是「网关测试」回环调用 /v1 用的一次性内部令牌（见 apikeys.go），
	// 使面板服务测试不依赖任何业务 API Key 存在。
	svcMu     sync.Mutex
	svcTokens map[string]struct{}
}

func New(s *store.Store) *Server {
	srv := &Server{Store: s, Router: router.New(s), Vision: provider.NewMediaResolver(s)}
	// 在线探针发送器：经共享 Provider 直发（出口配置与业务流量一致；绕过 router，
	// 不占重试预算、不触发账号冷却、不写 request_logs）。测试覆写 newSender 注入假实现。
	srv.probe.newSender = func(acc *store.Account, model string) probeSender {
		return func(ctx context.Context, text string) probeOutcome {
			content, _ := json.Marshal(text)
			// 不 ResetConversation：探针消息带唯一 nonce，指纹必然未命中 → 冷启动
			// USER_QUERY；Reset 会清掉该账号全部业务会话映射，干扰线上续聊。
			req := &provider.ChatRequest{
				Model:    model,
				Messages: []provider.ChatMessage{{Role: "user", Content: content}},
				WafProbe: true, // 原样出站：不中和（掐灭待验证特征）、不截断（破坏等长 padding）
			}
			cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			return classifyProbeOutcome(srv.Router.Provider.Chat(cctx, acc, req))
		}
	}
	return srv
}

func (s *Server) Register(mux *http.ServeMux) {
	// 健康检查
	mux.HandleFunc("GET /health", s.health)

	// 对话类端点（/v1/*）：对外暴露的模型推理协议（见 openai.go / responses.go / anthropic.go）
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.traceChat(s.openAI))
	mux.HandleFunc("POST /v1/responses", s.traceChat(s.responses))
	mux.HandleFunc("POST /v1/messages", s.traceChat(s.anthropic))

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
	mux.HandleFunc("POST /api/refresh-quota-exhausted", s.refreshExhaustedQuota)

	// 管理类端点（/api/*）——设置、analytics、代理检查（见 metrics.go）
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("POST /api/proxy-check", s.proxyCheck)
	mux.HandleFunc("POST /api/proxy-test", s.proxyTest)
	mux.HandleFunc("GET /api/analytics", s.analytics)

	// 管理类端点（/api/*）——API KEY 管理（见 apikeys.go）
	mux.HandleFunc("GET /api/keys", s.listKeys)
	mux.HandleFunc("POST /api/keys", s.createKey)
	mux.HandleFunc("PATCH /api/keys/{id}", s.updateKey)
	mux.HandleFunc("DELETE /api/keys/{id}", s.deleteKey)

	// 管理类端点（/api/*）——运维只读与缓存探针（见 ops.go）
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/request-logs", s.requestLogs)
	mux.HandleFunc("GET /api/cache-probe", s.cacheProbe)
	mux.HandleFunc("DELETE /api/cache-probe", s.cacheProbeReset)
	mux.HandleFunc("POST /api/sql-query", s.sqlQuery)

	// 管理类端点（/api/*）——WAF 检测（见 waf.go）
	mux.HandleFunc("GET /api/waf-signatures", s.wafSignatures)
	mux.HandleFunc("GET /api/waf/baselines", s.wafBaselines)
	mux.HandleFunc("GET /api/waf/analyze", s.wafAnalyze)
	mux.HandleFunc("POST /api/waf/probe", s.wafProbeStart)
	mux.HandleFunc("GET /api/waf/probe/{job_id}", s.wafProbeStatus)
	mux.HandleFunc("DELETE /api/waf/probe/{job_id}", s.wafProbeAbort)

	// 面板登录（见 login.go）：ADMIN_PASSWORD 设置后生效
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)

	// 面板静态资源（见 ops.go）
	mux.HandleFunc("GET /dashboard.js", s.dashboardAsset)
	mux.HandleFunc("GET /dashboard/", s.dashboardStatic)
	mux.HandleFunc("GET /", s.dashboard)
}

// auth 统一鉴权入口，按端点家族分流：
//   - /api/accounts*：面板页 + 外部服务 REST 集成（openapi.yaml）共用，双凭据：
//     /login 会话 Cookie 或任一有效 API Key（Bearer/x-api-key）皆可。openapi.yaml
//     对外承诺的就是 Bearer/x-api-key，不能收窄成纯会话。
//   - 其余 /api/* 面板端点：只认 /login 签发的会话 Cookie，API Key 不是面板凭据。
//     未设 ADMIN_PASSWORD 时无登录可言，维持开放引导态（兼容既有无密码部署）。
//   - /v1/* 对外模型协议：只认 Bearer/x-api-key（resolveKey），不认浏览器会话。
//     api_keys 表为空时全开放（首次创建密钥前的引导态）。
func (s *Server) auth(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		// accounts 系列对外（REST 集成）开放 API Key：先按 Key 验，验不过再走会话。
		accountsAPI := strings.HasPrefix(r.URL.Path, "/api/accounts")
		if accountsAPI {
			if _, err := s.resolveKey(r); err == nil {
				return true
			}
		}
		if !loginEnabled() || validSession(r) {
			return true
		}
		if accountsAPI {
			// 集成方没带有效 Key、也没会话：按 openapi.yaml 的错误契约回 401 invalid_api_key。
			jsonError(w, 401, "Invalid API key", "invalid_api_key")
		} else {
			jsonError(w, 401, "未登录或会话已过期", "authentication_error")
		}
		return false
	}
	_, err := s.resolveKey(r)
	if err == nil {
		return true
	}
	// 鉴权失败的错误体按调用方协议分流：Anthropic 客户端期望 authentication_error，
	// OpenAI 客户端期望 invalid_request_error + code。面板端点(/api/*)统一走 OpenAI 形状，
	// 面板前端只读 error.message。
	var ke *keyAuthError
	if errors.As(err, &ke) {
		protoError(w, r, ke.status, ke.msg, ke.typ, "invalid_request_error", ke.code)
		return false
	}
	jsonError(w, 500, err.Error(), "internal_error")
	return false
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
