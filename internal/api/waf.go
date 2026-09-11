// waf.go —— 面板「WAF 检测」页的端点：签名表暴露（单一事实源出口）、
// 对照候选列表、离线分析（analyze，见 Task 5）。全部只读、零出站请求。
package api

import (
	"net/http"
	"strconv"

	"ps2api/internal/provider"
)

// wafSignatures 返回 WAF 特征子串表（前端 SQL 预设等从这里取，单一事实源）。
func (s *Server) wafSignatures(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"probes": provider.WafSignatureProbes()})
}

// wafBaselines 返回某条 403 日志的对照候选成功请求（手动换对照的下拉数据源）。
func (s *Server) wafBaselines(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	logID, _ := strconv.ParseInt(r.URL.Query().Get("log_id"), 10, 64)
	if logID <= 0 {
		jsonError(w, 400, "log_id 必须为正整数", "invalid_request_error")
		return
	}
	target, err := s.Store.GetRequestLog(logID)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	if target == nil {
		jsonError(w, 404, "日志不存在", "invalid_request_error")
		return
	}
	cands, err := s.Store.WafBaselineCandidates(target, 9)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	data := make([]map[string]interface{}, 0, len(cands))
	for _, c := range cands {
		data = append(data, map[string]interface{}{
			"id": c.ID, "createdAt": c.CreatedAt, "requestBytes": c.RequestBytes,
			"accountEmail": c.AccountEmail, "conversationId": c.ConversationID,
		})
	}
	jsonWrite(w, 200, map[string]interface{}{"data": data})
}
