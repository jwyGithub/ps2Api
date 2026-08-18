package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestIsClientReservedTool(t *testing.T) {
	cases := map[string]bool{
		"functions__exec":            true,
		"functions.exec":             true,
		"collaboration__spawn_agent": true,
		"collaboration.handoff":      true,
		"executeShellCommand":        true, // Agent Mode 原生工具，thirdParty 重名会被上游整个拒绝
		"readFile":                   true,
		"listDirectory":              true,
		"searchInFiles":              true,
		"get_weather":                false,
		"mcp__codegraph__explore":    false, // 非保留命名空间的字面 __ 不误伤
		"functionsX":                 false, // 前缀必须带分隔符
	}
	for name, want := range cases {
		if got := isClientReservedTool(name); got != want {
			t.Errorf("isClientReservedTool(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestBuildThirdPartyToolsDropsReserved(t *testing.T) {
	var p Provider
	tools := []interface{}{
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "functions__exec"}},
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "collaboration__wait_agent"}},
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "get_weather"}},
	}
	out := p.buildThirdPartyTools(tools)
	proxy, ok := out["proxy-tools"].(map[string]interface{})
	if !ok {
		t.Fatalf("proxy-tools missing: %#v", out)
	}
	list, _ := proxy["tools"].([]map[string]interface{})
	if len(list) != 1 || list[0]["name"] != "get_weather" {
		t.Fatalf("reserved tools not filtered, got %#v", list)
	}
}

func TestBuildThirdPartyToolsCompactsClaudeCodePayload(t *testing.T) {
	p := New()
	out := p.buildThirdPartyTools([]interface{}{map[string]interface{}{
		"name":        "large_tool",
		"description": strings.Repeat("long description ", 100),
		"input_schema": map[string]interface{}{
			"type":        "object",
			"description": "schema docs",
			"required":    []interface{}{"command"},
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string", "description": "property docs", "default": "pwd"},
			},
		},
	}})
	tool := out["proxy-tools"].(map[string]interface{})["tools"].([]map[string]interface{})[0]
	if len(tool["description"].(string)) > MaxToolDescLen {
		t.Fatalf("tool description was not bounded: %d", len(tool["description"].(string)))
	}
	schema := tool["parameters"].(map[string]interface{})
	command := schema["properties"].(map[string]interface{})["command"].(map[string]interface{})
	if schema["description"] != nil || command["description"] != nil || command["default"] != nil || command["type"] != "string" {
		t.Fatalf("tool schema was not safely compacted: %#v", schema)
	}
}

func TestParseSimulatedToolCalls(t *testing.T) {
	text := `I'll look that up.

<tool_call>
<name>get_weather</name>
<arguments>{"city":"Tokyo","unit":"c"}</arguments>
</tool_call>`
	cleaned, calls := parseSimulatedToolCalls(text, map[string]bool{"get_weather": true})
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, `"city":"Tokyo"`) {
		t.Fatalf("args = %q", calls[0].Function.Arguments)
	}
	if strings.Contains(cleaned, "<tool_call>") {
		t.Fatalf("cleaned still has markup: %q", cleaned)
	}
}

func TestParseSimulatedToolCallsAcceptsInvokeClosingTag(t *testing.T) {
	text := `<tool_call><name>Bash</name><arguments>{"command":"pwd"}</arguments></invoke>`
	cleaned, calls := parseSimulatedToolCalls(text, map[string]bool{"Bash": true})
	if len(calls) != 1 || calls[0].Function.Name != "Bash" || calls[0].Function.Arguments != `{"command":"pwd"}` || cleaned != "" {
		t.Fatalf("unexpected parsed call: cleaned=%q calls=%+v", cleaned, calls)
	}
}

func TestParseSimulatedToolCallsAcceptsAlternateParameterClosingTag(t *testing.T) {
	text := `<tool_call><name>Edit</name><arguments>{"file_path":"/tmp/file","new_string":"import example;"}</parameter></invoke>`
	cleaned, calls := parseSimulatedToolCalls(text, map[string]bool{"Edit": true})
	if len(calls) != 1 || calls[0].Function.Name != "Edit" || calls[0].Function.Arguments != `{"file_path":"/tmp/file","new_string":"import example;"}` || cleaned != "" {
		t.Fatalf("unexpected parsed call: cleaned=%q calls=%+v", cleaned, calls)
	}
}

func TestParseSimulatedToolCallsIgnoresUnknown(t *testing.T) {
	text := `<tool_call>
<name>secret_tool</name>
<arguments>{}</arguments>
</tool_call>`
	_, calls := parseSimulatedToolCalls(text, map[string]bool{"get_weather": true})
	if len(calls) != 0 {
		t.Fatalf("expected no calls, got %+v", calls)
	}
}

