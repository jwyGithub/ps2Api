package store

import (
	"path/filepath"
	"testing"
)

// TestRunSQLGuard 钉住 2026-09-23 新契约：面板诊断控制台接受任意 SQL
// （写语句/DDL/PRAGMA 放行——原只读前缀守卫被有意移除），多语句仍一律拒绝。
// SELECT（含注释前缀、尾分号）照常工作，行数上限触发 truncated。
func TestRunSQLGuard(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sql_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 多语句一律拒绝（注释伪装首词 strip 后首词即写动词，非 SELECT 时 Query 报错）。
	for _, q := range []string{
		"SELECT 1; DROP TABLE accounts",
		"-- note\nSELECT 1; DELETE FROM accounts",
		"/* 看起来像注释 */ SELECT 1; DROP TABLE accounts",
	} {
		if _, _, _, err := s.RunSQL(q, 10); err == nil {
			t.Fatalf("多语句应被拒绝: %s", q)
		}
	}

	// 写语句放行并生效。
	if _, _, _, err := s.RunSQL("UPDATE settings SET value='1' WHERE key='nope'", 10); err != nil {
		t.Fatalf("写语句应被接受: %v", err)
	}
	if _, _, _, err := s.RunSQL("CREATE TABLE guard_probe (k TEXT)", 10); err != nil {
		t.Fatalf("DDL 应被接受: %v", err)
	}

	// 注释前缀 + 尾分号的 SELECT 应放行。
	cols, rows, _, err := s.RunSQL("-- note\nSELECT 403 AS code;", 10)
	if err != nil {
		t.Fatalf("只读查询被误拒: %v", err)
	}
	if len(cols) != 1 || len(rows) != 1 || rows[0]["code"] != int64(403) {
		t.Fatalf("查询结果不符: cols=%v rows=%v", cols, rows)
	}

	// 行数上限：3 行结果限 2 行 → truncated=true 且只返回 2 行。
	_, rows, truncated, err := s.RunSQL("SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3", 2)
	if err != nil || !truncated || len(rows) != 2 {
		t.Fatalf("行数上限未生效: rows=%d truncated=%v err=%v", len(rows), truncated, err)
	}
}
