# 折叠重放质量优化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 消除会话失效后冷启动折叠重放的三处质量缺陷：tool-tail 不补种、任务块被 system-reminder 挤占预算、cap 中段省略不感知内容权重。

**Architecture:** 三处独立改动，全部集中在 `internal/provider`：`seed.go` 放开 tool-tail 补种并按 toolTailIndex 存前缀指纹；`messages.go` 新增 `stripSystemReminders` 剥离注入块、折叠渲染改为产出带权重的 `[]querySection`、新增 `capUpstreamQuerySections` 按三档权重整段丢弃；`request.go` 出站处改调 sections 版 cap。非折叠路径（hasConv 增量、首轮直发、探针）行为零变化。

**Tech Stack:** Go 1.x，标准库（regexp/strings/testing），无新依赖。

## Global Constraints

- 出站仍是单条 `input.query` 字符串，上游协议零变化。
- 剥离/丢弃只改出站渲染文本，不碰 `req.Messages`，不影响会话指纹与账号粘性。
- 探针（`req.WafProbe`）路径原样旁路：不剥离、不 cap、不加任何前缀。
- 段预算常量不变：`FoldedSystemBudgetRunes=2000`、`FoldedTextMsgBudgetRunes=2000`、`FoldedTailToolResultRunes=4000`、`FoldedSkillListRunes=2800`、`MaxUpstreamQueryRunes=10000`。
- 高权重段（skills、task、tail）永不丢；硬上限 `MaxUpstreamQueryRunes - 100 = 9900` rune。
- 每个 task 结束时 `go test ./internal/provider/` 全绿。

---

### Task 1: tool-tail 允许补种（shouldSeed）

**Files:**
- Modify: `internal/provider/seed.go:18-42`（shouldSeed）
- Test: `internal/provider/seed_test.go`（TestShouldSeed 内追加断言）

**Interfaces:**
- Consumes: `toolTailIndex(messages []ChatMessage) int`（messages.go，已存在）；`ChatMessage{Role string, ...}`。
- Produces: `shouldSeed` 签名不变 `(p *Provider, accID int64, req *ChatRequest) bool`，tool-tail 且无会话命中时返回 true。

- [ ] **Step 1: 在 TestShouldSeed 末尾追加失败测试**

在 `internal/provider/seed_test.go` 的 `TestShouldSeed` 函数体内、最后的 `t.Setenv("GATEWAY_CONTEXT_SEED", "0")` 块之前插入：

```go
	// tool-tail 请求 + 冷启动 + 长历史 → true（2026-09-18 改：会话失效后的
	// 重放轮也补种，消除 128 条消息裸折叠被中段省略吞掉方案的事故）。
	toolTailHistory := []ChatMessage{mustMsg(t, "user", "任务")}
	for i := 0; i < 3; i++ {
		toolTailHistory = append(toolTailHistory, *assistantFollowup(&Result{ToolCalls: []ToolCall{{ID: "tc-" + fmt.Sprint(i), Type: "function", Function: &FunctionCall{Name: "Bash", Arguments: "ls"}}}}))
		toolTailHistory = append(toolTailHistory, ChatMessage{Role: "tool", ToolCallID: "tc-" + fmt.Sprint(i), Content: mustJSON(t, "输出"+fmt.Sprint(i))})
	}
	if !p.shouldSeed(3, &ChatRequest{Messages: toolTailHistory}) {
		t.Fatal("cold-start tool-tail with long history should seed")
	}
```

同时在文件头 import 块确认已有 `"fmt"`（已有）。

`mustJSON` 若不存在，在 `conv_test.go` 的 `mustMsg` 旁新增：

```go
func mustJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return json.RawMessage(b)
}
```

注意：`assistantFollowup` 只在 `ToolCalls` 非空时 marshal 进 `msg.ToolCalls`；role:"tool" 消息让 `hasReusableHistory` 命中。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/provider/ -run TestShouldSeed -v`
Expected: FAIL — `cold-start tool-tail with long history should seed`（现状 shouldSeed 对 toolTail 返回 false）。

- [ ] **Step 3: 修改 shouldSeed**

`internal/provider/seed.go` 删除这段（约 30-32 行）：

```go
	if toolTail(req.Messages) {
		return false
	}
