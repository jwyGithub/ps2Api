package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ps2api/internal/store"
)

// ─── 系统设置（真实持久化到 SQLite settings 表）──────────────────

type settingDef struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"` // number | bool | text
	Default     string `json:"default"`
	Description string `json:"description"`
}

// settingDefs 是面板可配置项的白名单。前端按此渲染表单，PUT 只接受这些键。
var settingDefs = []settingDef{
	{Key: "retry_count", Label: "请求重试次数", Type: "number", Default: "3", Description: "单次请求失败后最多重试几次（在号池内切换账号）"},
	{Key: "failover_enabled", Label: "失败自动切换账号", Type: "bool", Default: "true", Description: "关闭后单次请求失败不再换号重试"},
	{Key: "alert_error_rate", Label: "错误率告警阈值", Type: "number", Default: "0.5", Description: "最近 10 分钟错误率超过该比例（0~1）时触发告警"},
	{Key: "alert_quota", Label: "额度告警阈值", Type: "number", Default: "0.2", Description: "账号剩余额度低于总配额该比例（0~1）时触发告警"},
	{Key: "log_retention", Label: "日志页展示条数", Type: "number", Default: "100", Description: "实时日志与部分聚合最多展示的最近日志条数"},
	{Key: "cache_probe_enabled", Label: "缓存探针（影子度量）", Type: "bool", Default: "false", Description: "只度量不改返回：记录可缓存请求指纹，用真实流量测潜在命中率。长期开启会让探针表增长，测完可关"},
}

func defaultSettings() map[string]string {
	m := map[string]string{}
	for _, d := range settingDefs {
		m[d.Key] = d.Default
	}
	return m
}

// allSettings 返回默认值 + 数据库覆盖后的完整配置。
func (s *Server) allSettings() map[string]string {
	out := defaultSettings()
	rows, err := s.Store.ListSettings()
	if err != nil {
		return out
	}
	for k, v := range rows {
		if _, ok := out[k]; ok {
			out[k] = v
		}
	}
	return out
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.allSettings()
	// 返回真实 API Key：面板初始化时读取并缓存到本地，之后每个请求带上它。
	// 面板本身是本机只读入口，与账号 token 同一信任边界。
	key := s.apiKey()
	jsonWrite(w, 200, map[string]interface{}{
		"settings":  settings,
		"defs":      settingDefs,
		"apiKey":    key,
		"apiKeySet": key != "",
	})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var q struct {
		Settings map[string]string `json:"settings"`
		// APIKey 用指针区分「未提供」(nil) 与「清空以关闭鉴权」("")。
		APIKey *string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		jsonError(w, 400, err.Error(), "invalid_request")
		return
	}
	if q.APIKey != nil {
		if err := s.Store.SetSetting("api_key", strings.TrimSpace(*q.APIKey)); err != nil {
			jsonError(w, 500, err.Error(), "internal_error")
			return
		}
	}
	valid := map[string]bool{}
	for _, d := range settingDefs {
		valid[d.Key] = true
	}
	for k, v := range q.Settings {
		if !valid[k] {
			jsonError(w, 400, "unknown setting key: "+k, "invalid_request")
			return
		}
		if v == "" {
			continue
		}
		if err := s.Store.SetSetting(k, v); err != nil {
			jsonError(w, 500, err.Error(), "internal_error")
			return
		}
	}
	jsonWrite(w, 200, map[string]interface{}{"success": true, "settings": s.allSettings()})
}

// ─── 统计分析聚合 ───────────────────────────────────────────────

func (s *Server) analytics(w http.ResponseWriter, r *http.Request) {
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	daily, err := s.Store.DailySeries(days)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	hourly, err := s.Store.HourlySeries(24)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	models, err := s.Store.ModelDistribution(days)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	top, err := s.Store.AccountAggregates(days, 20)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	channels, err := s.Store.ChannelComparison(days)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	heatmap, err := s.Store.Heatmap(days)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	today, err := s.Store.TodayCallsByAccount()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	accounts, err := s.Store.ListAccounts()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{
		"daily":         daily,
		"hourly":        hourly,
		"models":        models,
		"topAccounts":   top,
		"channels":      channels,
		"heatmap":       heatmap,
		"todayCalls":    today,
		"quotaForecast": buildQuotaForecast(accounts, time.Now()),
	})
}

