// apikeys.go —— 面板「API KEY 管理」：对外 /v1 端点多密钥的增删改查、
// 每密钥并发限制（进程内计数）与 AI credits 用量计费（额度 = credits × 倍率）。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// keyCtxKey 把本次请求命中的 APIKey 放进请求 ctx，供 handler 在响应完成后计费回写。
type keyCtxKey struct{}

// keySlots 是每密钥的进程内并发计数器：acquire 时超限即拒绝（429）。
// 内存态、重启归零（与号池冷却一致的口径）。ponytail: 全局一把锁，量级是单机网关，够用。
type keySlots struct {
	mu sync.Mutex
	m  map[int64]int64
}

func (s *keySlots) acquire(id int64, limit int) bool {
	if limit <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[int64]int64{}
	}
	if s.m[id] >= int64(limit) {
		return false
	}
	s.m[id]++
	return true
}

func (s *keySlots) release(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[id] > 0 {
		s.m[id]--
	}
}

// presentedKey 从 Authorization: Bearer 或 x-api-key 头取出客户端出示的密钥。
func presentedKey(r *http.Request) string {
	if v := r.Header.Get("Authorization"); len(v) > 7 && v[:7] == "Bearer " {
		return v[7:]
	}
	return r.Header.Get("x-api-key")
}

// keyAuthError 携带鉴权失败时该回给客户端的状态码与协议错误信息。
type keyAuthError struct {
	status int
	msg    string
	typ    string
	code   string
}

func (e *keyAuthError) Error() string { return e.msg }

// resolveKey 静默解析并校验请求出示的密钥（不写响应）：
//   - (nil, nil)  引导态：一个密钥都没建过 → 不鉴权（保持「未设置 Key 时开放」的旧语义）；
//   - (nil, err)  校验失败（缺失/未知/停用/过期/超额），err 为 *keyAuthError；
//   - (k, nil)    通过，返回命中的密钥。
func (s *Server) resolveKey(r *http.Request) (*store.APIKey, error) {
	keys, err := s.Store.ListAPIKeys()
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	pk := presentedKey(r)
	if pk == "" {
		return nil, &keyAuthError{401, "Invalid API key", "authentication_error", "invalid_api_key"}
	}
	// 「网关测试」回环令牌：面板服务测试带它调自己的 /v1，等价引导态放行
	// （不挂任何密钥语义：不占并发槽、不计额度）。
	if s.isSvcToken(pk) {
		return nil, nil
	}
	var k *store.APIKey
	for _, cand := range keys {
		if cand.Key == pk {
			k = cand
			break
		}
	}
	if k == nil {
		return nil, &keyAuthError{401, "Invalid API key", "authentication_error", "invalid_api_key"}
	}
	if !k.Enabled {
		return nil, &keyAuthError{401, "API key disabled", "authentication_error", "api_key_disabled"}
	}
	if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()) {
		return nil, &keyAuthError{401, "API key expired", "authentication_error", "api_key_expired"}
	}
	if k.QuotaLimit > 0 && k.QuotaUsed >= k.QuotaLimit {
		return nil, &keyAuthError{429, "API key quota exceeded", "rate_limit_error", "insufficient_quota"}
	}
	return k, nil
}

// chargeKey 在响应完成后把 AI credits 消耗（×倍率）回写到该密钥。
// ctx 无密钥（引导态/未过鉴权路径）时是 no-op。并发请求可能小幅超出限额，
// 与入口检查叠加足够（one-api 同口径）。ponytail: 不做精确分布式计量。
func (s *Server) chargeKey(ctx context.Context, res *provider.Result) {
	k, _ := ctx.Value(keyCtxKey{}).(*store.APIKey)
	if k == nil || res == nil {
		return
	}
	if res.Credits <= 0 {
		return
	}
	cost := int64(math.Ceil(res.Credits * k.Multiplier))
	if cost <= 0 {
		return
	}
	_ = s.Store.AddAPIKeyUsage(k.ID, cost)
}

// ─── 「网关测试」回环令牌 ─────────────────────────────────────────
// 面板账号测试的 service 模式回环调用本服务 /v1（走完整网关链路），但面板鉴权已与
// API Key 解耦：测试签发一个进程内一次性随机令牌，resolveKey 认它放行，测试结束即注销。
// 仅存内存、重启即失效、128 位随机不可猜测；不写库、不占并发槽、不计额度。
func (s *Server) svcTokenIssue() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	tok := "sk-svc-" + hex.EncodeToString(raw)
	s.svcMu.Lock()
	if s.svcTokens == nil {
		s.svcTokens = map[string]struct{}{}
	}
	s.svcTokens[tok] = struct{}{}
	s.svcMu.Unlock()
	return tok
}

func (s *Server) svcTokenRevoke(tok string) {
	s.svcMu.Lock()
	delete(s.svcTokens, tok)
	s.svcMu.Unlock()
}

func (s *Server) isSvcToken(tok string) bool {
	s.svcMu.Lock()
	_, ok := s.svcTokens[tok]
	s.svcMu.Unlock()
	return ok
}

// ─── 管理端点（面板 /api/*，登录会话鉴权） ─────────

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	keys, err := s.Store.ListAPIKeys()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"data": keys})
}

// keyForm 是新增/编辑共用的请求体。ExpiresInDays：空/0=永久；正数=从现在起 N 天（编辑即重设）。
type keyForm struct {
	Name             string   `json:"name"`
	ExpiresInDays    *int     `json:"expiresInDays"`
	QuotaLimit       int64    `json:"quotaLimit"`
	ConcurrencyLimit int64    `json:"concurrencyLimit"`
	Multiplier       *float64 `json:"multiplier"`
	Enabled          *bool    `json:"enabled"`
}

func (f *keyForm) expiresAt() *time.Time {
	if f.ExpiresInDays == nil || *f.ExpiresInDays <= 0 {
		return nil
	}
	t := time.Now().Add(time.Duration(*f.ExpiresInDays) * 24 * time.Hour)
	return &t
}

func (f *keyForm) multiplier() float64 {
	if f.Multiplier == nil || *f.Multiplier <= 0 {
		return 1
	}
	return *f.Multiplier
}

// normalize 修剪负数字段：额度/并发 <0 归 0（= 不限）。
func (f *keyForm) normalize() {
	if f.QuotaLimit < 0 {
		f.QuotaLimit = 0
	}
	if f.ConcurrencyLimit < 0 {
		f.ConcurrencyLimit = 0
	}
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var f keyForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		jsonError(w, 400, "请求体需为 JSON 对象", "invalid_request")
		return
	}
	f.normalize()
	// 密钥值由服务端生成：sk- + 24 字节随机数的 hex（48 字符）。
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	k, err := s.Store.CreateAPIKey("sk-"+hex.EncodeToString(raw), f.Name, f.expiresAt(), f.QuotaLimit, f.ConcurrencyLimit, f.multiplier())
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"success": true, "key": k})
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var f keyForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		jsonError(w, 400, "请求体需为 JSON 对象", "invalid_request")
		return
	}
	f.normalize()
	enabled := true
	if f.Enabled != nil {
		enabled = *f.Enabled
	}
	if err := s.Store.UpdateAPIKey(id, f.Name, f.expiresAt(), f.QuotaLimit, f.ConcurrencyLimit, f.multiplier(), enabled); err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	k, _ := s.Store.GetAPIKey(id)
	if k == nil {
		jsonError(w, 404, "API key 不存在", "not_found")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"success": true, "key": k})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Store.DeleteAPIKey(id); err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]bool{"success": true})
}
