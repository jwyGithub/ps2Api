package provider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const toolProtocolHint = `You can call tools. When you need a tool, reply with ONLY this markup (no extra prose around it):

<tool_call>
<name>TOOL_NAME</name>
<arguments>{"arg":"value"}</arguments>
</tool_call>

Rules:
- arguments MUST be a single JSON object.
- You may emit multiple <tool_call> blocks if several tools are needed.
- After receiving [Tool Result ...], continue the task; call more tools if needed, otherwise answer the user.
- Do not invent tool names that are not listed below.

Available tools:
`

var (
	toolCallBlockRe = regexp.MustCompile(`(?is)<tool_call>\s*<name>\s*([^<]+?)\s*</name>\s*<(?:arguments|parameters?)>\s*([\s\S]*?)\s*</(?:arguments|parameters?)>\s*</(?:tool_call|invoke)>`)
	fenceWrapRe     = regexp.MustCompile("(?is)```(?:xml|tool_call|json)?\\s*(<tool_call>[\\s\\S]*?</tool_call>)\\s*```")
)

func allowedToolNames(tools []interface{}) map[string]bool {
	out := map[string]bool{}
	for _, name := range toolNames(tools) {
		out[name] = true
	}
	return out
}

func toolNames(tools []interface{}) []string {
	var names []string
	seen := map[string]bool{}
	for _, tool := range tools {
		name := extractToolName(tool)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func selectedTools(tools []interface{}, choice interface{}) ([]interface{}, string) {
	if choice == nil {
		return tools, ""
	}
	if mode, ok := choice.(string); ok {
		switch mode {
		case "none":
			return nil, ""
		case "required", "any":
			return tools, "You must call at least one available tool."
		default:
			return tools, ""
		}
	}
	m, ok := choice.(map[string]interface{})
	if !ok {
		return tools, ""
	}
	if typ, _ := m["type"].(string); typ == "none" {
		return nil, ""
	} else if typ == "any" {
		return tools, "You must call at least one available tool."
	}
	name, _ := m["name"].(string)
	if fn, ok := m["function"].(map[string]interface{}); ok && name == "" {
		name, _ = fn["name"].(string)
	}
	if name == "" {
		return tools, ""
	}
	for _, tool := range tools {
		if extractToolName(tool) == name {
			return []interface{}{tool}, "You must call the " + name + " tool."
		}
	}
	return nil, ""
}

// codexToolNamespaces 是 Codex 内部带点号的工具命名空间。OpenAI 工具名不允许点号，
// Codex 把 functions.exec 序列化成 functions__exec 发出；但它的执行器只认带点号的原名，
// 不做反向映射，于是网关转发的 functions__exec 被判为 “unsupported custom tool call”，
// 模型反复重发同一调用。这里在出站方向把已知命名空间的第一个 __ 还原成 . 让 Codex 认得。
// 仅限这两个命名空间，避免误伤 MCP 惯用的 server__tool 这类字面 __ 工具名。
var codexToolNamespaces = []string{"functions", "collaboration"}

func remapCodexToolName(name string) string {
	for _, ns := range codexToolNamespaces {
		if strings.HasPrefix(name, ns+"__") {
			return ns + "." + strings.TrimPrefix(name, ns+"__")
		}
	}
	return name
}

func extractToolName(tool interface{}) string {
	m, ok := tool.(map[string]interface{})
	if !ok {
		return ""
	}
	if fn, ok := m["function"].(map[string]interface{}); ok {
		if name, _ := fn["name"].(string); name != "" {
			return name
		}
	}
	name, _ := m["name"].(string)
	return name
}

func extractToolDesc(tool interface{}) string {
	m, ok := tool.(map[string]interface{})
	if !ok {
		return ""
	}
	if fn, ok := m["function"].(map[string]interface{}); ok {
		if d, _ := fn["description"].(string); d != "" {
			return d
		}
	}
	d, _ := m["description"].(string)
	return d
}

func extractToolSchema(tool interface{}) interface{} {
	m, ok := tool.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	if fn, ok := m["function"].(map[string]interface{}); ok {
		if fn["parameters"] != nil {
			return fn["parameters"]
		}
	}
	if m["input_schema"] != nil {
		return m["input_schema"]
	}
	if m["parameters"] != nil {
		return m["parameters"]
	}
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

func buildToolProtocol(tools []interface{}) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(toolProtocolHint)
	for _, tool := range tools {
		name := extractToolName(tool)
		if name == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s%s\n", name, compactToolSignature(extractToolSchema(tool)))
	}
	return strings.TrimRight(b.String(), "\n")
}

func compactToolSignature(schema interface{}) string {
	m, _ := schema.(map[string]interface{})
	properties, _ := m["properties"].(map[string]interface{})
	if len(properties) == 0 {
		return "()"
	}
	required := map[string]bool{}
	if list, ok := m["required"].([]interface{}); ok {
		for _, item := range list {
			if name, ok := item.(string); ok {
				required[name] = true
			}
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		property, _ := properties[name].(map[string]interface{})
		typeName, _ := property["type"].(string)
		if typeName == "" {
			typeName = "any"
		}
		if required[name] {
			name += "*"
		}
		parts = append(parts, name+":"+typeName)
	}
	return "(" + strings.Join(parts, ",") + ")"
}

func injectToolProtocol(query string, tools []interface{}) string {
	proto := buildToolProtocol(tools)
	if proto == "" {
		return query
	}
	const separator = "\n\nUser request:\n"
	combined := proto + separator + query
	if len(combined) <= MaxQueryLen {
		return combined
	}
	queryBudget := MaxQueryLen / 2
	if len(query) < queryBudget {
		queryBudget = len(query)
	}
	q := strings.ToValidUTF8(query[len(query)-queryBudget:], "")
	protoBudget := MaxQueryLen - len(separator) - len(q)
	if len(proto) > protoBudget {
		proto = proto[:protoBudget]
		if end := strings.LastIndexByte(proto, '\n'); end > 0 {
			proto = proto[:end]
		}
	}
	return proto + separator + q
}

func normalizeToolJSON(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if s == "" {
		return "{}", true
	}
	for _, candidate := range []string{s, escapeJSONControls(s)} {
		var v map[string]interface{}
		if json.Unmarshal([]byte(candidate), &v) != nil || v == nil {
			continue
		}
		b, err := json.Marshal(v)
		if err == nil {
			return string(b), true
		}
	}
	return "", false
}

func escapeJSONControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	for _, r := range s {
		if inString && !escaped {
			switch r {
			case '\n':
				b.WriteString(`\n`)
				continue
			case '\r':
				b.WriteString(`\r`)
				continue
			case '\t':
				b.WriteString(`\t`)
				continue
			}
		}
		b.WriteRune(r)
		if r == '"' && !escaped {
			inString = !inString
		}
		if inString && r == '\\' {
			escaped = !escaped
		} else {
			escaped = false
		}
	}
	return b.String()
}

func newSimToolCallID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "call_sim"
	}
	return "call_" + hex.EncodeToString(b[:])
}

