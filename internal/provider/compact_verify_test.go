package provider

import (
	"os"
	"strings"
	"testing"
)

// 用真实 flagged 出站 query（2026-11-09 案例 log 284，本地文件 _q284.txt）验证压缩效果。
// 文件缺失时跳过（CI 无该文件）。
func TestCompactSkillsDirectoryRealData(t *testing.T) {
	data, err := os.ReadFile(`C:\Users\sandbox\Desktop\postman-agent\_q284.txt`)
	if err != nil {
		t.Skip("real-data file not present")
	}
	q := string(data)
	if !strings.Contains(q, skillsDirectoryMarker) {
		t.Skip("query has no skills directory")
	}
	got := compactSkillsDirectory(q)
	origN := len([]rune(q))
	newN := len([]rune(got))
	t.Logf("runes %d -> %d (%.0f%%)", origN, newN, float64(newN)/float64(origN)*100)
	if strings.Contains(got, skillsHeaderMarker) {
		t.Fatal("instruction line must be gone")
	}
	if strings.Contains(got, "canvas-design: Create beautiful") {
		t.Fatal("descriptions must be stripped")
	}
	if !strings.Contains(got, "- canvas-design") {
		t.Fatal("skill names must be preserved")
	}
	if !strings.Contains(got, "[User Message]") {
		t.Fatal("tail user messages must be preserved")
	}
}
