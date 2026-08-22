package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

func ExtractText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var out []string
	for _, part := range parts {
		typ, _ := part["type"].(string)
		switch typ {
		case "text":
			if t, ok := part["text"].(string); ok {
				out = append(out, t)
			}
		case "tool_result":
			toolID, _ := part["tool_use_id"].(string)
			out = append(out, fmt.Sprintf("<tool_result id=%q>\n%s\n</tool_result>", toolID, toolResultText(part["content"])))
		case "image_url", "image", "input_image":
			out = append(out, "[image attachment]")
		default:
			if t, ok := part["text"].(string); ok {
				out = append(out, t)
			}
		}
	}
	return strings.Join(out, "\n")
}

func toolResultText(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var out []string
		for _, b := range c {
			if m, ok := b.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					out = append(out, t)
					continue
				}
				typ, _ := m["type"].(string)
				switch typ {
				case "image", "image_url", "input_image":
					out = append(out, "[image attachment]")
				case "document":
					out = append(out, "[document attachment]")
				default:
					if raw, err := json.Marshal(m); err == nil {
						out = append(out, string(raw))
					}
				}
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func isAnthropicToolResult(msg ChatMessage) bool {
	if msg.Role != "user" || len(msg.Content) == 0 {
		return false
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		return false
	}
	for _, p := range parts {
		if p["type"] == "tool_result" {
			return true
		}
	}
	return false
}

// UnsupportedToolResult returns the tool name when the caller reports that it
// cannot execute a custom tool. Replaying this history only makes the model
// emit the same tool call again, so the API layer can stop the loop early.
func UnsupportedToolResult(messages []ChatMessage) (string, bool) {
	for _, msg := range messages {
		if msg.Role == "tool" {
			if name := unsupportedToolName(ExtractText(msg.Content)); name != "" {
				return name, true
			}
			continue
		}
		if !isAnthropicToolResult(msg) {
			continue
		}
		var blocks []map[string]interface{}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block["type"] != "tool_result" {
				continue
			}
			failed, _ := block["is_error"].(bool)
			if !failed {
				continue
			}
			if name := unsupportedToolName(toolResultText(block["content"])); name != "" {
				return name, true
			}
		}
	}
	return "", false
}

func unsupportedToolName(content string) string {
	const marker = "unsupported custom tool call:"
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(strings.ToLower(content), marker) {
		return ""
	}
	name := strings.TrimSpace(content[len(marker):])
	if fields := strings.Fields(name); len(fields) > 0 {
		return strings.Trim(fields[0], "`\"'")
	}
	return "unknown"
}

// ---------- 请求构造 ----------
