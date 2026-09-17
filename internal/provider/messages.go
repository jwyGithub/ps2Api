package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type splitResult struct {
	Query string
}

func toolTail(messages []ChatMessage) bool {
	return toolTailIndex(messages) >= 0
}

// toolTailIndex finds the last tool result while ignoring token accounting
// system messages appended by Anthropic-compatible clients.
func toolTailIndex(messages []ChatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if isTrailingTokenMetadata(messages[i]) {
			continue
		}
		if messages[i].Role == "tool" || isAnthropicToolResult(messages[i]) {
			return i
		}
		return -1
	}
	return -1
}

func isTrailingTokenMetadata(msg ChatMessage) bool {
	if msg.Role != "system" {
		return false
	}
	text := ExtractText(msg.Content)
	return strings.Contains(text, "<total_tokens>")
}

func formatAssistantToolCalls(raw json.RawMessage) string {
	var calls []ToolCall
	if len(raw) == 0 || json.Unmarshal(raw, &calls) != nil {
		return ""
	}
	var out []string
	for _, call := range calls {
		out = append(out, fmt.Sprintf("[Assistant Tool Call id=%s name=%s]", call.ID, call.Function.Name))
	}
	return strings.Join(out, "\n\n")
}

// countFoldedToolResults 返回一条消息在折叠时会贡献的 tool-result 条数（OpenAI 的 role:"tool"
// 记 1；Anthropic user 轮里按其携带的 tool_result block 数计），供 foldedToolResultBudget 按
// 「总条数」公平分摊预算。text block 及无法解析的内容不计入。
func countFoldedToolResults(msg ChatMessage) int {
	if msg.Role == "tool" {
		return 1
	}
	var blocks []map[string]interface{}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return 0
	}
	n := 0
	for _, b := range blocks {
		if b["type"] == "tool_result" {
			n++
		}
	}
	return n
}

// foldedToolResultBudget 按本次请求里参与折叠的 tool-result 总条数，动态分摊单条结果的 rune
// 预算：条数少时每条拿到大配额（尽量完整保留），多时自动收紧、公平均分 FoldedToolResultTotalBudgetRunes
// 总预算，并夹一个 MinFoldedToolResultRunes 下限保证每条都留得下关键头尾。整段最终仍由
// capUpstreamQuery 兜底到 MaxUpstreamQueryRunes 以内。
func foldedToolResultBudget(count int) int {
	if count <= 1 {
		return FoldedToolResultTotalBudgetRunes
	}
	per := FoldedToolResultTotalBudgetRunes / count
	if per < MinFoldedToolResultRunes {
		per = MinFoldedToolResultRunes
	}
	return per
}

// foldedToolResultParts 把一条历史 tool-result 消息（OpenAI 的 role:"tool"，或 Anthropic
// 在 user 轮里携带的 tool_result blocks）渲染成带标签的 "[Tool Result id=..]\n<content>" 片段，
// 供 conversationId=null 的 USER_QUERY 折叠路径复用。单条结果内容按传入的 budget（由
// foldedToolResultBudget 依当前 tool result 条数动态算出）保头保尾、中段省略，避免某条超大输出
// 把整段折叠挤爆（整段最终仍由 capUpstreamQuery 兜底）。只放开工具「结果」内容——assistant 的
// 工具调用参数仍只经 formatAssistantToolCalls 输出工具名、不含 arguments，故不会新增参数泄漏面。
// 无法解析出任何结果时返回 nil，由调用方回退到占位标记。
func foldedToolResultParts(msg ChatMessage, budget int) []string {
	if msg.Role == "tool" {
		content := truncateMiddleRunes(ExtractText(msg.Content), budget)
		return []string{fmt.Sprintf("[Tool Result id=%s]\n%s", msg.ToolCallID, content)}
	}
	var blocks []map[string]interface{}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}
	var parts []string
	for _, b := range blocks {
		switch b["type"] {
		case "tool_result":
			id, _ := b["tool_use_id"].(string)
			label := "Tool Result"
			if failed, _ := b["is_error"].(bool); failed {
				label = "Tool Error"
			}
			content := truncateMiddleRunes(toolResultText(b["content"]), budget)
			parts = append(parts, fmt.Sprintf("[%s id=%s]\n%s", label, id, content))
		case "text":
			// tool_result 与 text 混排的 user 轮：一并保留用户文本，避免折叠时丢历史提问。
			if text, _ := b["text"].(string); text != "" {
				parts = append(parts, "[User]\n"+text)
			}
		}
	}
	return parts
}

