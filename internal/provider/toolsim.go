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

// clientReservedToolPrefixes 是 Codex 等客户端只在本地执行的保留工具命名空间。
// 客户端会把它们上报到 tools 里，但不接受经外部端点回传的调用：一旦转发给上游让
// 模型调用，回传后必被客户端判为 "unsupported custom tool call"，触发 api.go 里的
// UnsupportedToolResult 熔断（400）与客户端重发，形成死循环（见 traces：functions__exec
// 被上游发出 1367 次、toolCallGroupId 恒为 null，且点号/下划线两种名字均被拒）。
// 不向上游播发这些工具即可从根上消除死循环——它们本就 100% 无法经代理执行。
var clientReservedToolPrefixes = []string{"functions__", "functions.", "collaboration__", "collaboration."}

// agentModeNativeTools 是 Postman Agent Mode 的原生工具名（desktop local-mode 经
// nativeToolsHash 已声明）。客户端（如 Codex 的本地 MCP）会把同名工具作为 thirdParty
// 重复上报，而上游对这些保留名会整个拒绝请求：
//
//	"Some of your MCP servers have tool names that are reserved for Agent Mode.
//	 Try removing the MCP servers with these tools: executeShellCommand"
//
// 从 thirdParty 过滤掉即可——模型仍经原生 nativeToolsHash 发出这些工具（带真实
// groupId），客户端靠工具名匹配在本地执行，与是否上报 thirdParty 无关（已验证：
// 直连上游 thirdParty 为空时 gpt-5.6-sol 照样吐结构化 executeShellCommand）。
var agentModeNativeTools = map[string]bool{
	"executeShellCommand": true,
	"readFile":            true,
	"listDirectory":       true,
	"searchInFiles":       true,
}

// requestRejectionMarkers 是上游因「请求内容」而拒绝时的 failure 文案特征(工具名冲突、
// 无可用工具等)。这类失败与账号健康无关,换账号重试无用且会污染整个号池,应直接返回。
var requestRejectionMarkers = []string{
	"reserved for agent mode", // MCP/thirdParty 工具名与 Agent Mode 原生工具重名
	"reseved for agent mode",  // 上游原文 typo,一并匹配
	"remove the mcp servers",
	"no tools available",
	"no available tool",
	"unsupported custom tool call",
	"unsupported call:",
}

func isRequestRejectionMessage(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range requestRejectionMarkers {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

func isClientReservedTool(name string) bool {
	if agentModeNativeTools[name] {
		return true
	}
	for _, p := range clientReservedToolPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
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
		tc.Function.Name = name
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

// simToolOpen is the raw opening marker a model emits to start a simulated
// tool call. During streaming we must never forward a partial copy of it as
// prose, otherwise the client sees a broken "<tool_ca" leak.
const simToolOpen = "<tool_call>"

// fenceLangs are the code-fence languages parseSimulatedToolCalls is willing to
// unwrap around a <tool_call> block (see fenceWrapRe). The empty entry covers a
// bare ``` fence with no language tag.
var fenceLangs = []string{"", "xml", "tool_call", "json"}

// isViablePrefix reports whether s could still be extended into (or already
// begins) a tool-call marker — either the raw <tool_call> opener or a code
// fence that may wrap one. When true the caller must hold s back rather than
// flush it as streaming text. It is deliberately conservative: a false positive
// only delays a few bytes, a false negative would leak a broken marker.
func isViablePrefix(s string) bool {
	if s == "" {
		return false
	}
	ls := strings.ToLower(s)
	// Raw form: s is a prefix of "<tool_call>", or already starts with it
	// (a complete/opened marker that must stay buffered for final parsing).
	if strings.HasPrefix(simToolOpen, ls) || strings.HasPrefix(ls, simToolOpen) {
		return true
	}
	// Still typing the fence delimiter itself: "`", "``", "```".
	if strings.HasPrefix("```", s) {
		return true
	}
	if strings.HasPrefix(s, "```") {
		rest := s[3:]
		lrest := strings.ToLower(rest)
		for _, lang := range fenceLangs {
			// The language tag is still being typed, e.g. "```xm".
			if lang != "" && strings.HasPrefix(lang, lrest) {
				return true
			}
			var after string
			switch {
			case lang == "":
				after = rest
			case strings.HasPrefix(lrest, lang):
				after = rest[len(lang):]
			default:
				continue
			}
			// After the (optional) language tag we allow whitespace, then the
			// start of the raw <tool_call> opener.
			trimmed := strings.TrimLeft(after, " \t\r\n")
			lt := strings.ToLower(trimmed)
			if trimmed == "" || strings.HasPrefix(simToolOpen, lt) || strings.HasPrefix(lt, simToolOpen) {
				return true
			}
		}
	}
	return false
}

// holdBackLen returns how many trailing bytes of buf must be retained because
// they might be building toward a tool-call marker. Everything before that is
// guaranteed free of any marker prefix and safe to stream immediately.
func holdBackLen(buf string) int {
	for k := 0; k < len(buf); k++ {
		if buf[k] != '<' && buf[k] != '`' {
			continue
		}
		if isViablePrefix(buf[k:]) {
			return len(buf) - k
		}
	}
	return 0
}

// splitStreamSafe splits buf into a prefix that is safe to emit as streaming
// prose right now and a pending remainder that must stay buffered until more
// text arrives (or the stream ends).
func splitStreamSafe(buf string) (safe, pending string) {
	h := holdBackLen(buf)
	return buf[:len(buf)-h], buf[len(buf)-h:]
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
