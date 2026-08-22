package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

type splitResult struct {
	Query           string
	SeedingMessages []map[string]string
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
						if b["type"] == "tool_result" {
							id, _ := b["tool_use_id"].(string)
							label := "Tool Result"
							if failed, _ := b["is_error"].(bool); failed {
								label = "Tool Error"
							}
							parts = append([]string{fmt.Sprintf("[%s id=%s]\n%s", label, id, toolResultText(b["content"]))}, parts...)
						} else if b["type"] == "text" {
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
		budget := MaxQueryLen - len(instruction)
		if len(block) > budget {
			head := 256
			block = block[:head] + "\n...[tool result truncated]...\n" + block[len(block)-(budget-head-32):]
		}
		query = block + instruction
	} else {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				queryIdx = i
				break
			}
		}
		raw := ""
		if queryIdx >= 0 {
			raw = ExtractText(messages[queryIdx].Content)
		}
		if len(raw) > MaxQueryLen {
			raw = raw[len(raw)-MaxQueryLen:]
		}
		query = raw
	}

	if hasConv {
		return splitResult{Query: query}
	}

	// 首轮：把历史塞进 seedingMessages
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
	if len(context) > MaxQueryLen {
		const marker = "\n...[conversation context truncated]...\n"
		budget := MaxQueryLen - len(marker)
		head := budget / 2
		context = strings.ToValidUTF8(context[:head], "") + marker +
			strings.ToValidUTF8(context[len(context)-(budget-head):], "")
	}
	return splitResult{
		Query: query,
		SeedingMessages: []map[string]string{
			{"role": "user", "content": context},
			{"role": "assistant", "content": "I have the full conversation history above and will continue from where we left off."},
		},
	}
}
