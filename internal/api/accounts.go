// accounts.go —— 账号管理端点（/api/accounts*）：列表、新增、导入/导出、
// 启用切换、删除，以及全量与单账号的额度刷新探测。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ps2api/internal/provider"
)

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	all, err := s.Store.ListAccounts()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	for _, a := range all {
		a.Tokens = ""
		a.Password = ""
	}
	jsonWrite(w, 200, map[string]interface{}{"data": all})
}

type addAccountReq struct {
	Email  string          `json:"email"`
	Tokens provider.Tokens `json:"tokens"`
}

type accountFile struct {
	Version    int                  `json:"version"`
	ExportedAt string               `json:"exported_at"`
	Accounts   []accountFileAccount `json:"accounts"`
}

type accountFileAccount struct {
	Email    string          `json:"email"`
	Password string          `json:"password"`
	Source   string          `json:"source"`
	Enabled  *bool           `json:"enabled"`
	Tokens   provider.Tokens `json:"tokens"`
}

func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var q addAccountReq
	if json.NewDecoder(r.Body).Decode(&q) != nil || q.Email == "" {
		jsonError(w, 400, "email and tokens required", "invalid_request")
		return
	}
	b, _ := json.Marshal(q.Tokens)
	a, err := s.Store.UpsertAccount(q.Email, "manual", string(b), "manual")
	if err != nil {
		jsonError(w, 400, err.Error(), "invalid_request")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"success": true, "account": a})
}

func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	all, err := s.Store.ListAccounts()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	out := accountFile{Version: 1, ExportedAt: time.Now().Format(time.RFC3339), Accounts: make([]accountFileAccount, 0, len(all))}
	for _, account := range all {
		var tokens provider.Tokens
		if err := json.Unmarshal([]byte(account.Tokens), &tokens); err != nil {
			jsonError(w, 500, "账号 "+account.Email+" 的登录凭据无效", "internal_error")
			return
		}
		enabled := account.Enabled
		out.Accounts = append(out.Accounts, accountFileAccount{
			Email: account.Email, Password: account.Password, Source: account.Source,
			Enabled: &enabled, Tokens: tokens,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="account.json"`)
	w.Header().Set("Cache-Control", "no-store")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(out)
}

func (s *Server) importAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	var input accountFile
	if err := decoder.Decode(&input); err != nil {
		jsonError(w, 400, "无效的 account.json: "+err.Error(), "invalid_request")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		jsonError(w, 400, "account.json 只能包含一个 JSON 对象", "invalid_request")
		return
	}
	if input.Version != 1 || len(input.Accounts) == 0 {
		jsonError(w, 400, "account.json 版本必须为 1 且至少包含一个账号", "invalid_request")
		return
	}
	seen := make(map[string]bool, len(input.Accounts))
	for i := range input.Accounts {
		account := &input.Accounts[i]
		account.Email = strings.TrimSpace(account.Email)
		if account.Email == "" || account.Enabled == nil {
			jsonError(w, 400, fmt.Sprintf("第 %d 个账号缺少 email 或 enabled", i+1), "invalid_request")
			return
		}
		if seen[account.Email] {
			jsonError(w, 400, "account.json 包含重复账号: "+account.Email, "invalid_request")
			return
		}
		seen[account.Email] = true
		if account.Tokens.UserID == "" || account.Tokens.WorkspaceID == "" || (account.Tokens.AccessToken == "" && account.Tokens.PostmanSID == "") {
			jsonError(w, 400, "账号 "+account.Email+" 的 tokens 不完整", "invalid_request")
			return
		}
		if account.Tokens.AccessToken == "" && account.Tokens.WorkspaceSubdomain == "" {
			jsonError(w, 400, "账号 "+account.Email+" 缺少 workspace_subdomain", "invalid_request")
			return
		}
	}
	importedIDs := make([]int64, 0, len(input.Accounts))
	for _, account := range input.Accounts {
		tokens, _ := json.Marshal(account.Tokens)
		id, err := s.Store.ImportAccount(account.Email, account.Password, string(tokens), account.Source, *account.Enabled)
		if err != nil {
			jsonError(w, 500, err.Error(), "internal_error")
			return
		}
		// 仅启用账号需要刷新额度；禁用 / exhausted 账号会被探测逻辑跳过。
		if *account.Enabled {
			importedIDs = append(importedIDs, id)
		}
	}
	// 导入后自动刷新额度：改为后台异步探测本次导入的启用账号，导入请求立即返回不被探测阻塞。
	// 探测结果经 persistQuota 写库，账号列表接口（/api/accounts）已带 quotaLimit/quotaRemaining，
	// 前端导入后轮询列表即可看到额度陆续到位，无需等待或新增任务状态接口。
	// 关键：必须用 context.Background()——不能用 r.Context()，后者在本响应写完即被取消，
	// 会导致后台探测中途夭折。
	if len(importedIDs) > 0 {
		go s.Router.ProbeAccountsByIDs(context.Background(), importedIDs)
	}
	jsonWrite(w, 200, map[string]interface{}{
		"success":    true,
		"imported":   len(input.Accounts),
		"refreshing": len(importedIDs),
	})
}

// refreshQuota 对所有启用账号发起轻量探测调用并写库，
// 供额度管理页「刷新额度」按钮调用；已采集过的账号也会重新核实。
func (s *Server) refreshQuota(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	results := s.Router.ProbeQuotas(r.Context())
	ok, failed := 0, 0
	for _, pr := range results {
		if pr.OK {
			ok++
		} else {
			failed++
		}
	}
	jsonWrite(w, 200, map[string]interface{}{"ok": ok, "failed": failed, "results": results})
}

// refreshAccountQuota 对号池页某一行账号发起单账号额度探测并写库，
// 供每行「刷新额度」按钮调用；返回该账号最新的 limit/remaining。
func (s *Server) refreshAccountQuota(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	pr, err := s.Router.ProbeAccountQuota(r.Context(), id)
	if err != nil {
		jsonError(w, 404, err.Error(), "not_found")
		return
	}
	ok, failed := 0, 0
	if pr.OK {
		ok = 1
	} else {
		failed = 1
	}
	jsonWrite(w, 200, map[string]interface{}{"ok": ok, "failed": failed, "result": pr})
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Store.DeleteAccount(id); err != nil {
		jsonError(w, 404, err.Error(), "not_found")
		return
	}
	jsonWrite(w, 200, map[string]bool{"success": true})
}
func (s *Server) toggleAccount(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var q struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&q)
	if err := s.Store.SetAccountEnabled(id, q.Enabled); err != nil {
		jsonError(w, 404, err.Error(), "not_found")
		return
	}
	// 重新启用账号时，其历史未处理告警自动解决（真实告警生命周期）
	if q.Enabled {
		_ = s.Store.ResolveAlertsBySource("account", "account_error", id)
		_ = s.Store.ResolveAlertsBySource("account", "quota_exhausted", id)
		_ = s.Store.ResolveAlertsBySource("account", "low_quota", id)
	}
	jsonWrite(w, 200, map[string]bool{"success": true})
}
