package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Account struct {
	ID                     int64            `json:"id"`
	Email                  string           `json:"email"`
	Password               string           `json:"-"`
	Status                 string           `json:"status"` // active | exhausted | error | pending
	Enabled                bool             `json:"enabled"`
	Tokens                 string           `json:"-"`      // JSON blob
	Source                 string           `json:"source"` // manual | local | detect-web | browser
	Plan                   string           `json:"plan"`   // 来自 Postman usage.userType
	QuotaLimit             float64          `json:"quotaLimit"`
	QuotaUsed              float64          `json:"quotaUsed"`
	QuotaRemaining         float64          `json:"quotaRemaining"`
	QuotaOverage           float64          `json:"quotaOverage"`
	QuotaSpillage          float64          `json:"quotaSpillage"`
	QuotaState             string           `json:"quotaState"`
	QuotaAllowOverage      bool             `json:"quotaAllowOverage"`
	QuotaTeamPooled        bool             `json:"quotaTeamPooled"`
	QuotaWarningThresholds []QuotaThreshold `json:"quotaWarningThresholds"`
	QuotaCycleStart        *time.Time       `json:"quotaCycleStart"`
	QuotaCycleEnd          *time.Time       `json:"quotaCycleEnd"`
	RateLimit              int              `json:"rateLimit"`
	RateRemaining          int              `json:"rateRemaining"`
	RateWindowSeconds      int              `json:"rateWindowSeconds"`
	RateResetAt            *time.Time       `json:"rateResetAt"`
	LastUsedAt             *time.Time       `json:"lastUsedAt"`
	LastLoginAt            *time.Time       `json:"lastLoginAt"`
	ErrorMessage           string           `json:"errorMessage"`
	CreatedAt              time.Time        `json:"createdAt"`
	UpdatedAt              time.Time        `json:"updatedAt"`
}

type QuotaThreshold struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

type QuotaSnapshot struct {
	Plan              string
	State             string
	Limit             float64
	Used              float64
	Remaining         float64
	Overage           float64
	Spillage          float64
	AllowOverage      bool
	TeamPooled        bool
	WarningThresholds []QuotaThreshold
	CycleStart        *time.Time
	CycleEnd          *time.Time
}