func TestParseFencedToolCalls(t *testing.T) {
	text := "```xml\n<tool_call>\n<name>search</name>\n<arguments>{\"q\":\"go\"}</arguments>\n</tool_call>\n```"
	_, calls := parseSimulatedToolCalls(text, map[string]bool{"search": true})
	if len(calls) != 1 || calls[0].Function.Name != "search" {
		t.Fatalf("got %+v", calls)
	}
}

func TestParseSimulatedToolCallRepairsMultilineCommand(t *testing.T) {
	text := "<tool_call><name>Bash</name><arguments>{\"command\":\"cat <<'EOF'\nhello\nEOF\"}</arguments></tool_call>"
	_, calls := parseSimulatedToolCalls(text, map[string]bool{"Bash": true})
	if len(calls) != 1 {
		t.Fatalf("multiline command was not repaired: %+v", calls)
	}
	var arguments map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &arguments); err != nil || arguments["command"] != "cat <<'EOF'\nhello\nEOF" {
		t.Fatalf("multiline command was not repaired: %q, %v", calls[0].Function.Arguments, err)
	}
}

func TestParseSimulatedToolCallDoesNotInventRawArgument(t *testing.T) {
	text := `<tool_call><name>Bash</name><arguments>{not json}</arguments></tool_call>`
	cleaned, calls := parseSimulatedToolCalls(text, map[string]bool{"Bash": true})
	if len(calls) != 0 || !strings.Contains(cleaned, "{not json}") {
		t.Fatalf("malformed arguments must stay as text, cleaned=%q calls=%+v", cleaned, calls)
	}
}

func TestInjectToolProtocol(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "get_weather",
				"description": "weather",
				"parameters":  map[string]interface{}{"type": "object"},
			},
		},
	}
	out := injectToolProtocol("hello", tools)
	if !strings.Contains(out, "<tool_call>") || !strings.Contains(out, "get_weather") || !strings.Contains(out, "hello") {
		t.Fatalf("unexpected inject output:\n%s", out)
	}
}

func TestInjectToolProtocolKeepsUserRequestWithManyTools(t *testing.T) {
	tools := make([]interface{}, 100)
	for i := range tools {
		tools[i] = map[string]interface{}{
			"name":        fmt.Sprintf("tool_%03d", i),
			"description": strings.Repeat("large description ", 100),
			"input_schema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string", "description": strings.Repeat("large property description ", 100)},
				},
				"required": []interface{}{"command"},
			},
		}
	}
	query := "请分析 employee/list 的组织数据关联"
	out := injectToolProtocol(query, tools)
	if len(out) > MaxQueryLen || !strings.Contains(out, "User request:\n"+query) || !strings.Contains(out, "tool_099(command*:string)") {
		t.Fatalf("tool protocol lost the user request or tools: len=%d\n%s", len(out), out)
	}
}

func TestApplySimulatedToolsSkipsWhenNativePresent(t *testing.T) {
	res := &Result{ToolCalls: []ToolCall{{ID: "native", Type: "function"}}}
	text := `<tool_call><name>get_weather</name><arguments>{}</arguments></tool_call>`
	tools := []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "get_weather"}}}
	got := applySimulatedTools(res, text, tools)
	if got != text || len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "native" {
		t.Fatalf("should keep native tool calls")
	}
}

func TestAnthropicToolSchema(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{
			"name":         "lookup",
			"description":  "find things",
			"input_schema": map[string]interface{}{"type": "object"},
		},
	}
	proto := buildToolProtocol(tools)
	if !strings.Contains(proto, "lookup") {
		t.Fatalf("missing anthropic tool: %s", proto)
	}
	if _, err := json.Marshal(tools); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedToolsHonorsChoice(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "first"}},
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "second"}},
	}
	if got, _ := selectedTools(tools, "none"); len(got) != 0 {
		t.Fatalf("none selected %d tools", len(got))
	}
	got, instruction := selectedTools(tools, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "second"}})
	if len(got) != 1 || extractToolName(got[0]) != "second" || !strings.Contains(instruction, "second") {
		t.Fatalf("specific selection = %#v, %q", got, instruction)
	}
}

func TestIsRequestRejectionMessage(t *testing.T) {
	reject := []string{
		"Some of your MCP servers have tool names that are reseved for Agent Mode. Try removing the MCP servers with these tools: executeShellCommand",
		"unsupported custom tool call: functions__exec",
		"unsupported call: mcp__postman_local__executeShellCommand",
		"No tools available for this request",
	}
	for _, m := range reject {
		if !isRequestRejectionMessage(m) {
			t.Errorf("expected request-rejection for %q", m)
		}
	}
	// 账号/网络类错误不能被误判为请求拒绝(否则不会触发换号/重试)
	keep := []string{"Postman AI quota exceeded", "Postman rate limited", `write tcp 192.1.1.1: broken pipe`, "Upstream timeout"}
	for _, m := range keep {
		if isRequestRejectionMessage(m) {
			t.Errorf("must NOT classify as request-rejection: %q", m)
		}
	}
}
