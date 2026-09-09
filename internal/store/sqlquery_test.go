package store

import (
	"path/filepath"
	"testing"
)

// TestRunReadOnlyQueryGuard 钉住只读守卫：写语句/PRAGMA/注释伪装/多语句一律拒绝，
// SELECT（含注释前缀、尾分号）放行，行数上限触发 truncated。
func TestRunReadOnlyQueryGuard(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sql_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, q := range []string{
		"UPDATE accounts SET enabled=1",
		"DELETE FROM accounts",
		"INSERT INTO settings VALUES ('k','v')",
		"DROP TABLE accounts",
		"PRAGMA journal_mode=DELETE",
		"/* 看起来像注释 */ DROP TABLE accounts",
		"-- note\nDELETE FROM accounts",
		"SELECT 1; DROP TABLE accounts",
	} {
		if _, _, _, err := s.RunReadOnlyQuery(q, 10); err == nil {
			t.Fatalf("非只读语句应被拒绝: %s", q)
		}
	}

	// 注释前缀 + 尾分号的 SELECT 应放行。
	cols, rows, _, err := s.RunReadOnlyQuery("-- note\nSELECT 403 AS code;", 10)
	if err != nil {
		t.Fatalf("只读查询被误拒: %v", err)
	}
	if len(cols) != 1 || len(rows) != 1 || rows[0]["code"] != int64(403) {
		t.Fatalf("查询结果不符: cols=%v rows=%v", cols, rows)
	}

	// 行数上限：3 行结果限 2 行 → truncated=true 且只返回 2 行。
	_, rows, truncated, err := s.RunReadOnlyQuery("SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3", 2)
	if err != nil || !truncated || len(rows) != 2 {
		t.Fatalf("行数上限未生效: rows=%d truncated=%v err=%v", len(rows), truncated, err)
	}
}