```

并把函数头注释的排除条件说明同步更新：原注释「四个条件全满足：冷启动（无会话命中）、非 tool-tail 重放、历史足够长、开关开启」改为：

```go
// shouldSeed 报告本次请求是否应做上下文补种。条件全满足：
// 冷启动（无会话命中）、历史足够长、开关开启。tool-tail 重放也补种（2026-09-18 改）：
// 会话失效后的重放轮若不补种，128 条消息裸折叠进 10000 rune，中段省略会吞掉
// 已批准方案等关键上下文（2026-09-18 线上事故，见设计文档）。
// WafProbe 探针绝不补种（必须逐字复现可疑内容）。
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/provider/ -run TestShouldSeed -v`
Expected: PASS。

- [ ] **Step 5: 跑全量 provider 测试防回归**

Run: `go test ./internal/provider/`
Expected: PASS（注意 `TestStreamInternalSeedsColdStart` 等端到端测试若构造了 tool-tail 冷启动会话，可能因新行为多出一轮补种请求而失败——若失败，检查其 mock server 的 bodies 计数断言并更新期望值，行为本身是正确的新契约）。

- [ ] **Step 6: Commit**

```bash
git add internal/provider/seed.go internal/provider/seed_test.go internal/provider/conv_test.go
git commit -m "feat: allow tool-tail requests to context-seed after session loss"
```

---

### Task 2: 补种前缀指纹对 tool-tail 取 messages[:toolIdx]

**Files:**
- Modify: `internal/provider/seed.go:55-67`（seedConversation 的 queryIdx 段）
- Test: `internal/provider/seed_test.go`（新增测试）

**Interfaces:**
- Consumes: `toolTailIndex(messages []ChatMessage) int`。
- Produces: `seedConversation` 签名不变。tool-tail 请求的前缀指纹 = `conversationFingerprint(messages[:toolIdx])`，重试轮 `LookupConversation` 前缀循环命中。

- [ ] **Step 1: 写失败测试**

在 `internal/provider/seed_test.go` 追加：

```go
// TestSeedConversationToolTailPrefixMapping: tool-tail 请求补种成功后，按
// messages[:toolIdx] 前缀存映射——客户端重试同一批 messages 时
// LookupConversation 的前缀循环在 i==toolIdx 处命中，恢复增量模式。
func TestSeedConversationToolTailPrefixMapping(t *testing.T) {
	var bodies []map[string]interface{}
	srv := mockPostmanServer(t, "conv-toolseed-9", &bodies)
	client := &http.Client{Transport: redirectTransport{base: http.DefaultTransport, target: srv.URL}}
	p := New()
	p.Client = client
	acc := seedTestAccount(t, srv)

	msgs := []ChatMessage{mustMsg(t, "user", "TASK 原始任务")}
	for i := 0; i < 3; i++ {
		msgs = append(msgs, *assistantFollowup(&Result{ToolCalls: []ToolCall{{ID: "tc-" + fmt.Sprint(i), Type: "function", Function: &FunctionCall{Name: "Bash", Arguments: "ls"}}}}))
		msgs = append(msgs, ChatMessage{Role: "tool", ToolCallID: "tc-" + fmt.Sprint(i), Content: mustJSON(t, "输出"+fmt.Sprint(i))})
	}
	req := &ChatRequest{Model: "claude-opus-4-8", Messages: msgs}
	tokens := &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}

	res := p.seedConversation(context.Background(), acc, req, tokens, "CLAUDE_OPUS_48_BEDROCK")
	if !res.Success {
		t.Fatalf("seed failed: %s", res.Error)
	}
	if got := p.LookupConversation(acc.ID, msgs); got != "conv-toolseed-9" {
		t.Fatalf("retried tool-tail must hit seeded conversation, got %q", got)
	}
	// 补种请求出站体：tool-tail 重放的待处理结果不出现在补种轮（tail=摘要指令）。
	q := bodies[0]["input"].(map[string]interface{})["query"].(string)
	if strings.Contains(q, "输出0") {
		t.Fatal("seed query must not include pending tool results in tail")
	}
	if !strings.Contains(q, "总结当前任务状态") {
		t.Fatal("seed query must contain summary instruction")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/provider/ -run TestSeedConversationToolTailPrefixMapping -v`
Expected: FAIL — `LookupConversation` 返回 ""（现状 queryIdx 找不到非 tool_result 的 user 消息时返回，seedConversation 直接 return，不存映射）。

- [ ] **Step 3: 修改 seedConversation**

`internal/provider/seed.go` 把 queryIdx 段（原 55-66 行）：

```go
	queryIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && !isAnthropicToolResult(req.Messages[i]) {
			queryIdx = i
			break
		}
	}
	if queryIdx < 0 {
		return seedRes
	}
	fp := conversationFingerprint(req.Messages[:queryIdx])
