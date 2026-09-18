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

// TestStripSystemReminders: 剥离 <system-reminder> 注入块（跨行、可多块），
// 供 [User (task)] 渲染用——CLAUDE.md/gitStatus 等注入块不吃 2000 rune 任务预算。
func TestStripSystemReminders(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no block", "普通任务文本", "普通任务文本"},
		{"single block", "前文\n<system-reminder>\nCLAUDE.md 内容\n多行\n</system-reminder>\n真实任务", "前文\n\n真实任务"},
		{"multi block", "<system-reminder>\nA\n</system-reminder>\n中段\n<system-reminder>\nB\n</system-reminder>\n尾", "\n中段\n\n尾"},
		{"unclosed block kept", "文本 <system-reminder>\n未闭合保留原样", "文本 <system-reminder>\n未闭合保留原样"},
	}
	for _, c := range cases {
		if got := stripSystemReminders(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestFoldedTaskBlockStripsSystemReminders: 折叠路径 [User (task)] 渲染前剥离
// 注入块——长 CLAUDE.md 挤占预算、任务句掉进中段省略（2026-09-18 事故）。
func TestFoldedTaskBlockStripsSystemReminders(t *testing.T) {
	p := New()
	reminders := "<system-reminder>\n" + strings.Repeat("注入内容。", 800) + "\n</system-reminder>\n"
	msgs := []ChatMessage{mustMsg(t, "user", reminders+"我需要你修改 database-v2.html 的接口调用方式")}
	for i := 0; i < 3; i++ {
		msgs = append(msgs, *assistantFollowup(&Result{Content: "历史回复"}))
		msgs = append(msgs, mustMsg(t, "user", "跟进"))
	}
	msgs = append(msgs, mustMsg(t, "user", "最新一轮"))

	split := p.splitMessagesSeed(msgs, "", false, false)
	if !strings.Contains(split.Query, "我需要你修改 database-v2.html 的接口调用方式") {
		t.Fatal("task sentence must survive system-reminder stripping in folded task block")
	}
	if strings.Contains(split.Query, "注入内容") {
		t.Fatal("system-reminder body must be stripped from folded task block")
	}
}

// TestFoldedSectionOrderUnchanged: sections 化改造后折叠产物与原拼接顺序一致：
// skills 最前（普通续聊无 skills 时 task 最前）、context 居中、tail 最后。
// 本测试钉住拼接顺序，防止 sections 化时悄悄换位。
func TestFoldedSectionOrderUnchanged(t *testing.T) {
	p := New()
	msgs := []ChatMessage{mustMsg(t, "user", "原始任务AAA")}
	msgs = append(msgs, *assistantFollowup(&Result{Content: "中间回复BBB"}))
	msgs = append(msgs, mustMsg(t, "user", "最新一轮CCC"))

	split := p.splitMessagesSeed(msgs, "", false, false)
	q := split.Query
	iTask := strings.Index(q, "原始任务AAA")
	iHist := strings.Index(q, "中间回复BBB")
	iTail := strings.Index(q, "最新一轮CCC")
	if iTask < 0 || iHist < 0 || iTail < 0 {
		t.Fatalf("all sections must be present: %d/%d/%d", iTask, iHist, iTail)
	}
	if !(iTask < iHist && iHist < iTail) {
		t.Fatalf("order must be task < history < tail: %d/%d/%d", iTask, iHist, iTail)
	}
}

// TestCapUpstreamQuerySections: 超限时按权重整段丢弃。
func TestCapUpstreamQuerySections(t *testing.T) {
	mk := func(n, weight int, text string) []querySection {
		out := make([]querySection, n)
		for i := range out {
			out[i] = querySection{Text: text, Weight: weight}
		}
		return out
	}
	// 1) 不超限直通。
	small := []querySection{
		{Text: "skills", Weight: 3},
		{Text: "task", Weight: 3},
		{Text: strings.Repeat("a", 100), Weight: 1},
		{Text: "tail", Weight: 3},
	}
	if got := capUpstreamQuerySections(small); got != "skills\n\ntask\n\n"+strings.Repeat("a", 100)+"\n\ntail" {
		t.Fatalf("under limit must pass through verbatim, got %q", got)
	}
	// 2) 超限：低段从最旧开始丢，高段全存。
	old := strings.Repeat("旧", 2000)
	older := strings.Repeat("更旧", 3000) // 6000 runes: pushes joined over the 9900 limit so drop logic engages
	mid := strings.Repeat("中", 2000)
	over := []querySection{
		{Text: "SKILLS", Weight: 3},
		{Text: older, Weight: 1},
		{Text: old, Weight: 1},
		{Text: mid, Weight: 2},
		{Text: "TASK", Weight: 3},
		{Text: "TAIL", Weight: 3},
	}
	got := capUpstreamQuerySections(over)
	for _, keep := range []string{"SKILLS", "TASK", "TAIL", "中"} {
		if !strings.Contains(got, keep) {
			t.Errorf("weight-3/2 section must survive: %q", keep)
		}
	}
	if strings.Contains(got, "更旧") {
		t.Error("oldest low-weight section must be dropped first")
	}
	if !strings.Contains(got, "...[omitted:") {
		t.Errorf("omission marker with count required, got %q", got)
	}
	// 3) 全低段超限：丢到剩最新一条低段 + 标记。
	lows := mk(8, 1, strings.Repeat("x", 2000))
	got = capUpstreamQuerySections(lows)
	if !strings.Contains(got, "...[omitted:") {
		t.Error("marker required when dropping low sections")
	}
	if got := len([]rune(got)); got > 9900 {
		t.Errorf("result must stay under limit, got %d runes", got)
	}
}

// TestFoldedProbeQueryUncapped: 探针请求（WafProbe）折叠产物不经 cap/剥离/
// 权重丢弃——逐字复现可疑内容是探针的存在意义（见 ChatRequest.WafProbe 注释）。
// 出站旁路发生在 request.go（query = upstreamQuery 原样），本测试钉住
// splitMessagesSeed 层面探针 tail 不加 [User] 前缀的既有契约不因 sections 化回归。
//
// 折叠路径（convID==""）把任务段/历史段渲染在逐字 tail 之前，故断言钉住 tail 本身：
// 整条折叠 query 以 probe 原文结尾（probe 以 PROBE_MARKER 开头 → tail 逐字、未被
// cap/中和），且 tail 不带 [User] 角色前缀。断言针对 tail 而非整条 query 的前缀。
func TestFoldedProbeQueryUncapped(t *testing.T) {
	p := New()
	probe := "PROBE_MARKER 精确字节形态\n\n<script>alert(1)</script>"
	msgs := []ChatMessage{mustMsg(t, "user", "原始任务")}
	msgs = append(msgs, *assistantFollowup(&Result{Content: "回复"}))
	msgs = append(msgs, mustMsg(t, "user", probe))

	// splitMessagesSeed(messages, convID, wafProbe, contextSeed)：第三个 bool 为
	// wafProbe，置 true 触发探针路径。
	split := p.splitMessagesSeed(msgs, "", true, false)
	if !strings.HasSuffix(split.Query, probe) {
		suffix := split.Query
		if len(suffix) > 60 {
			suffix = suffix[len(suffix)-60:]
		}
		t.Fatalf("probe tail must be verbatim at query end, got suffix %q", suffix)
	}
	if strings.Contains(split.Query, "[User]\nPROBE_MARKER") {
		t.Fatal("probe tail must not carry [User] prefix")
	}
}