// foldedSystemParts 把一条折叠路径里的 system 消息拆成「清单块 + 其余文本」两部分：
//   - skills 清单（连续 ≥minSkillBlockLines 行的 "- name: description" 行块）压成
//     "- name: 描述首句"——清单是模型调用 skills 的唯一依据，按 FoldedSkillListRunes
//     独立预算兜底（2026-09-17 线上：89 条 ~28K 的清单在 FoldedSystemBudgetRunes=2000
//     的保头保尾下全丢，经网关的模型整个会话都看不到 skills）。块可多处出现，全部收集；
//     行数门槛把 ponytail 强度表（1-2 行）等杂散 kebab 列表排除在外。
//   - 其余文本走原 FoldedSystemBudgetRunes 保头保尾。
//
// 没有清单时返回 (nil, 原文)，调用方按原路径渲染。
func foldedSystemParts(text string) (skillLines []string, rest string) {
	lines := strings.Split(text, "\n")
	const minSkillBlockLines = 4
	var outLines []string
	var curBlock []string
	flush := func() {
		if len(curBlock) >= minSkillBlockLines {
			skillLines = append(skillLines, curBlock...)
		} else {
			outLines = append(outLines, curBlock...)
		}
		curBlock = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if m := skillEntryRe.FindStringSubmatch(trimmed); m != nil {
			desc := m[2]
			// 描述截到首个句号（含），再兜底 rune 上限——保留触发语境的最短可读形态。
			if i := strings.Index(desc, ". "); i > 0 {
				desc = desc[:i+1]
			}
			curBlock = append(curBlock, "- "+m[1]+": "+truncateRunes(desc, 80))
			continue
		}
		if len(curBlock) > 0 {
			flush()
		}
		outLines = append(outLines, line)
	}
	if len(curBlock) > 0 {
		flush()
	}
	if len(skillLines) == 0 {
		return nil, text
	}
	return skillLines, strings.Join(outLines, "\n")
}

// skillEntryRe 匹配 skills 清单条目："- name: 描述"。name 限 kebab-case（Claude Code
// 清单的实际形态），避免把 markdown 普通列表误当清单压缩。
var skillEntryRe = regexp.MustCompile(`^- ([a-z0-9][a-z0-9-]{1,60}): (.+)$`)