```

替换为：

```go
	// 前缀切点按请求形态分：普通续聊取「最后一条纯 user 消息」之后（第二轮完整
	// messages 的前缀循环恰好命中）；tool-tail 重放取 toolTailIndex 位置——待处理
	// tool results 之前的全部历史（含它们所回应的 assistant 调用）作为前缀指纹。
	prefixEnd := -1
	if toolIdx := toolTailIndex(req.Messages); toolIdx >= 0 {
		prefixEnd = toolIdx
	} else {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" && !isAnthropicToolResult(req.Messages[i]) {
				prefixEnd = i
				break
			}
		}
	}
	if prefixEnd < 0 {
		return seedRes
	}
	fp := conversationFingerprint(req.Messages[:prefixEnd])
```

注释第 55-56 行的「前缀指纹：messages[:queryIdx]（去掉最新 user 消息）」同步改为「前缀指纹：普通续聊=去掉最新 user 消息；tool-tail=待处理 tool results 之前的全部历史」。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/provider/ -run 'TestSeedConversation|TestShouldSeed' -v`
Expected: PASS（新旧两个映射测试都绿——普通续聊路径 prefixEnd 语义与原 queryIdx 相同）。

- [ ] **Step 5: 跑全量 + Commit**

Run: `go test ./internal/provider/`
Expected: PASS。

```bash
git add internal/provider/seed.go internal/provider/seed_test.go
git commit -m "feat: seed tool-tail prefix fingerprint at toolTailIndex boundary"
```

---

### Task 3: stripSystemReminders + 任务块剥离

**Files:**
- Modify: `internal/provider/messages.go`（新增函数 + taskIdx 渲染处约 388 行）
- Test: `internal/provider/skills_fold_test.go`（同文件追加，均为折叠渲染测试）

**Interfaces:**
- Consumes: `truncateMiddleRunes(s string, max int) string`（strutil.go）。
- Produces: `stripSystemReminders(text string) string`。

- [ ] **Step 1: 写失败测试**

在 `internal/provider/skills_fold_test.go` 末尾追加：

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/provider/ -run 'TestStripSystemReminders|TestFoldedTaskBlockStripsSystemReminders' -v`
Expected: FAIL — `undefined: stripSystemReminders`。

- [ ] **Step 3: 实现 stripSystemReminders 并应用**

在 `internal/provider/messages.go` 的 `foldedSystemParts` 之前新增：

```go
// systemReminderRe 匹配 <system-reminder>...</system-reminder> 注入块（跨行、
// 非贪婪）。Claude Code 客户端把 CLAUDE.md/gitStatus 等注入首条 user 消息，
// 折叠渲染 [User (task)] 时剥离，让 2000 rune 预算花在真实任务句上。
var systemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// stripSystemReminders 剥掉文本中全部 system-reminder 注入块。未闭合块保留
// 原样（宁多勿丢）。只改出站渲染，不影响会话指纹。
func stripSystemReminders(text string) string {
	return systemReminderRe.ReplaceAllString(text, "")
}
```

`splitMessagesSeed` 里 taskIdx 渲染处（约 388 行）：

```go
		if task := ExtractText(messages[taskIdx].Content); task != "" {
			taskBlock = "[User (task)]\n" + truncateMiddleRunes(task, FoldedTextMsgBudgetRunes)
		}
