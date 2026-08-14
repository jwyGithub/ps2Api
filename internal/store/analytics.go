package store

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// ─── 延迟 / 成本统计 ─────────────────────────────────────────────

// LatencyAndCostStats 基于真实请求日志计算平均延迟（ms）、P95 延迟（ms）与估算成本（USD）。
// 成本按模型输入/输出 token 单价估算，token 本身是估算值，因此成本也标记为估算。
func (s *Store) LatencyAndCostStats() (avg float64, p95 float64, cost float64, err error) {
	rows, err := s.db.Query(`SELECT model, prompt_tokens, completion_tokens, duration_ms FROM request_logs`)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	var durations []int64
	var totalDur, n float64
	for rows.Next() {
		var model sql.NullString
		var p, c int64
		var d sql.NullInt64
		if err := rows.Scan(&model, &p, &c, &d); err != nil {
			return 0, 0, 0, err
		}
		if d.Valid && d.Int64 > 0 {
			durations = append(durations, d.Int64)
			totalDur += float64(d.Int64)
			n++
		}
		cost += modelCost(model.String, float64(p), float64(c))
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}
	if n > 0 {
		avg = totalDur / n
	}
	if len(durations) > 0 {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		idx := int(float64(len(durations))*0.95) - 1
		if idx < 0 {
			idx = 0
		}
		p95 = float64(durations[idx])
	}
	return avg, p95, cost, nil
}

// modelCost 返回一次请求的估算成本。价格单位：USD / 1M tokens。
func modelCost(model string, promptTokens, completionTokens float64) float64 {
	in, out := modelPrice(model)
	return promptTokens*in/1e6 + completionTokens*out/1e6
}

func modelPrice(model string) (in, out float64) {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "claude-opus"):
		return 15, 75
	case strings.HasPrefix(m, "claude-sonnet"):
		return 3, 15
	case strings.HasPrefix(m, "claude-haiku"):
		return 1, 5
	case strings.HasPrefix(m, "gpt-5.6"):
		return 1.25, 10
	case strings.HasPrefix(m, "gpt-5.5"):
		return 1.25, 10
	case strings.HasPrefix(m, "gpt-5.4"):
		return 2.5, 10
	case strings.HasPrefix(m, "gpt-5.2"):
		return 1.25, 10
	}
	return 3, 15
}

// ─── 时序聚合 ───────────────────────────────────────────────────

type SeriesPoint struct {
	Label   string `json:"label"`
	Total   int64  `json:"total"`
	Success int64  `json:"success"`
	Error   int64  `json:"error"`
}

