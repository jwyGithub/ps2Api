package router

import (
	"testing"
	"time"
)

// ponytail: 只测调度核心（nextDaily 的边界），探测逻辑已被线上按钮路径覆盖
func TestNextDaily(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		// 22:59:59 -> 当天 23:00
		{time.Date(2026, 9, 14, 22, 59, 59, 0, loc), time.Date(2026, 9, 14, 23, 0, 0, 0, loc)},
		// 恰好 23:00:00 -> 明天 23:00（不立即触发）
		{time.Date(2026, 9, 14, 23, 0, 0, 0, loc), time.Date(2026, 9, 15, 23, 0, 0, 0, loc)},
		// 23:00:01 -> 明天 23:00
		{time.Date(2026, 9, 14, 23, 0, 1, 0, loc), time.Date(2026, 9, 15, 23, 0, 0, 0, loc)},
		// 凌晨 -> 当天 23:00
		{time.Date(2026, 9, 14, 0, 5, 0, 0, loc), time.Date(2026, 9, 14, 23, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		if got := nextDaily(c.now, 23); !got.Equal(c.want) {
			t.Errorf("nextDaily(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}