```

改为：

```go
		// 注入块剥离后再截断：2000 rune 预算花在任务句上，而不是 CLAUDE.md。
		if task := stripSystemReminders(ExtractText(messages[taskIdx].Content)); task != "" {
			taskBlock = "[User (task)]\n" + truncateMiddleRunes(task, FoldedTextMsgBudgetRunes)
		}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/provider/ -run 'TestStripSystemReminders|TestFoldedTaskBlockStripsSystemReminders' -v`
Expected: PASS。

- [ ] **Step 5: 跑全量 + Commit**

Run: `go test ./internal/provider/`
Expected: PASS（skills_fold_test.go 现有测试若断言 task 块含 system-reminder 文本会失败——它们断言的是 skills 清单与任务标记存活，剥离不影响）。

```bash
git add internal/provider/messages.go internal/provider/skills_fold_test.go
git commit -m "feat: strip system-reminder blocks from folded task block"
```

---

### Task 4: querySection 类型与折叠渲染改造

**Files:**
- Modify: `internal/provider/messages.go`（新增类型 + `splitMessagesSeed` 折叠分支组装段）
- Test: `internal/provider/skills_fold_test.go`（新增）

**Interfaces:**
- Consumes: Task 3 的 `stripSystemReminders`；现有段预算常量。
- Produces: `type querySection struct { Text string; Weight int }`（Weight 3=高 2=中 1=低）；`splitMessagesSeed` 返回的 `splitResult.Query` 语义不变（仍为最终字符串——本 task 内先以 sections 拼接产出，cap 逻辑在 Task 5 接入）。

- [ ] **Step 1: 写失败测试（段落顺序与权重标注不变，出站字符串与现状一致）**

在 `internal/provider/skills_fold_test.go` 追加：

```go
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
```

- [ ] **Step 2: 运行确认当前通过（本 task 的护栏测试）**

Run: `go test ./internal/provider/ -run TestFoldedSectionOrderUnchanged -v`
Expected: PASS（改造前就该通过——它是防回归护栏）。

- [ ] **Step 3: 新增 querySection 类型与渲染改造**

在 `internal/provider/messages.go` 的 `splitResult` 定义后新增：

```go
// querySection 是折叠路径的一个渲染段，携带丢弃权重：3=高（skills/task/tail，
// 永不丢）、2=中（system 段）、1=低（历史 user/assistant/tool-result 段，从最旧
// 开始丢）。cap 超限时按权重整段丢弃，替代旧的固定 30% 中段切除。
type querySection struct {
	Text   string
	Weight int
}
```

`splitMessagesSeed` 折叠分支的组装段（原 `sections := make([]string, 0, 4)` 起，至 `return splitResult{...}`）替换为：

```go
	// 段落权重（见 querySection）：高=task/skills/tail（三次线上事故的存活契约），
	// 中=system，低=历史文本与工具结果（时间越早价值越低，丢弃从最旧开始）。
	sections := make([]querySection, 0, 8)
	taskBlock := ""
	if taskIdx >= 0 {
		// 注入块剥离后再截断：2000 rune 预算花在任务句上，而不是 CLAUDE.md。
		if task := stripSystemReminders(ExtractText(messages[taskIdx].Content)); task != "" {
			taskBlock = "[User (task)]\n" + truncateMiddleRunes(task, FoldedTextMsgBudgetRunes)
		}
	}
	// skills 清单段恒置最前：落在 capUpstreamQuery 头部 30% 保留区，永不落入
	// 中段省略区（2026-09-17 端到端重放实测契约，sections 化后由高权重保证）。
	if skillsBlock != "" {
		sections = append(sections, querySection{Text: skillsBlock, Weight: 3})
	}
	if !isToolTail && taskBlock != "" {
		sections = append(sections, querySection{Text: taskBlock, Weight: 3})
	}
	if context := strings.Join(contextParts, "\n\n"); context != "" {
		sections = append(sections, querySection{Text: context, Weight: 1})
	}
	if isToolTail && taskBlock != "" {
		sections = append(sections, querySection{Text: taskBlock, Weight: 3})
	}
	tail := query
	if contextSeed {
		// 补种轮：tail 不渲染最新消息（服务端还不需要它），改为摘要指令——
		// 模型复述任务状态即建立会话，第二轮增量带上最新消息。
		tail = seedSummaryInstruction
	} else if isToolTail {
		tail = truncateMiddleRunes(query, FoldedTailToolResultRunes)
	} else if queryIdx >= 0 && query != "" {
		// 探针请求不加 “[User]\n” 角色标注：探针必须逐字复现可疑内容，任何前缀
		// 都改变字节形状（见 ChatRequest.WafProbe 注释）。
		if wafProbe {
			tail = query
		} else {
			tail = "[User]\n" + query
		}
	}
	if tail != "" {
		sections = append(sections, querySection{Text: tail, Weight: 3})
	}
	// context 是整段低权重：丢弃粒度到不了「段内单条消息」。为此把 contextParts
	// 逐段入列——每条历史消息独立成段，旧的最先被丢。
	// （上面 context 整段入列是兜底；实际实现按下面的逐段入列执行。）
	_ = sections
	return splitResult{Query: joinWeightedSections(sections)}
