package provider

import "strings"

import "testing"

// 标题生成模板是上游安全分类器的高危信号（2026-10-08 线上定位，见 neutralizeTitlePrompt
// 注释）。必须压缩为无害等价指令后再出站；user session 内容保持原样。
func TestNeutralizeTitlePrompt(t *testing.T) {
	tpl := "[System]\n" + titlePromptMarker + " so the user can pick it out.\n\nLead with the most specific thing.\n\n[User]\n<session>\n修改ddns功能\n</session>\n\nWrite the title in the predominant language."
	got := neutralizeTitlePrompt(tpl)
	if strings.Contains(got, titlePromptMarker) {
		t.Fatalf("title template marker must not survive neutralization:\n%s", got)
	}
	if !strings.Contains(got, "<session>\n修改ddns功能\n</session>") {
		t.Fatalf("user session content must be preserved verbatim:\n%s", got)
	}
	if !strings.Contains(got, `"title"`) {
		t.Fatalf("JSON output requirement must be preserved:\n%s", got)
	}
	if len([]rune(got)) > 300 {
		t.Fatalf("compressed instruction should be compact, got %d runes", len([]rune(got)))
	}

	// 非标题请求原样直通
	normal := "[System]\nYou are Claude Code.\n\n[User]\n帮我写个爬虫"
	if got := neutralizeTitlePrompt(normal); got != normal {
		t.Fatalf("non-title query must pass through unchanged, got:\n%s", got)
	}

	// 只有模板头没有 session 段的畸形输入：原样返回（不误伤）
	broken := "[System]\n" + titlePromptMarker + " no session here"
	if got := neutralizeTitlePrompt(broken); got != broken {
		t.Fatalf("malformed title query must pass through, got:\n%s", got)
	}
}

// git 署名 reminder 块是 FREE 号上的确定性 flag 信号（2026-10-08 探针验证），
// 出站前必须整块剔除；session/任务内容不受影响。
func TestStripAttributionReminder(t *testing.T) {
	q := "before\n<system-reminder>\nCodebase instructions here.\n</system-reminder>\n<mid/>\n<system-reminder>\n" + attributionMarker + "\n- End git commit messages with:\nCo-Authored-By: x\n</system-reminder>\n<task>修改ddns</task>"
	got := stripAttributionReminder(q)
	if strings.Contains(got, attributionMarker) || strings.Contains(got, "Co-Authored-By") {
		t.Fatalf("attribution block must be removed:\n%s", got)
	}
	for _, keep := range []string{"before", "Codebase instructions here.", "<mid/>", "<task>修改ddns</task>"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("must keep %q, got:\n%s", keep, got)
		}
	}

	// 无特征原样直通
	if got := stripAttributionReminder("plain query"); got != "plain query" {
		t.Fatalf("plain query must pass through: %q", got)
	}
	// 只有特征没有完整包裹：原样直通（不误伤）
	partial := "x " + attributionMarker + " y"
	if got := stripAttributionReminder(partial); got != partial {
		t.Fatalf("unwrapped marker must pass through: %q", got)
	}
}