// quotaForecast 按账号官方额度周期折算日均消耗，再推算到当前自然月月底。
type quotaForecast struct {
	Month               string  `json:"month"`
	DaysInMonth         int     `json:"daysInMonth"`
	DaysElapsed         int     `json:"daysElapsed"`
	DaysRemaining       int     `json:"daysRemaining"`
	ObservedAccounts    int     `json:"observedAccounts"`
	TotalAccounts       int     `json:"totalAccounts"`
	TotalLimit          float64 `json:"totalLimit"`
	TotalUsed           float64 `json:"totalUsed"`
	CurrentRemaining    float64 `json:"currentRemaining"`
	DailyConsumption    float64 `json:"dailyConsumption"`
	ForecastAdditional  float64 `json:"forecastAdditional"`
	ForecastMonthUsage  float64 `json:"forecastMonthUsage"`
	ForecastRemaining   float64 `json:"forecastRemaining"`
	Shortfall           float64 `json:"shortfall"`
	AverageAccountLimit float64 `json:"averageAccountLimit"`
	SuggestedAccounts   int     `json:"suggestedAccounts"`
	CoverageDays        float64 `json:"coverageDays"`
	NeedsRefill         bool    `json:"needsRefill"`
	Status              string  `json:"status"` // sufficient | refill | insufficient_data
}

func buildQuotaForecast(accounts []*store.Account, now time.Time) quotaForecast {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	nextMonth := monthStart.AddDate(0, 1, 0)
	daysInMonth := nextMonth.Add(-time.Nanosecond).Day()
	daysElapsed := now.Day()
	daysRemaining := daysInMonth - daysElapsed
	if daysRemaining < 0 {
		daysRemaining = 0
	}

	f := quotaForecast{
		Month:         monthStart.Format("2006-01"),
		DaysInMonth:   daysInMonth,
		DaysElapsed:   daysElapsed,
		DaysRemaining: daysRemaining,
		TotalAccounts: len(accounts),
		Status:        "insufficient_data",
	}
	dailyConsumption := 0.0
	for _, account := range accounts {
		if account == nil || !account.Enabled || account.QuotaLimit <= 0 {
			continue
		}
		f.ObservedAccounts++
		f.TotalLimit += account.QuotaLimit
		f.CurrentRemaining += maxFloat(account.QuotaRemaining, 0)
		used := account.QuotaUsed
		// 兼容只有总量/剩余量的旧快照。
		if used <= 0 && account.QuotaLimit > account.QuotaRemaining {
			used = account.QuotaLimit - account.QuotaRemaining
		}
		if used > 0 {
			f.TotalUsed += used
			elapsed := daysElapsed
			if account.QuotaCycleStart != nil && account.QuotaCycleStart.Before(now) {
				cycleDays := int(math.Ceil(now.Sub(*account.QuotaCycleStart).Hours() / 24))
				if cycleDays > 0 {
					elapsed = cycleDays
				}
			}
			dailyConsumption += used / float64(maxInt(elapsed, 1))
		}
	}
	if f.ObservedAccounts == 0 {
		return f
	}
	f.DailyConsumption = dailyConsumption
	f.ForecastAdditional = f.DailyConsumption * float64(daysRemaining)
	f.ForecastMonthUsage = f.DailyConsumption * float64(daysInMonth)
	f.ForecastRemaining = f.CurrentRemaining - f.ForecastAdditional
	if f.ForecastRemaining < 0 {
		f.Shortfall = -f.ForecastRemaining
	}
	f.AverageAccountLimit = f.TotalLimit / float64(f.ObservedAccounts)
	if f.AverageAccountLimit > 0 && f.Shortfall > 0 {
		f.SuggestedAccounts = int(math.Ceil(f.Shortfall / f.AverageAccountLimit))
	}
	if f.DailyConsumption > 0 {
		f.CoverageDays = f.CurrentRemaining / f.DailyConsumption
	}
	f.NeedsRefill = f.Shortfall > 0
	if f.NeedsRefill {
		f.Status = "refill"
	} else {
		f.Status = "sufficient"
	}
	return f
}