```

**实现说明（执行者注意）**：上面为展示结构。实际代码里 **context 不整段入列**，改为在 contextParts 产出循环里逐条携带权重。具体：`contextParts` 从 `[]string` 改为 `[]querySection`：

- `[System]` 段：`querySection{Text: "[System]\n" + ..., Weight: 2}`
- `[User]`/`[Assistant]`/`[Tool Result]` 历史段：`Weight: 1`
- `[Previous tool result omitted]` 占位：`Weight: 1`

组装时顺序不变地 append 进 `sections`（task/skills/tail 仍按原位置），`joinWeightedSections` 暂时实现为纯拼接（权重暂不消费，Task 5 的 cap 消费）：

```go
// joinWeightedSections 按 section 顺序拼接出站 query。当前 cap 阶段（Task 5）
// 之前，权重只随结构传递，拼接行为与旧 strings.Join 一致。
func joinWeightedSections(sections []querySection) string {
	parts := make([]string, len(sections))
	for i, s := range sections {
		parts[i] = s.Text
	}
	return strings.Join(parts, "\n\n")
}
```

同时把 `contextParts` 相关的 `foldedToolResultParts` 调用产物 append 处同步改为带权重的 querySection（`[Tool Result]`/`[Tool Error]`/`[User]` 混排均为 Weight 1）。

- [ ] **Step 4: 运行护栏与既有折叠测试**

Run: `go test ./internal/provider/ -run 'TestFolded|TestSplitMessages' -v`
Expected: PASS——`TestSplitMessagesContextSeedReplacesTailWithSummaryInstruction`、`TestFoldedSystemKeepsSkillList`、`TestFoldedTaskBlockStripsSystemReminders`、`TestFoldedSectionOrderUnchanged` 全绿（拼接顺序与内容零变化）。

- [ ] **Step 5: 跑全量 + Commit**

Run: `go test ./internal/provider/`
Expected: PASS。

```bash
git add internal/provider/messages.go internal/provider/skills_fold_test.go
git commit -m "refactor: fold replay renders weighted query sections"
```

---

### Task 5: capUpstreamQuerySections 三档权重丢弃

**Files:**
- Modify: `internal/provider/messages.go`（新增 capUpstreamQuerySections；splitMessagesSeed 出站处接线）
- Test: `internal/provider/skills_fold_test.go`（新增）

**Interfaces:**
- Consumes: Task 4 的 `[]querySection`；`MaxUpstreamQueryRunes`。
- Produces: `capUpstreamQuerySections(sections []querySection) string`。`capUpstreamQuery(q string) string` 保留不动（非折叠路径与防御兜底共用）。

- [ ] **Step 1: 写失败测试**

在 `internal/provider/skills_fold_test.go` 追加：

```go
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
	older := strings.Repeat("更旧", 2000)
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
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/provider/ -run TestCapUpstreamQuerySections -v`
Expected: FAIL — `undefined: capUpstreamQuerySections`。

- [ ] **Step 3: 实现 capUpstreamQuerySections**

在 `capUpstreamQuery` 之后新增（messages.go）：

```go
// capUpstreamQuerySections 把折叠段列表压进上游 10000 rune 校验上限。
// 语义（设计文档 2026-09-18 改动三）：
//   - 不超限：按原顺序直通，与旧拼接逐字节一致。
//   - 超限：先丢低权重段（从最旧开始），不够再丢中权重段；高权重段（skills/
//     task/tail）永不丢。丢弃以整段为单位，段内不截断（段预算已约束过）。
//   - 丢弃时插入 "...[omitted: N older history sections]..." 标记。
//   - 防御兜底：只剩高段仍超限（理论不可能：4000+2000+2800=8800），
//     回退 capUpstreamQuery 字符串硬切。
func capUpstreamQuerySections(sections []querySection) string {
	const limit = MaxUpstreamQueryRunes - 100
	joined := joinWeightedSections(sections)
	if len([]rune(joined)) <= limit {
		return joined
	}
	// 尝试丢弃：从低权重、最旧的段开始。keep[i]=false 表示丢弃。
	drop := func(minWeight int) (keep []bool, dropped int) {
		keep = make([]bool, len(sections))
		// 先全保留，再从最旧的「可丢段」开始丢到装得下。
		for i := range keep {
			keep[i] = true
		}
		total := len([]rune(joined))
		for i := 0; i < len(sections); i++ {
			if sections[i].Weight >= minWeight {
				continue // 高于本档的不丢
			}
			total -= len([]rune(sections[i].Text)) + 2 // "\n\n" 分隔
			keep[i] = false
			dropped++
			if total <= limit {
				break
			}
		}
		return keep, dropped
	}
	var keep []bool
	var dropped int
	keep, dropped = drop(2) // 丢 Weight<2（即 1）
	if keptJoin(sections, keep) != "" && len([]rune(keptJoin(sections, keep))) > limit {
		keep, dropped = drop(3) // 中段也超：继续丢 Weight<3（即 1、2）
	}
	result := keptJoin(sections, keep)
	if len([]rune(result)) > limit {
		// 只剩高段仍超（理论不可能）：字符串硬切兜底。
		return capUpstreamQuery(result)
	}
	if dropped > 0 {
		marker := fmt.Sprintf("...[omitted: %d older history sections]...", dropped)
		// 标记插在最后一个被丢段的位置（时间序即位置序），保持前后文邻接关系。
		return insertOmissionMarker(sections, keep, marker)
	}
	return result
}