type Alert struct {
	ID         int64      `json:"id"`
	Level      string     `json:"level"` // severe | warning | info
	Title      string     `json:"title"`
	Message    string     `json:"message"`
	SourceType string     `json:"sourceType"` // account | system
	SourceID   *int64     `json:"sourceId,omitempty"`
	AlertType  string     `json:"alertType"` // account_error | quota_exhausted | low_quota | high_error_rate
	Status     string     `json:"status"`    // open | resolved
	CreatedAt  time.Time  `json:"createdAt"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

type RequestLog struct {
	ID               int64     `json:"id"`
	AccountID        *int64    `json:"accountId"`
	Model            string    `json:"model"`
	Endpoint         string    `json:"endpoint"` // anthropic | openai：调用来源兼容端点
	PromptTokens     int       `json:"promptTokens"`
	CompletionTokens int       `json:"completionTokens"`
	TotalTokens      int       `json:"totalTokens"`
	Status           string    `json:"status"` // success | error
	DurationMs       int64     `json:"durationMs"`
	ErrorMessage     string    `json:"errorMessage"`
	RequestBytes     int       `json:"requestBytes"` // 出站请求体字节数，用于 403 与体积相关性分析
	CreatedAt        time.Time `json:"createdAt"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS accounts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  email TEXT NOT NULL UNIQUE,
  password TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  enabled INTEGER NOT NULL DEFAULT 1,
  tokens TEXT,
  quota_limit REAL DEFAULT 0,
  quota_used REAL DEFAULT 0,
  quota_remaining REAL DEFAULT 0,
  quota_overage REAL DEFAULT 0,
  quota_spillage REAL DEFAULT 0,
  quota_state TEXT NOT NULL DEFAULT '',
  quota_allow_overage INTEGER NOT NULL DEFAULT 0,
  quota_team_pooled INTEGER NOT NULL DEFAULT 0,
  quota_warning_thresholds TEXT NOT NULL DEFAULT '[]',
  quota_cycle_start DATETIME,
  quota_cycle_end DATETIME,
  rate_limit INTEGER NOT NULL DEFAULT 0,
  rate_remaining INTEGER NOT NULL DEFAULT 0,
  rate_window_seconds INTEGER NOT NULL DEFAULT 0,
  rate_reset_at DATETIME,
  last_used_at DATETIME,
  last_login_at DATETIME,
  error_message TEXT,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS request_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id INTEGER REFERENCES accounts(id),
  model TEXT,
  endpoint TEXT NOT NULL DEFAULT '',
  prompt_tokens INTEGER DEFAULT 0,
  completion_tokens INTEGER DEFAULT 0,
  total_tokens INTEGER DEFAULT 0,
  status TEXT NOT NULL,
  duration_ms INTEGER,
  error_message TEXT,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS request_logs_created_idx ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS request_logs_account_idx ON request_logs(account_id);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS alerts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  level TEXT NOT NULL DEFAULT 'warning',
  title TEXT NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  source_type TEXT NOT NULL DEFAULT 'system',
  source_id INTEGER,
  alert_type TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'open',
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  resolved_at DATETIME
);
CREATE INDEX IF NOT EXISTS alerts_status_idx ON alerts(status);
CREATE INDEX IF NOT EXISTS alerts_created_idx ON alerts(created_at);
CREATE TABLE IF NOT EXISTS cache_probe (
  key TEXT PRIMARY KEY,
  hits INTEGER NOT NULL DEFAULT 0,
  first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`)
	if err != nil {
		return err
	}
	accountColumns := []struct{ name, decl string }{
		{"source", "TEXT NOT NULL DEFAULT ''"}, {"plan", "TEXT NOT NULL DEFAULT ''"},
		{"quota_used", "REAL DEFAULT 0"}, {"quota_overage", "REAL DEFAULT 0"}, {"quota_spillage", "REAL DEFAULT 0"},
		{"quota_state", "TEXT NOT NULL DEFAULT ''"}, {"quota_allow_overage", "INTEGER NOT NULL DEFAULT 0"},
		{"quota_team_pooled", "INTEGER NOT NULL DEFAULT 0"}, {"quota_warning_thresholds", "TEXT NOT NULL DEFAULT '[]'"},
		{"quota_cycle_start", "DATETIME"}, {"quota_cycle_end", "DATETIME"},
		{"rate_limit", "INTEGER NOT NULL DEFAULT 0"}, {"rate_remaining", "INTEGER NOT NULL DEFAULT 0"},
		{"rate_window_seconds", "INTEGER NOT NULL DEFAULT 0"}, {"rate_reset_at", "DATETIME"},
	}
	for _, column := range accountColumns {
		if err := s.ensureColumn("accounts", column.name, column.decl); err != nil {
			return err
		}
	}
	// 兼容旧库：补齐 request_logs.endpoint（调用来源端点）
	if err := s.ensureColumn("request_logs", "endpoint", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// 兼容旧库：补齐 request_logs.request_bytes（出站请求体大小，供 403 体积相关性分析）
	return s.ensureColumn("request_logs", "request_bytes", "INTEGER NOT NULL DEFAULT 0")
}

// ensureColumn 检查表是否已有指定列，没有则 ALTER TABLE 补齐（SQLite 无 IF NOT EXISTS）。
func (s *Store) ensureColumn(table, col, decl string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + col + " " + decl)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ListAccounts() ([]*Account, error) {
	rows, err := s.db.Query(`SELECT id,email,password,status,enabled,tokens,source,plan,
		quota_limit,quota_used,quota_remaining,quota_overage,quota_spillage,quota_state,
		quota_allow_overage,quota_team_pooled,quota_warning_thresholds,quota_cycle_start,quota_cycle_end,
		rate_limit,rate_remaining,rate_window_seconds,rate_reset_at,
		last_used_at,last_login_at,error_message,created_at,updated_at FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a := &Account{}
		var enabled, allowOverage, teamPooled int
		var tokens, source, plan, quotaState, thresholds, errmsg sql.NullString
		var cycleStart, cycleEnd, rateReset, lastUsed, lastLogin sql.NullTime
		if err := rows.Scan(
			&a.ID, &a.Email, &a.Password, &a.Status, &enabled, &tokens, &source, &plan,
			&a.QuotaLimit, &a.QuotaUsed, &a.QuotaRemaining, &a.QuotaOverage, &a.QuotaSpillage, &quotaState,
			&allowOverage, &teamPooled, &thresholds, &cycleStart, &cycleEnd,
			&a.RateLimit, &a.RateRemaining, &a.RateWindowSeconds, &rateReset,
			&lastUsed, &lastLogin, &errmsg, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		a.Enabled = enabled != 0
		a.QuotaAllowOverage = allowOverage != 0
		a.QuotaTeamPooled = teamPooled != 0
		a.Tokens = tokens.String
		a.Source = source.String
		a.Plan = plan.String
		a.QuotaState = quotaState.String
		_ = json.Unmarshal([]byte(thresholds.String), &a.QuotaWarningThresholds)
		a.ErrorMessage = errmsg.String
		if cycleStart.Valid {
			a.QuotaCycleStart = &cycleStart.Time
		}
		if cycleEnd.Valid {
			a.QuotaCycleEnd = &cycleEnd.Time
		}
		if rateReset.Valid {
			a.RateResetAt = &rateReset.Time
		}
		if lastUsed.Valid {
			a.LastUsedAt = &lastUsed.Time
		}
		if lastLogin.Valid {
			a.LastLoginAt = &lastLogin.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ActiveAccounts() ([]*Account, error) {
	all, err := s.ListAccounts()
	if err != nil {
		return nil, err
	}
	var out []*Account
	for _, a := range all {
		if a.Status == "active" && a.Enabled {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *Store) GetAccount(id int64) (*Account, error) {
	all, err := s.ListAccounts()
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, fmt.Errorf("account %d not found", id)
}

func (s *Store) UpsertAccount(email, password, tokens, source string) (*Account, error) {
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO accounts (email,password,tokens,source,status,last_login_at,created_at,updated_at)
		VALUES (?,?,?,?,'active',?,?,?)
		ON CONFLICT(email) DO UPDATE SET tokens=excluded.tokens,source=excluded.source,status='active',last_login_at=excluded.last_login_at,error_message=NULL,updated_at=excluded.updated_at`,
		email, password, tokens, source, now, now, now)
	if err != nil {
		return nil, err
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM accounts WHERE email=?`, email).Scan(&id); err != nil {
		return nil, err
	}
	return s.GetAccount(id)
}

// ImportAccount upserts an account by email and returns its row id. The id lets
// callers（例如导入后自动刷新额度）精确定位到本次导入的账号，无需整池扫描。
func (s *Store) ImportAccount(email, password, tokens, source string, enabled bool) (int64, error) {
	v := 0
	if enabled {
		v = 1
	}
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO accounts (email,password,tokens,source,status,enabled,last_login_at,created_at,updated_at)
		VALUES (?,?,?,?,'active',?,?,?,?)
		ON CONFLICT(email) DO UPDATE SET password=excluded.password,tokens=excluded.tokens,source=excluded.source,status='active',enabled=excluded.enabled,last_login_at=excluded.last_login_at,error_message=NULL,updated_at=excluded.updated_at`,
		email, password, tokens, source, v, now, now, now)
	if err != nil {
		return 0, err
	}
	// upsert 命中冲突分支时 LastInsertId 不可靠，按 email 回查（与 UpsertAccount 一致）。
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM accounts WHERE email=?`, email).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) DeleteAccount(id int64) error {
	if _, err := s.db.Exec(`UPDATE request_logs SET account_id=NULL WHERE account_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM accounts WHERE id=?`, id)
	return err
}

func (s *Store) SetAccountEnabled(id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE accounts SET enabled=?,updated_at=? WHERE id=?`, v, time.Now(), id)
	return err
}

func (s *Store) SetAccountStatus(id int64, status, errmsg string) error {
	_, err := s.db.Exec(`UPDATE accounts SET status=?,error_message=?,updated_at=? WHERE id=?`, status, errmsg, time.Now(), id)
	return err
}

func (s *Store) MarkUsed(id int64) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE accounts SET last_used_at=?,updated_at=? WHERE id=?`, now, now, id)
	return err
}

func (s *Store) SetQuotaSnapshot(id int64, q QuotaSnapshot) error {
	thresholds, _ := json.Marshal(q.WarningThresholds)
	_, err := s.db.Exec(`UPDATE accounts SET plan=?,quota_state=?,quota_limit=?,quota_used=?,quota_remaining=?,
		quota_overage=?,quota_spillage=?,quota_allow_overage=?,quota_team_pooled=?,quota_warning_thresholds=?,
		quota_cycle_start=?,quota_cycle_end=?,updated_at=? WHERE id=?`,
		q.Plan, q.State, q.Limit, q.Used, q.Remaining, q.Overage, q.Spillage, q.AllowOverage, q.TeamPooled,
		string(thresholds), q.CycleStart, q.CycleEnd, time.Now(), id)
	return err
}

func (s *Store) SetRateLimit(id int64, limit, remaining, windowSeconds int, resetAt *time.Time) error {
	_, err := s.db.Exec(`UPDATE accounts SET rate_limit=?,rate_remaining=?,rate_window_seconds=?,rate_reset_at=?,updated_at=? WHERE id=?`,
		limit, remaining, windowSeconds, resetAt, time.Now(), id)
	return err
}

func (s *Store) SetPlan(id int64, plan string) error {
	if plan == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE accounts SET plan=?,updated_at=? WHERE id=?`, plan, time.Now(), id)
	return err
}

func (s *Store) UpdateTokens(id int64, tokens string) error {
	_, err := s.db.Exec(`UPDATE accounts SET tokens=?,updated_at=? WHERE id=?`, tokens, time.Now(), id)
	return err
}

func (s *Store) LogRequest(l *RequestLog) error {
	_, err := s.db.Exec(`INSERT INTO request_logs (account_id,model,endpoint,prompt_tokens,completion_tokens,total_tokens,status,duration_ms,error_message,request_bytes,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		l.AccountID, l.Model, l.Endpoint, l.PromptTokens, l.CompletionTokens, l.TotalTokens, l.Status, l.DurationMs, l.ErrorMessage, l.RequestBytes, time.Now())
	return err
}

// Cloudflare403BodySizeSummary 统计最近 window 内 Cloudflare 网关拒绝(403)错误日志，
// 按出站请求体大小分桶（以 80KB 软告警阈值为界）给出次数与占比，并据「超阈值占比」
// 给出 body 大小是否为主要诱因的判断。无匹配样本时返回空串（调用方据此不追加）。
// 用途：在 403 告警里用真实数据回答「请求体大小与 403 是否相关」，避免仅凭单条日志臆测。
func (s *Store) Cloudflare403BodySizeSummary(window time.Duration) (string, error) {
	minutes := int(window.Minutes())
	if minutes < 1 {
		minutes = 1
	}
	var b0, b1, b2, b3, b4, total int
	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN request_bytes < 16384 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN request_bytes >= 16384 AND request_bytes < 32768 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN request_bytes >= 32768 AND request_bytes < 65536 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN request_bytes >= 65536 AND request_bytes < 81920 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN request_bytes >= 81920 THEN 1 ELSE 0 END),0),
		COUNT(*)
		FROM request_logs
		WHERE status='error' AND error_message LIKE '%Cloudflare%'
		  AND created_at >= datetime('now', ?)`,
		fmt.Sprintf("-%d minutes", minutes)).Scan(&b0, &b1, &b2, &b3, &b4, &total)
	if err != nil {
		return "", err
	}
	if total == 0 {
		return "", nil
	}
	pct := func(n int) float64 { return float64(n) * 100 / float64(total) }
	out := fmt.Sprintf("近期 403 请求体大小分布（最近 %d 分钟，共 %d 次 Cloudflare 拒绝）:", minutes, total)
	out += fmt.Sprintf("\n  <16KB: %d 次 (%.0f%%)", b0, pct(b0))
	out += fmt.Sprintf("\n  16–32KB: %d 次 (%.0f%%)", b1, pct(b1))
	out += fmt.Sprintf("\n  32–64KB: %d 次 (%.0f%%)", b2, pct(b2))
	out += fmt.Sprintf("\n  64–80KB: %d 次 (%.0f%%)", b3, pct(b3))
	out += fmt.Sprintf("\n  >80KB(超软阈值): %d 次 (%.0f%%)", b4, pct(b4))
	hint := "多数 403 发生在正常体积请求，body 大小并非主要诱因（更可能是边缘风控评分/速率判定）"
	if float64(b4)/float64(total) >= 0.5 {
		hint = "多数 403 集中在超大请求体，body 大小可能是主要诱因，建议压缩出站 payload"
	}
	out += fmt.Sprintf("\n结论: 超软阈值(>80KB)的 403 占比 %.0f%%——%s", pct(b4), hint)
	return out, nil
}

func (s *Store) RecentLogs(limit int) ([]*RequestLog, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,account_id,model,endpoint,prompt_tokens,completion_tokens,total_tokens,status,duration_ms,error_message,request_bytes,created_at FROM request_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RequestLog
	for rows.Next() {
		l := &RequestLog{}
		var accID sql.NullInt64
		var model, endpoint, errmsg sql.NullString
		if err := rows.Scan(&l.ID, &accID, &model, &endpoint, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.Status, &l.DurationMs, &errmsg, &l.RequestBytes, &l.CreatedAt); err != nil {
			return nil, err
		}
		if accID.Valid {
			l.AccountID = &accID.Int64
		}
		l.Model = model.String
		l.Endpoint = endpoint.String
		l.ErrorMessage = errmsg.String
		out = append(out, l)
	}
	return out, rows.Err()
}

type Stats struct {
	TotalRequests   int64   `json:"totalRequests"`
	SuccessRequests int64   `json:"successRequests"`
	ErrorRequests   int64   `json:"errorRequests"`
	TotalTokens     int64   `json:"totalTokens"`
	ActiveAccounts  int     `json:"activeAccounts"`
	TotalAccounts   int     `json:"totalAccounts"`
	AvgLatencyMs    float64 `json:"avgLatencyMs"`
	P95LatencyMs    float64 `json:"p95LatencyMs"`
	EstimatedCost   float64 `json:"estimatedCost"`
	ErrorRate       float64 `json:"errorRate"`
	TodayRequests   int64   `json:"todayRequests"`
}

func (s *Store) GetStats() (*Stats, error) {
	st := &Stats{}
	row := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0), COALESCE(SUM(total_tokens),0) FROM request_logs`)
	if err := row.Scan(&st.TotalRequests, &st.SuccessRequests, &st.ErrorRequests, &st.TotalTokens); err != nil {
		return nil, err
	}
	// substr(created_at,1,19)：驱动可能写入含时区/单调时钟的长格式，date() 解析不了，截前 19 位按本地墙钟再比
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN substr(created_at,1,19)>=date('now','localtime') THEN 1 ELSE 0 END),0) FROM request_logs`).Scan(&st.TodayRequests); err != nil {
		return nil, err
	}
	st.AvgLatencyMs, st.P95LatencyMs, st.EstimatedCost, _ = s.LatencyAndCostStats()
	if st.TotalRequests > 0 {
		st.ErrorRate = float64(st.ErrorRequests) / float64(st.TotalRequests)
	}
	all, err := s.ListAccounts()
	if err != nil {
		return nil, err
	}
	st.TotalAccounts = len(all)
	for _, a := range all {
		if a.Status == "active" && a.Enabled {
			st.ActiveAccounts++
		}
	}
	return st, nil
}

// ─── 告警记录 ────────────────────────────────────────────────

// CreateAlert 写入一条告警。同 source_type+source_id+alert_type 已有未处理告警时跳过（去重）。
func (s *Store) CreateAlert(level, title, message, sourceType string, sourceID *int64, alertType string) error {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM alerts WHERE status='open' AND source_type=? AND alert_type=? AND (source_id=? OR (? IS NULL AND source_id IS NULL)) LIMIT 1`,
		sourceType, alertType, sourceID, sourceID).Scan(&id)
	if err == nil {
		return nil // 已有未处理同源告警，去重
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO alerts (level,title,message,source_type,source_id,alert_type,status,created_at) VALUES (?,?,?,?,?,?,'open',?)`,
		level, title, message, sourceType, sourceID, alertType, time.Now())
	return err
}

func (s *Store) ListAlerts(status string, limit int) ([]*Alert, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id,level,title,message,source_type,source_id,alert_type,status,created_at,resolved_at FROM alerts`
	var args []interface{}
	if status == "open" || status == "resolved" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Alert
	for rows.Next() {
		a := &Alert{}
		var srcID sql.NullInt64
		var resolved sql.NullTime
		if err := rows.Scan(&a.ID, &a.Level, &a.Title, &a.Message, &a.SourceType, &srcID, &a.AlertType, &a.Status, &a.CreatedAt, &resolved); err != nil {
			return nil, err
		}
		if srcID.Valid {
			a.SourceID = &srcID.Int64
		}
		if resolved.Valid {
			a.ResolvedAt = &resolved.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ResolveAlert(id int64) error {
	_, err := s.db.Exec(`UPDATE alerts SET status='resolved',resolved_at=? WHERE id=? AND status='open'`, time.Now(), id)
	return err
}

func (s *Store) ResolveAlertsBySource(sourceType, alertType string, sourceID int64) error {
	_, err := s.db.Exec(`UPDATE alerts SET status='resolved',resolved_at=? WHERE status='open' AND source_type=? AND alert_type=? AND source_id=?`,
		time.Now(), sourceType, alertType, sourceID)
	return err
}

// AlertSummary 返回未处理告警按级别计数 + MTTR（已解决告警的平均处理时长，分钟）。
type AlertSummary struct {
	Severe   int     `json:"severe"`
	Warning  int     `json:"warning"`
	Info     int     `json:"info"`
	Open     int     `json:"open"`
	Resolved int     `json:"resolved"`
	MTTRMin  float64 `json:"mttrMin"`
}

func (s *Store) AlertSummary() (*AlertSummary, error) {
	sum := &AlertSummary{}
	if err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN status='open' AND level='severe' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='open' AND level='warning' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='open' AND level='info' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='open' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='resolved' THEN 1 ELSE 0 END),0)
		FROM alerts`).Scan(&sum.Severe, &sum.Warning, &sum.Info, &sum.Open, &sum.Resolved); err != nil {
		return nil, err
	}
	var mttr float64
	var cnt int
	if err := s.db.QueryRow(`SELECT COALESCE(AVG((julianday(resolved_at)-julianday(created_at))*1440),0), COUNT(*) FROM alerts WHERE status='resolved' AND resolved_at IS NOT NULL`).Scan(&mttr, &cnt); err != nil {
		return nil, err
	}
	sum.MTTRMin = mttr
	return sum, nil
}

func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key,value,updated_at) VALUES (?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, time.Now())
	return err
}

func (s *Store) ListSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key,value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// QueryRowScan 单行查询便捷方法（供评估器等内部逻辑使用）。
func (s *Store) QueryRowScan(query string, dest ...interface{}) error {
	return s.db.QueryRow(query).Scan(dest...)
}

// RecordCacheProbe 记录一次可缓存请求的指纹：首见插入 hits=0，重复则 hits+1。
// hits 即「若开启响应缓存本可命中的次数」，用于零风险度量真实命中率。
func (s *Store) RecordCacheProbe(key string) error {
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO cache_probe (key,hits,first_seen,last_seen) VALUES (?,0,?,?)
ON CONFLICT(key) DO UPDATE SET hits=hits+1,last_seen=excluded.last_seen`, key, now, now)
	return err
}

// CacheProbeStats 返回去重后的请求数与可命中次数。
// 可缓存请求总数 = distinct + repeats；潜在命中率 = repeats/(distinct+repeats)。
func (s *Store) CacheProbeStats() (distinct, repeats int64, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(hits),0) FROM cache_probe`).Scan(&distinct, &repeats)
	return
}

// ResetCacheProbe 清空探针表，开始一个干净的度量窗口。
func (s *Store) ResetCacheProbe() error {
	_, err := s.db.Exec(`DELETE FROM cache_probe`)
	return err
}

func (s *Store) ResolveAllOpen() error {
	_, err := s.db.Exec(`UPDATE alerts SET status='resolved',resolved_at=? WHERE status='open'`, time.Now())
	return err
}

func (s *Store) ResolveAllByType(sourceType, alertType string) error {
	_, err := s.db.Exec(`UPDATE alerts SET status='resolved',resolved_at=? WHERE status='open' AND source_type=? AND alert_type=?`,
		time.Now(), sourceType, alertType)
	return err
}