// DailySeries 按自然日聚合最近 days 天的请求量（含今天）。\n
func (s *Store) DailySeries(days int) ([]*SeriesPoint, error) {
	if days <= 0 {
		days = 14
	}
	// created_at 可能被驱动写成 "2026-08-12 20:34:40.9893571 +0800 CST m=+..." 这种
	// SQLite 无法直接解析的格式，date() 会返回 NULL。统一截取前 19 位按本地墙钟解析，
	// 并对 NULL 兜底，避免 Scan 报 "converting NULL to string"。
	rows, err := s.db.Query(`SELECT COALESCE(date(substr(created_at,1,19)),'1970-01-01') d, COUNT(*),
		COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0)
		FROM request_logs WHERE substr(created_at,1,19) >= date('now','localtime','-`+itoa(days-1)+` day')
		GROUP BY d ORDER BY d`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[string]*SeriesPoint{}
	for rows.Next() {
		var d string
		p := &SeriesPoint{}
		if err := rows.Scan(&d, &p.Total, &p.Success, &p.Error); err != nil {
			return nil, err
		}
		p.Label = d
		byDay[d] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 补齐无请求的日期，保证曲线连续。时间一律按本地墙钟（与 substr 解析一致）。
	var out []*SeriesPoint
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		if p, ok := byDay[d]; ok {
			out = append(out, p)
		} else {
			out = append(out, &SeriesPoint{Label: d})
		}
	}
	return out, nil
}

// HourlySeries 按小时聚合最近 hours 小时（含当前小时）。
func (s *Store) HourlySeries(hours int) ([]*SeriesPoint, error) {
	if hours <= 0 {
		hours = 24
	}
	// 边界用 strftime 截断到小时（部分 SQLite 版本不支持 'start of hour' 修饰符）。
	// created_at 先 substr 前 19 位按本地墙钟解析（兼容驱动的长格式），NULL 兜底防 Scan 报错。
	rows, err := s.db.Query(`SELECT COALESCE(strftime('%Y-%m-%d %H:00',substr(created_at,1,19)),'1970-01-01 00:00') h, COUNT(*),
		COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0)
		FROM request_logs WHERE substr(created_at,1,19) >= strftime('%Y-%m-%d %H:00:00','now','localtime','-`+itoa(hours-1)+` hours')
		GROUP BY h ORDER BY h`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byHour := map[string]*SeriesPoint{}
	for rows.Next() {
		var h string
		p := &SeriesPoint{}
		if err := rows.Scan(&h, &p.Total, &p.Success, &p.Error); err != nil {
			return nil, err
		}
		p.Label = h
		byHour[h] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*SeriesPoint
	now := time.Now() // 本地墙钟，与 SQL 侧 substr 解析一致
	start := now.Add(-time.Duration(hours-1) * time.Hour).Truncate(time.Hour)
	for i := 0; i < hours; i++ {
		h := start.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:00")
		if p, ok := byHour[h]; ok {
			out = append(out, p)
		} else {
			out = append(out, &SeriesPoint{Label: h})
		}
	}
	return out, nil
}

// ─── 分布 ───────────────────────────────────────────────────────

type ModelUsage struct {
	Model  string `json:"model"`
	Count  int64  `json:"count"`
	Tokens int64  `json:"tokens"`
}

func (s *Store) ModelDistribution(days int) ([]*ModelUsage, error) {
	q := `SELECT COALESCE(NULLIF(model,''),'(未标注)') m, COUNT(*), COALESCE(SUM(total_tokens),0) FROM request_logs`
	if days > 0 {
		q += ` WHERE substr(created_at,1,19) >= datetime('now','localtime','-` + itoa(days) + ` days')`
	}
	q += ` GROUP BY m ORDER BY COUNT(*) DESC`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ModelUsage
	for rows.Next() {
		u := &ModelUsage{}
		if err := rows.Scan(&u.Model, &u.Count, &u.Tokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ─── 账号 / 渠道效能 ────────────────────────────────────────────

type AccountAgg struct {
	AccountID    int64   `json:"accountId"`
	Email        string  `json:"email"`
	Source       string  `json:"source"`
	Calls        int64   `json:"calls"`
	Success      int64   `json:"success"`
	Error        int64   `json:"error"`
	Tokens       int64   `json:"tokens"`
	AvgLatencyMs float64 `json:"avgLatencyMs"`
	SuccessRate  float64 `json:"successRate"`
	Score        float64 `json:"score"`
}

func (s *Store) AccountAggregates(days, limit int) ([]*AccountAgg, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT COALESCE(r.account_id,0) aid, COALESCE(a.email,'(已删除账号)') email, COALESCE(a.source,'') src,
		COUNT(*), COALESCE(SUM(CASE WHEN r.status='success' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.status='error' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(r.total_tokens),0), COALESCE(AVG(r.duration_ms),0)
		FROM request_logs r LEFT JOIN accounts a ON a.id=r.account_id`
	if days > 0 {
		q += ` WHERE substr(r.created_at,1,19) >= datetime('now','localtime','-` + itoa(days) + ` days')`
	}
	q += ` GROUP BY aid ORDER BY COUNT(*) DESC LIMIT ?`
	rows, err := s.db.Query(q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AccountAgg
	for rows.Next() {
		a := &AccountAgg{}
		var aid sql.NullInt64
		if err := rows.Scan(&aid, &a.Email, &a.Source, &a.Calls, &a.Success, &a.Error, &a.Tokens, &a.AvgLatencyMs); err != nil {
			return nil, err
		}
		if aid.Valid {
			a.AccountID = aid.Int64
		}
		if a.Calls > 0 {
			a.SuccessRate = float64(a.Success) / float64(a.Calls)
		}
		// 效能分：成功率 60% + 调用量 25% + 低延迟 15%，归一化到 0-100
		latScore := 1.0
		if a.AvgLatencyMs > 0 {
			latScore = 2000.0 / (a.AvgLatencyMs + 2000.0)
			if latScore > 1 {
				latScore = 1
			}
		}
		a.Score = (a.SuccessRate*60 + 25 + latScore*15)
		out = append(out, a)
	}
	return out, rows.Err()
}

type ChannelAgg struct {
	Channel      string  `json:"channel"`
	Calls        int64   `json:"calls"`
	Success      int64   `json:"success"`
	Error        int64   `json:"error"`
	Tokens       int64   `json:"tokens"`
	AvgLatencyMs float64 `json:"avgLatencyMs"`
	SuccessRate  float64 `json:"successRate"`
	Cost         float64 `json:"cost"`
}

// ChannelComparison 按「渠道」聚合：渠道由账号 token 类型决定（桌面 x-access-token vs Web postman.sid）。
func (s *Store) ChannelComparison(days int) ([]*ChannelAgg, error) {
	accounts, err := s.ListAccounts()
	if err != nil {
		return nil, err
	}
	channelOf := map[int64]string{}
	for _, a := range accounts {
		if strings.Contains(a.Tokens, "access_token") && !strings.Contains(a.Tokens, "postman_sid") {
			channelOf[a.ID] = "Postman Desktop"
		} else if strings.Contains(a.Tokens, "postman_sid") {
			channelOf[a.ID] = "Postman Web"
		} else {
			channelOf[a.ID] = "未知渠道"
		}
	}
	q := `SELECT COALESCE(account_id,0), model, COUNT(*),
		COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(total_tokens),0), COALESCE(AVG(duration_ms),0),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0)
		FROM request_logs`
	if days > 0 {
		q += ` WHERE substr(created_at,1,19) >= datetime('now','localtime','-` + itoa(days) + ` days')`
	}
	q += ` GROUP BY account_id, model`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agg := map[string]*ChannelAgg{}
	order := []string{}
	key := func(ch string) string { return ch }
	for rows.Next() {
		var aid sql.NullInt64
		var model sql.NullString
		var calls, success, errs, tokens, p, c int64
		var avg float64
		if err := rows.Scan(&aid, &model, &calls, &success, &errs, &tokens, &avg, &p, &c); err != nil {
			return nil, err
		}
		id := int64(0)
		if aid.Valid {
			id = aid.Int64
		}
		ch := channelOf[id]
		if ch == "" {
			ch = "未知渠道"
		}
		a, ok := agg[key(ch)]
		if !ok {
			a = &ChannelAgg{Channel: ch}
			agg[key(ch)] = a
			order = append(order, ch)
		}
		a.Calls += calls
		a.Success += success
		a.Error += errs
		a.Tokens += tokens
		a.Cost += modelCost(model.String, float64(p), float64(c))
		if avg > 0 && calls > 0 {
			// 加权平均（按调用量）
			a.AvgLatencyMs = (a.AvgLatencyMs*float64(a.Calls-calls) + avg*float64(calls)) / float64(a.Calls)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*ChannelAgg
	for _, ch := range order {
		a := agg[ch]
		if a.Calls > 0 {
			a.SuccessRate = float64(a.Success) / float64(a.Calls)
		}
		out = append(out, a)
	}
	return out, nil
}

// ─── 活跃热力图 ────────────────────────────────────────────────

type HeatCell struct {
	Weekday int   `json:"weekday"` // 0=周日
	Hour    int   `json:"hour"`
	Count   int64 `json:"count"`
}

// Heatmap 统计最近 days 天内 星期×小时 的请求分布。
func (s *Store) Heatmap(days int) ([]*HeatCell, error) {
	if days <= 0 {
		days = 7
	}
	rows, err := s.db.Query(`SELECT COALESCE(CAST(strftime('%w',substr(created_at,1,19)) AS INTEGER),0),
		COALESCE(CAST(strftime('%H',substr(created_at,1,19)) AS INTEGER),0), COUNT(*)
		FROM request_logs WHERE substr(created_at,1,19) >= datetime('now','localtime','-`+itoa(days)+` days')
		GROUP BY 1,2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[int]map[int]int64{}
	for rows.Next() {
		var w, h int
		var c int64
		if err := rows.Scan(&w, &h, &c); err != nil {
			return nil, err
		}
		if by[w] == nil {
			by[w] = map[int]int64{}
		}
		by[w][h] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*HeatCell
	for w := 0; w < 7; w++ {
		for h := 0; h < 24; h++ {
			out = append(out, &HeatCell{Weekday: w, Hour: h, Count: by[w][h]})
		}
	}
	return out, nil
}

// ─── 今日调用（按账号）──────────────────────────────────────────

func (s *Store) TodayCallsByAccount() (map[int64]int64, error) {
	rows, err := s.db.Query(`SELECT COALESCE(account_id,0), COUNT(*) FROM request_logs WHERE substr(created_at,1,19) >= date('now') GROUP BY account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, c int64
		if err := rows.Scan(&id, &c); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
