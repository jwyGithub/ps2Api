package provider

import (
	"fmt"
	"strings"
	"testing"
)

// 构造一条模拟 Claude Code SessionStart hook 的 system 消息：
// 前置散文 + 大段 skills 清单（- name: 描述 一条一行）+ 尾部杂项 + 杂散 kebab 单行（不该被吸入）。
func skillsSystemMsg(t *testing.T) ChatMessage {
	t.Helper()
	var b strings.Builder
	b.WriteString("SessionStart hook: ponytail active.\n\n")
	b.WriteString("# Intro\nYou have superpowers. Invoke skills via the Skill tool.\n\n")
	// 杂散 kebab 单行（不足 4 行的块）——不得进清单
	b.WriteString("Settings: `- full: \"the ladder enforced\"` applies here.\n")
	b.WriteString("- claude: Catch-all agent for tasks.\n\n")
	// 89 条 skills 清单：描述长度贴近真实（首句 40-70 字符）
	for i := 0; i < 89; i++ {
		fmt.Fprintf(&b, "- skill-%02d: Use this skill when doing task %d. It has extra detail that goes on longer than the first sentence would carry.\n", i, i)
	}
	b.WriteString("\n<total_tokens>15000000 tokens left</total_tokens>\n")
	return mustMsg(t, "system", b.String())
}

// TestFoldedSystemKeepsSkillList 钉住：巨型 system（含 skills 清单）折叠后，
// 清单条目的「名字」必须几乎全数存活（预算内带短描述、超预算降级名字-only），
// 不能像普通散文一样被中段省略。没有名单，模型在整个会话里都不知道有哪些
// skill 可调（2026-09-17 线上：经网关的模型从不调用 skills，直连正常）。
// 同时钉住清单段置 query 最前（capUpstreamQuery 头部保留区）与杂散 kebab 行排除。
func TestFoldedSystemKeepsSkillList(t *testing.T) {
	p := New()
	sys := skillsSystemMsg(t)
	body := p.buildBody(&ChatRequest{Messages: []ChatMessage{
		sys,
		mustMsg(t, "user", "hello"),
	}}, &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "w", WorkspaceSubdomain: "sub"}, "test", 1)
	query := body["input"].(map[string]interface{})["query"].(string)
	// 名单段必须置最前（cap 头部保留区）
	if !strings.HasPrefix(query, "[System skills]") {
		t.Fatalf("skills section must be at query head, got head: %q", query[:80])
	}
	// 89 条名字应全数存活（描述超预算的降级为名字-only）
	found := strings.Count(query, "- skill-")
	if found < 85 {
		t.Fatalf("skill names must survive folding (with name-only degradation), got %d/89 in %d runes", found, len([]rune(query)))
	}
	// 长描述必须被压缩（截到首句或名字-only），不得原样全带
	if strings.Contains(query, "extra detail that goes on longer than the first sentence would carry.") {
		t.Fatalf("long descriptions should be compacted")
	}
	// 杂散 kebab 单行不得吸进清单段（它们仍会出现在 [System] 散文段，但不在 skills 段内）
	secEnd := strings.Index(query, "[System]")
	sec := query
	if secEnd > 0 {
		sec = query[:secEnd]
	}
	if strings.Contains(sec, "- full:") || strings.Contains(sec, "- claude: Catch-all") {
		t.Fatalf("scattered kebab lines must not be absorbed into the skill list section: %q", sec[:200])
	}
	// 总长受上游上限约束
	if len([]rune(query)) > MaxUpstreamQueryRunes {
		t.Fatalf("query exceeds upstream limit: %d", len([]rune(query)))
	}
}
