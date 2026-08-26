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
	var contextParts []string
	for i, msg := range messages {
		if i == queryIdx || i >= skipFrom {
			continue
		}
		if msg.Role == "tool" || isAnthropicToolResult(msg) {
			contextParts = append(contextParts, "[Previous tool result omitted]")
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