func parseSimulatedToolCalls(text string, allowed map[string]bool) (string, []ToolCall) {
	if text == "" || len(allowed) == 0 {
		return text, nil
	}
	src := fenceWrapRe.ReplaceAllString(text, "$1")
	matches := toolCallBlockRe.FindAllStringSubmatchIndex(src, -1)
	if len(matches) == 0 {
		return text, nil
	}
	var calls []ToolCall
	var cleaned strings.Builder
	last := 0
	for _, loc := range matches {
		cleaned.WriteString(src[last:loc[0]])
		name := strings.TrimSpace(src[loc[2]:loc[3]])
		if name == "" || !allowed[name] {
			cleaned.WriteString(src[loc[0]:loc[1]])
			last = loc[1]
			continue
		}
		args, ok := normalizeToolJSON(src[loc[4]:loc[5]])
		if !ok {
			cleaned.WriteString(src[loc[0]:loc[1]])
			last = loc[1]
			continue
		}
		last = loc[1]
		tc := ToolCall{ID: newSimToolCallID(), Type: "function"}
		tc.Function.Name = remapCodexToolName(name)
		tc.Function.Arguments = args
		calls = append(calls, tc)
	}
	cleaned.WriteString(src[last:])
	out := strings.TrimSpace(cleaned.String())
	if len(calls) == 0 {
		return text, nil
	}
	return out, calls
}

func applySimulatedTools(res *Result, content string, tools []interface{}) string {
	if res == nil || len(res.ToolCalls) > 0 {
		return content
	}
	cleaned, calls := parseSimulatedToolCalls(content, allowedToolNames(tools))
	if len(calls) == 0 {
		return content
	}
	res.ToolCalls = calls
	return cleaned
}

func simulatedDeltas(content string, tools []interface{}) (string, []Delta) {
	cleaned, calls := parseSimulatedToolCalls(content, allowedToolNames(tools))
	if len(calls) == 0 {
		return content, nil
	}
	var out []Delta
	for i, tc := range calls {
		out = append(out, Delta{
			ToolCalls: []DeltaToolCall{{
				Index: i,
				ID:    tc.ID,
				Type:  "function",
				Function: &struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
			}},
		})
	}
	return cleaned, out
}
