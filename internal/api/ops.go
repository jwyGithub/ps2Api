// ops.go —— 运维只读端点（/api/stats、/api/logs、/api/cache-probe）
// 与管理面板静态资源（dashboard* 处理器）。
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ps2api/internal/dashboard"
)

// cacheProbe 返回影子缓存探针的度量结果（潜在命中率 + single-flight 潜在收益）。
func (s *Server) cacheProbe(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	jsonWrite(w, 200, s.Router.CacheProbeStats())
}

// cacheProbeReset 清空探针，开始一个干净的度量窗口。
func (s *Server) cacheProbeReset(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	if err := s.Store.ResetCacheProbe(); err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]bool{"success": true})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	v, e := s.Store.GetStats()
	if e != nil {
		jsonError(w, 500, e.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, v)
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	limit := 100
	if v, _ := s.Store.GetSetting("log_retention"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	v, e := s.Store.RecentLogs(limit)
	if e != nil {
		jsonError(w, 500, e.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"data": v})
}
// requestLogs 按「指纹会话」分页返回完整请求日志（入站请求体 + 转发给上游的请求体 + 元数据），
// 供面板「请求日志」页排查网关的请求转换。同一会话的多轮调用会聚在一起并按调用先后排列，
// 便于看清会话内的前后调用关系。page 从 1 起，pageSize 以「会话」为单位，默认 20、上限 100；
// total 返回会话总组数。
func (s *Server) requestLogs(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	page := 1
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = v
	}
	pageSize := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("pageSize")); err == nil && v > 0 && v <= 100 {
		pageSize = v
	}
	total, err := s.Store.CountRequestLogSessions()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	logs, err := s.Store.PageRequestLogsGrouped((page-1)*pageSize, pageSize)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"data": logs, "total": total, "page": page, "pageSize": pageSize, "grouped": true})
}

// sqlQuery 面板「数据查询」页：对 SQLite 执行只读查询（SELECT/WITH/EXPLAIN），
// 最多 200 行、单元格超长截断；写操作与 PRAGMA 一律拒绝（见 store.RunReadOnlyQuery）。
func (s *Server) sqlQuery(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var body struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || strings.TrimSpace(body.SQL) == "" {
		jsonError(w, 400, `请求体需为 {"sql":"SELECT ..."}`, "invalid_request_error")
		return
	}
	started := time.Now()
	cols, rows, truncated, err := s.Store.RunReadOnlyQuery(body.SQL, 200)
	if err != nil {
		jsonError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{
		"columns": cols, "rows": rows, "count": len(rows),
		"truncated": truncated, "elapsedMs": time.Since(started).Milliseconds(),
	})
}

// loginPage 提供面板登录页（static/login.html）。登录未开启或已持有效会话时
// 直接回面板首页。
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !loginEnabled() || validSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	data, err := dashboard.Files.ReadFile("static/login.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	// 登录开启且无有效会话：跳登录页（/v1/* 与 /health 不受影响）。
	if loginEnabled() && !validSession(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	data, err := dashboard.Files.ReadFile("static/index.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<h1>Dashboard not found. Place index.html in internal/dashboard/static/</h1>")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) dashboardAsset(w http.ResponseWriter, r *http.Request) {
	data, err := dashboard.Files.ReadFile("static/dashboard.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Write(data)
}

func (s *Server) dashboardStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/dashboard/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := dashboard.Files.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(name, ".html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Write(data)
}