func maxFloat(value, floor float64) float64 {
	if value < floor {
		return floor
	}
	return value
}

func maxInt(value, floor int) int {
	if value < floor {
		return floor
	}
	return value
}

// ─── 告警中心（真实告警记录）────────────────────────────────────

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	list, err := s.Store.ListAlerts(status, limit)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	sum, err := s.Store.AlertSummary()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"data": list, "summary": sum})
}

func (s *Server) resolveAlert(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Store.ResolveAlert(id); err != nil {
		jsonError(w, 404, err.Error(), "not_found")
		return
	}
	jsonWrite(w, 200, map[string]bool{"success": true})
}

func (s *Server) resolveAllAlerts(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	if err := s.Store.ResolveAllOpen(); err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]bool{"success": true})
}

// ─── 告警后台评估器 ─────────────────────────────────────────────

// runAlertEvaluator 每 60s 基于真实统计数据评估并落告警：
//  1. 最近 10 分钟错误率超过 alert_error_rate（且样本数 ≥ 5）→ high_error_rate
//  2. 启用账号剩余额度低于 alert_quota 比例 → low_quota
//
// 错误率恢复后自动解决 high_error_rate 告警（MTTR 由真实解决时间计算）。
func (s *Server) runAlertEvaluator() {
	for {
		time.Sleep(60 * time.Second)
		s.evaluateAlerts()
	}
}

func (s *Server) evaluateAlerts() {
	settings := s.allSettings()
	errRate, _ := strconv.ParseFloat(settings["alert_error_rate"], 64)
	if errRate < 0 || errRate > 1 {
		errRate = 0.5
	}
	quotaTh, _ := strconv.ParseFloat(settings["alert_quota"], 64)
	if quotaTh < 0 || quotaTh > 1 {
		quotaTh = 0.2
	}

	// 1) 错误率
	var total, errs int
	_ = s.Store.QueryRowScan(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0) FROM request_logs WHERE created_at>=datetime('now','-10 minutes')`, &total, &errs)
	if total >= 5 {
		rate := float64(errs) / float64(total)
		if rate > errRate {
			msg := "最近 10 分钟错误率 " + trimFloat(rate*100) + "%（阈值 " + trimFloat(errRate*100) + "%），共 " + strconv.Itoa(errs) + "/" + strconv.Itoa(total) + " 次失败"
			_ = s.Store.CreateAlert("warning", "错误率过高", msg, "system", nil, "high_error_rate")
		} else {
			_ = s.Store.ResolveAllByType("system", "high_error_rate")
		}
	}

	// 2) 额度
	accounts, err := s.Store.ListAccounts()
	if err != nil {
		return
	}
	for _, a := range accounts {
		if !a.Enabled || a.Status != "active" {
			continue
		}
		if a.QuotaLimit <= 0 || a.QuotaRemaining <= 0 {
			continue
		}
		ratio := a.QuotaRemaining / a.QuotaLimit
		if ratio < quotaTh {
			sid := a.ID
			_ = s.Store.CreateAlert("warning", "账号额度不足: "+a.Email,
				"剩余 "+trimFloat(a.QuotaRemaining)+" / "+trimFloat(a.QuotaLimit)+"（"+trimFloat(ratio*100)+"%），低于阈值 "+trimFloat(quotaTh*100)+"%",
				"account", &sid, "low_quota")
		}
	}
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 1, 64)
}