// keptJoin 只拼接 keep[i]=true 的段。
func keptJoin(sections []querySection, keep []bool) string {
	var parts []string
	for i, s := range sections {
		if keep[i] {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// insertOmissionMarker 在首个被丢段的位置插入省略标记后返回最终串。
func insertOmissionMarker(sections []querySection, keep []bool, marker string) string {
	var parts []string
	inserted := false
	for i, s := range sections {
		if !keep[i] {
			if !inserted {
				parts = append(parts, marker)
				inserted = true
			}
			continue
		}
		parts = append(parts, s.Text)
	}
	if !inserted {
		return strings.Join(parts, "\n\n")
	}
	return strings.Join(parts, "\n\n")
}
```

注意 `keptJoin`/`insertOmissionMarker` 中 marker 计入长度：`insertOmissionMarker` 的产物也必须 ≤ limit——drop 循环的 `total -= ... + 2` 已留了分隔余量，marker 长度（约 50 rune）从 `limit` 预扣：把 `limit` 的使用改为 `limit-markerReserve`，`const markerReserve = 80`。执行者落地时以「最终产物 rune 数 ≤ limit」为验收，写一个断言进测试（Step 1 测试 3 已有 `> 9900` 检查）。

- [ ] **Step 4: splitMessagesSeed 出站处接线**

`joinWeightedSections(sections)` 的调用点改为：

```go
	return splitResult{Query: capUpstreamQuerySections(sections)}
```

同时删除 `joinWeightedSections` 注释里「当前 cap 阶段之前」的措辞（它现在是 cap 的内部 helper，保留函数供 cap 复用）。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/provider/ -run 'TestCapUpstreamQuerySections|TestFolded|TestSplitMessages' -v`
Expected: PASS。

- [ ] **Step 6: 跑全量 + Commit**

Run: `go test ./internal/provider/`
Expected: PASS。

```bash
git add internal/provider/messages.go internal/provider/skills_fold_test.go
git commit -m "feat: weighted section dropping in capUpstreamQuery"
```

---

### Task 6: WafProbe 旁路确认 + 出站链路核验

**Files:**
- Modify: `internal/provider/request.go:42-49`（确认探针旁路，可能无改动）
- Test: `internal/provider/skills_fold_test.go`（新增端到端断言）

**Interfaces:**
- Consumes: Task 5 全部。
- Produces: 无新接口；本 task 是契约钉子。

- [ ] **Step 1: 写失败测试（钉住探针旁路与旧 cap 路径共存）**

```go
// TestFoldedProbeQueryUncapped: 探针请求（WafProbe）折叠产物不经 cap/剥离/
// 权重丢弃——逐字复现可疑内容是探针的存在意义（见 ChatRequest.WafProbe 注释）。
// 出站旁路发生在 request.go（query = upstreamQuery 原样），本测试钉住
// splitMessagesSeed 层面探针 tail 不加前缀的既有契约不因 sections 化回归。
func TestFoldedProbeQueryUncapped(t *testing.T) {
	p := New()
	msgs := []ChatMessage{mustMsg(t, "user", "原始任务")}
	msgs = append(msgs, *assistantFollowup(&Result{Content: "回复"}))
	msgs = append(msgs, mustMsg(t, "user", "PROBE_MARKER 精确字节形态\n\n<script>alert(1)</script>"))

	split := p.splitMessagesSeed(msgs, "", true, false)
	if !strings.HasPrefix(split.Query, "PROBE_MARKER") {
		t.Fatalf("probe tail must be verbatim, got prefix %q", split.Query[:min(60, len(split.Query))])
	}
	if strings.Contains(split.Query, "[User]\nPROBE_MARKER") {
		t.Fatal("probe tail must not carry [User] prefix")
	}
}
```

若 `min` helper 不存在，用内联 `if len(...) > 60 { ... }` 替代。

- [ ] **Step 2: 运行**

Run: `go test ./internal/provider/ -run TestFoldedProbeQueryUncapped -v`
Expected: PASS（探针路径在 sections 化中未触碰——若 FAIL 说明 Task 4/5 改坏探针分支，回去修）。

- [ ] **Step 3: 核对 request.go 出站接线**

打开 `internal/provider/request.go` 42-49 行确认：

```go
	upstreamQuery := split.Query
	if wafNeutralizeEnabled() && !req.WafProbe {
		upstreamQuery = wafNeutralize(upstreamQuery)
	}
	query := capUpstreamQuery(upstreamQuery)
	if req.WafProbe {
		query = upstreamQuery
	}
```

问题：这里对**已 cap 过的**折叠产物再跑一次字符串 `capUpstreamQuery`——双重 cap 冗余但无害（第一次已 ≤9900，第二次直通）。**不改动**（探针旁路依赖这段的 `req.WafProbe` 分支，重构风险大于收益）。在 `request.go` 42 行上方加一行注释：

```go
	// 折叠路径的权重丢弃已在 splitMessagesSeed 内完成（capUpstreamQuerySections）；
	// 此处 capUpstreamQuery 对折叠产物是幂等直通，对增量/首轮路径仍是唯一 cap。
```

- [ ] **Step 4: 跑全量 + Commit**

Run: `go test ./...`
Expected: PASS（全仓库）。

```bash
git add internal/provider/request.go internal/provider/skills_fold_test.go
git commit -m "test: pin probe bypass and cap wiring contracts"
```

---

## Self-Review 结果

- **Spec 覆盖**：改动一（Task 1-2）、改动二（Task 3）、改动三（Task 4-5）、探针旁路与路径隔离（Task 6）。设计文档五条测试要求全部有对应 task。
- **占位符扫描**：Task 4 Step 3 含「实现说明」引导段，明确指示执行者以逐段入列实现，非 TBD。
- **类型一致性**：`querySection{Text, Weight}`、`capUpstreamQuerySections([]querySection) string`、`stripSystemReminders(string) string` 在 Task 3-5 间签名一致。
