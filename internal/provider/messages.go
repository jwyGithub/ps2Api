package provider

import (
	"encoding/json"
	"fmt"
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

func (p *Provider) splitMessages(messages []ChatMessage, convID string) splitResult {
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
	// 改为把完整历史线性折叠进单条 USER_QUERY（conversationId=null）。折叠本身不截断；
	// 上游 10000 字符硬上限由出站前的 capUpstreamQuery 统一兜底（保头保尾、省略中段）。
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

	var contextParts []string
	for i, msg := range messages {
		if i == queryIdx || i >= skipFrom {
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
				contextParts = append(contextParts, "[System]\n"+text)
			}
		case "user":
			if text != "" {
				contextParts = append(contextParts, "[User]\n"+text)
			}
		case "assistant":
			block := "[Assistant]"
			if text != "" {
				block = "[Assistant]\n" + text
			}
			if calls := formatAssistantToolCalls(msg.ToolCalls); calls != "" {
				block += "\n\n" + calls
			}
			contextParts = append(contextParts, block)
		}
	}
	context := strings.Join(contextParts, "\n\n")
	if context == "" {
		return splitResult{Query: query}
	}
	// 折叠：历史在前，最新一轮在后。tool-tail 的 query 已是带指令的工具块，直接拼接；
	// 普通对话把最新用户输入标注为 [User] 以保留角色边界。
	tail := query
	if !isToolTail && queryIdx >= 0 && query != "" {
		tail = "[User]\n" + query
	}
	if tail != "" {
		context = context + "\n\n" + tail
	}
	return splitResult{Query: context}
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
