package store

import (
	"testing"
	"time"
)

// TestPurgeRequestLogsOlderThan 验证只保留最近 N 天：更早的行被删、边界内的行保留。
// LogRequest 固定用 time.Now() 无法造旧数据，这里用与其相同的方式（传 time.Time 参数）
// 直接写入自定义 created_at，保证存储格式与生产一致。
func TestPurgeRequestLogsOlderThan(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	now := time.Now()
	insert := func(created time.Time) {
		if _, err := s.db.Exec(`INSERT INTO request_logs (status,created_at) VALUES ('success',?)`, created); err != nil {
			t.Fatal(err)
		}
	}
	insert(now.AddDate(0, 0, -10)) // 10 天前 -> 删
	insert(now.AddDate(0, 0, -8))  // 8 天前  -> 删
	insert(now.AddDate(0, 0, -1))  // 1 天前  -> 留
	insert(now)                    // 现在    -> 留

	deleted, err := s.PurgeRequestLogsOlderThan(7)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	remaining, err := s.CountRequestLogs()
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("remaining = %d, want 2", remaining)
	}

	// days<=0 视为不清理，不应删除任何行。
	if n, err := s.PurgeRequestLogsOlderThan(0); err != nil || n != 0 {
		t.Fatalf("PurgeRequestLogsOlderThan(0) = (%d, %v), want (0, nil)", n, err)
	}
	if remaining, _ := s.CountRequestLogs(); remaining != 2 {
		t.Fatalf("remaining after no-op purge = %d, want 2", remaining)
	}
}