// foldSkillList 把压缩后的清单条目按 FoldedSkillListRunes 兜底。策略：名字优先——
// 先全量写「- name」（名字即 Skill 工具的调用参数，全部在场是底线），剩余预算再按序
// 把条目升级为「- name: 描述」（前段条目优先，与清单字母序一致）。
func foldSkillList(entries []string) string {
	bare := make([]string, len(entries))
	for i, e := range entries {
		name := strings.TrimPrefix(e, "- ")
		if j := strings.Index(name, ": "); j > 0 {
			name = name[:j]
		}
		bare[i] = "- " + name
	}
	fixed := 0
	for _, b := range bare {
		fixed += len([]rune(b)) + 1
	}
	if fixed > FoldedSkillListRunes {
		// 名字都放不下：按序截尾（字母序在先的条目保留）。
		var b strings.Builder
		for _, bLine := range bare {
			if b.Len()+len([]rune(bLine))+1 > FoldedSkillListRunes {
				break
			}
			b.WriteString(bLine)
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}
	// 名字全保后，剩余预算按序升级为带描述形态。
	var b strings.Builder
	left := FoldedSkillListRunes - fixed
	for i, e := range entries {
		if left >= len([]rune(e))-len([]rune(bare[i])) {
			b.WriteString(e)
			left -= len([]rune(e)) - len([]rune(bare[i]))
		} else {
			b.WriteString(bare[i])
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// splitMessagesSeed 带补种标志的折叠/切分主逻辑。
func (p *Provider) splitMessagesSeed(messages []ChatMessage, convID string, wafProbe, contextSeed bool) splitResult {
	toolIdx := toolTailIndex(messages)
	isToolTail := toolIdx >= 0
	hasConv := convID != ""

	var query string
	queryIdx := -1
	skipFrom := len(messages)

	if isToolTail {
		var parts []string
		for i := toolIdx; i >= 0; i-- {
			msg := messages[i]
			if msg.Role == "tool" {
				skipFrom = i
				parts = append([]string{fmt.Sprintf("[Tool Result id=%s]\n%s", msg.ToolCallID, ExtractText(msg.Content))}, parts...)
				continue
			}
			if isAnthropicToolResult(msg) {
				skipFrom = i
				var blocks []map[string]interface{}
				if json.Unmarshal(msg.Content, &blocks) == nil {
					for _, b := range blocks {
						switch b["type"] {
						case "tool_result":
							id, _ := b["tool_use_id"].(string)
							label := "Tool Result"
							if failed, _ := b["is_error"].(bool); failed {
								label = "Tool Error"
							}
							parts = append([]string{fmt.Sprintf("[%s id=%s]\n%s", label, id, toolResultText(b["content"]))}, parts...)
						case "text":
							if text, _ := b["text"].(string); text != "" {
								parts = append(parts, "[User Message]\n"+text)
							}
						}
					}
				}
				continue
			}
			break
		}
		block := strings.Join(parts, "\n\n")
		instruction := "\n\nProcess these tool results and continue. If you need another tool, emit <tool_call> markup; otherwise answer the user."
		// 工具结果不截断：Postman 接受很大的单轮 query，截断只会让模型拿到残缺的工具输出。
		query = block + instruction
	} else {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				queryIdx = i
				break
			}
		}
		// 这里不做截断；上游 10000 字符硬上限由出站前的 capUpstreamQuery 统一兜底。
		if queryIdx >= 0 {
			query = ExtractText(messages[queryIdx].Content)
		}
	}

	if hasConv {
		// 命中已有 Postman 会话：与网页版一致，只发新增的这一轮 query，
		// 历史由服务端按 conversationId 保存。
		return splitResult{Query: query}
	}

	// 未命中任何会话（冷启动/首轮/指纹未命中）：
	// 绝不使用 seedingMessages —— 上游 Postman 会以 INPUT_VALIDATION_ERROR/Forbidden 拒收
	// （已由网页/桌面全量抓包证实：真实客户端多轮只靠 conversationId，从不发 seedingMessages）。
	// 改为把完整历史线性折叠进单条 USER_QUERY（conversationId=null）。折叠路径逐段设预算
	// （system / 历史文本 / 待处理 tool-tail，见 types.go 的 Folded* 预算），且把「原始任务」
	// 后置渲染在紧贴最新一轮的位置——这两点保证原始任务永远落在 capUpstreamQuery 的尾部
	// 保留区，不再被巨型 system 消息挤进中段省略区（2026-09-10 线上事故）。
	// 后续轮次靠稳定指纹命中 conversationId 后自动切回增量发送。
	// 先数出折叠范围内的 tool-result 总条数，据此动态分摊单条预算：
	// 条数少时每条留得多，多时自动收紧，避免固定单条上限在短会话浪费预算、长会话又超预算。
	foldedResultCount := 0
	for i, msg := range messages {
		if i == queryIdx || i >= skipFrom {
			continue
		}
		if msg.Role == "tool" || isAnthropicToolResult(msg) {
			foldedResultCount += countFoldedToolResults(msg)
		}
	}
	perResultBudget := foldedToolResultBudget(foldedResultCount)

	// 任务消息：折叠时从时间序拎出、后置渲染在 tail 之前，保住它落在 capUpstreamQuery 的
	// 尾部保留区。选哪条 user 消息按路径分：
	//   - tool-tail 重放：tool 循环之前最近的一条 user 消息——正是这批待处理 tool results
	//     所回应的「本轮问题」。此前误取「首条 user 消息」（多轮会话里往往是环境上下文或
	//     旧问题），本轮问题作为普通折叠上下文落入中段省略区，模型只看到孤立的工具结果，
	//     回复「缺少具体任务目标」（2026-09-15 codex /v1/responses 线上形态；/v1/messages
	//     的同类反馈同根因）。
	//   - 普通续聊：首条 user 消息（原始任务），与 2026-09-10 事故的修复契约一致。
	taskIdx := -1
	if isToolTail {
		for i := toolIdx; i >= 0; i-- {
			if messages[i].Role == "user" && !isAnthropicToolResult(messages[i]) {
				taskIdx = i
				break
			}
		}
	}
	if taskIdx < 0 {
		for i, msg := range messages {
			if i == queryIdx || i >= skipFrom {
				continue
			}
			if msg.Role == "user" && !isAnthropicToolResult(msg) {
				taskIdx = i
				break
			}
		}
	}

	var contextParts []string
	var skillsBlock string // skills 清单段：前置渲染（见 sections 组装处的 cap 头部保留区契约）
	for i, msg := range messages {
		if i == queryIdx || i == taskIdx || i >= skipFrom {
			continue
		}
		if msg.Role == "tool" || isAnthropicToolResult(msg) {
			if parts := foldedToolResultParts(msg, perResultBudget); len(parts) > 0 {
				contextParts = append(contextParts, parts...)
			} else {
				contextParts = append(contextParts, "[Previous tool result omitted]")
			}
			continue
		}
		text := ExtractText(msg.Content)
		switch msg.Role {
		case "system":
			if text != "" {
				// skills 清单独立压缩（独立预算），其余文本走常规保头保尾——
				// 清单是模型调用 skills 的唯一依据，不能被散文预算挤掉。
				skillLines, rest := foldedSystemParts(text)
				if len(skillLines) > 0 {
					skillsBlock = "[System skills]\n" + foldSkillList(skillLines)
				}
				if strings.TrimSpace(rest) != "" {
					contextParts = append(contextParts, "[System]\n"+truncateMiddleRunes(rest, FoldedSystemBudgetRunes))
				}
			}
		case "user":
			if text != "" {
				contextParts = append(contextParts, "[User]\n"+truncateMiddleRunes(text, FoldedTextMsgBudgetRunes))
			}
		case "assistant":
			block := "[Assistant]"
			if text != "" {
				block = "[Assistant]\n" + truncateMiddleRunes(text, FoldedTextMsgBudgetRunes)
			}
			if calls := formatAssistantToolCalls(msg.ToolCalls); calls != "" {
				block += "\n\n" + calls
			}
			contextParts = append(contextParts, block)
		}
	}
	// 折叠分段顺序。tool-tail 重放：历史在前，本轮任务居中（紧贴待处理 tool 结果，
	// 落在 cap 的尾部保留区），2026-09-15 契约。普通续聊：原始任务置于最前——
	// 2026-09-17 线上事故：任务「后置渲染」紧贴最新消息，时间序等价于 assistant 答完后
	// 用户又下达了原始任务，模型把首任务读成新指令与最新消息并列，答非所问。前置则
	// 时间序正确，且恒落 capUpstreamQuery 头部 30% 保留区（与尾部同样安全）。
	// 重放模式下待处理 tool-tail 也截预算（单条巨结果会吃光尾部窗口）；
	// 普通对话把最新用户输入标注为 [User] 以保留角色边界。
	sections := make([]string, 0, 4)
	taskBlock := ""
	if taskIdx >= 0 {
		if task := ExtractText(messages[taskIdx].Content); task != "" {
			taskBlock = "[User (task)]\n" + truncateMiddleRunes(task, FoldedTextMsgBudgetRunes)
		}
	}
	// skills 清单段恒置最前：落在 capUpstreamQuery 头部 30% 保留区，永不落入中段省略区——
	// 折叠总量超 10000 时（如长会话 64K tool results），中段里的清单照样会被 cap 掐尾，
	// 模型只看到半个名单（2026-09-17 端到端重放实测：[System skills] 在中段时尾部条目丢失）。
	if skillsBlock != "" {
		sections = append(sections, skillsBlock)
	}
	if !isToolTail && taskBlock != "" {
		sections = append(sections, taskBlock)
	}
	if context := strings.Join(contextParts, "\n\n"); context != "" {
		sections = append(sections, context)
	}
	if isToolTail && taskBlock != "" {
		sections = append(sections, taskBlock)
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
		sections = append(sections, tail)
	}
	return splitResult{Query: strings.Join(sections, "\n\n")}
}

// capUpstreamQuery 把出站 query 压进上游 MaxUpstreamQueryRunes（10000 字符）校验上限。
// 超限时保留开头（系统提示核心）与结尾（最新一轮输入，信息权重最高），省略中段并留标记。
// 只改出站文本、不触碰 req.Messages，因此不影响会话指纹与账号粘性。
func capUpstreamQuery(q string) string {
	const marker = "\n\n...[middle context omitted: upstream limits query to 10000 chars]...\n\n"
	// 留 100 字符余量，防止服务端计数口径（如换行/转义）与本地存在细微差异。
	limit := MaxUpstreamQueryRunes - 100
	runes := []rune(q)
	if len(runes) <= limit {
		return q
	}
	head := limit * 3 / 10
	tailLen := limit - head - len([]rune(marker))
	return string(runes[:head]) + marker + string(runes[len(runes)-tailLen:])
}

// seedSummaryInstruction 是补种轮的 tail 指令：让模型对折叠历史做一段话总结，
// 不执行操作。摘要会留在服务端会话里供第二轮参照（折叠原文 + 摘要都在）。
const seedSummaryInstruction = "[User]\n以上是此前对话的完整上下文。请用一段话总结当前任务状态与最近进展，不要执行任何操作、不要调用任何工具。"
