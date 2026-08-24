// ops.go —— 运维只读端点（/api/stats、/api/logs、/api/cache-probe）
// 与管理面板静态资源（dashboard* 处理器）。
package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"

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
	v, e := s.Store.GetStats()
	if e != nil {
		jsonError(w, 500, e.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, v)
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
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
