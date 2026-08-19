package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// CompactMessages 为「网关拦截后重试」压缩请求体：把过大的 tool_result 正文截断到
// budget 字节（保留头尾、中间以标记省略）。真实诱因是 Cloudflare WAF 的托管内容规则命中
// 请求体里累积的类 HTML/JS 文本（原始文件转储、网页抓取等 tool_result），随 tool_use↔tool_result
// 往返轮次增多而累积——并非账号身份、也不单纯是字节数。因此换号无用，压缩掉这些巨型 tool_result
// 正文（既缩体积又剥离触发规则的标记文本）后原号重试才是对症的修复。
//
// 只截断 tool_result 的正文字符串，绝不增删消息、不改动 tool_use↔tool_result 配对，
// 故 Postman 侧历史回放依旧成立。返回压缩后的新切片与「是否发生截断」；未发生截断
// （正文都已 ≤ budget）时返回原切片与 false，调用方据此停止压缩、回退到换号 failover。
func CompactMessages(messages []ChatMessage, budget int) ([]ChatMessage, bool) {
	if budget < 256 {
		budget = 256
	}
	out := make([]ChatMessage, len(messages))
	copy(out, messages)
	changed := false

	for i := range out {
		msg := out[i]
		switch {
		case msg.Role == "tool":
			text := ExtractText(msg.Content)
			if compacted, ok := compactText(text, budget); ok {
				if raw, err := json.Marshal(compacted); err == nil {
					out[i].Content = raw
					changed = true
				}
			}
		case isAnthropicToolResult(msg):
			if raw, ok := compactAnthropicToolResults(msg.Content, budget); ok {
				out[i].Content = raw
				changed = true
			}
		}
	}
	return out, changed
}

// compactAnthropicToolResults 遍历 Anthropic content-blocks，截断其中 tool_result 块的正文，
// 其余块（text/image 等）与结构原样保留。返回重新序列化后的 Content 与是否发生截断。
func compactAnthropicToolResults(content json.RawMessage, budget int) (json.RawMessage, bool) {
	var blocks []map[string]interface{}
	if json.Unmarshal(content, &blocks) != nil {
		return nil, false
	}
	changed := false
	for _, b := range blocks {
		if b["type"] != "tool_result" {
			continue
		}
		switch c := b["content"].(type) {
		case string:
			if compacted, ok := compactText(c, budget); ok {
				b["content"] = compacted
				changed = true
			}
		case []interface{}:
			for _, item := range c {
				m, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				if t, ok := m["text"].(string); ok {
					if compacted, ok := compactText(t, budget); ok {
						m["text"] = compacted
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return nil, false
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// compactText 把超过 budget 字节的字符串截断为「头 + 省略标记 + 尾」，并保证 UTF-8 有效。
// 未超过 budget 时返回原串与 false。头部保留更多（错误/命令上下文通常在开头），尾部保留少量。
func compactText(s string, budget int) (string, bool) {
	if len(s) <= budget {
		return s, false
	}
	omitted := len(s) - budget
	marker := fmt.Sprintf("\n...[tool result compacted: %d bytes omitted]...\n", omitted)
	body := budget - len(marker)
	if body < 64 {
		body = 64
	}
	head := body * 3 / 4
	tail := body - head
	h := strings.ToValidUTF8(s[:head], "")
	t := strings.ToValidUTF8(s[len(s)-tail:], "")
	return h + marker + t, true
}
