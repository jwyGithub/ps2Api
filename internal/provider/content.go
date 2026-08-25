package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// unsupportedMediaKinds 把入站的多模态内容块类型映射到面向调用方的类别名。
//
// 上游 Postman /chat 的出站体只有一个纯文本 query 字段，没有任何图片/附件通道。
// 2026-08-25 的探针(imgprobe_test.go)逐一验证过：把 input.query 换成 content blocks
// 数组会被服务端以 INPUT_VALIDATION_ERROR/Forbidden 拒绝；而 input.attachments /
// input.images / input.files / body.selectedContext 四个候选字段全部被静默忽略
// （请求 200，但模型回答"没有收到任何图片"）；上游模型本身也自述
// "no tool for accepting an uploaded image"。
//
// 结论：这类内容只能在入站时明确拒绝。静默丢弃(降级成 "[image attachment]" 占位符)
// 更糟——模型看不到附件却收到一句"这里有个附件"，只能瞎猜，调用方还以为请求成功了。
var unsupportedMediaKinds = map[string]string{
	// Anthropic /v1/messages
	"image":    "image",
	"document": "document",
	// OpenAI /v1/chat/completions
	"image_url": "image",
	"file":      "document",
	// OpenAI /v1/responses
	"input_image": "image",
	"input_file":  "document",
}

// UnsupportedMediaContent 扫描入站消息，返回第一个上游无法接收的媒体类别("image"/"document")。
// 与 UnsupportedToolResult 同一模式：在 provider 层判定，由 HTTP 层决定怎么回错。
func UnsupportedMediaContent(messages []ChatMessage) (string, bool) {
	for _, msg := range messages {
		if kind, ok := UnsupportedMediaInJSON(msg.Content); ok {
			return kind, true
		}
	}
	return "", false
}

// UnsupportedMediaInJSON 递归扫描任意 JSON 值，找出第一个不受支持的媒体块。
//
// 递归而非只看顶层数组，是因为媒体块的位置有三种：直接在 content blocks 数组里、
// 嵌在 tool_result 的 content 内、或(Responses 协议)在 input 项的 content 数组里。
// 递归只按 {"type": ...} 这一个特征匹配，text 块字符串值里出现的同名字面量不会被
// 解析成对象，因此不会误报。
//
// Responses 协议必须用它直接扫原始 input：responsesToOpenAI 会把 content 抽成纯文本，
// 图片块在那一步就已丢失，转换成 ChatMessage 之后再查是查不到的。
func UnsupportedMediaInJSON(raw json.RawMessage) (string, bool) {
	var v interface{}
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return unsupportedMediaInValue(v)
}

func unsupportedMediaInValue(v interface{}) (string, bool) {
	switch node := v.(type) {
	case map[string]interface{}:
		if typ, _ := node["type"].(string); typ != "" {
			if kind, bad := unsupportedMediaKinds[typ]; bad {
				return kind, true
			}
		}
		for _, child := range node {
			if kind, ok := unsupportedMediaInValue(child); ok {
				return kind, true
			}
		}
	case []interface{}:
		for _, child := range node {
			if kind, ok := unsupportedMediaInValue(child); ok {
				return kind, true
			}
		}
	}
	return "", false
}

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
			// 兜底：正常路径下 HTTP 层已用 UnsupportedMediaContent 把带图请求挡成 400，
			// 走不到这里。保留占位符只为防御内部调用路径(会话重放等)漏检时不至于丢结构。
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
