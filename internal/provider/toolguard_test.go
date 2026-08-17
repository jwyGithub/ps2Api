package provider

import "testing"

func TestUnsupportedToolResultDetectsOpenAIHistory(t *testing.T) {
	messages := []ChatMessage{
		{Role: "tool", ToolCallID: "call_1", Content: rawText(t, "unsupported custom tool call: functions__exec")},
	}
	if name, ok := UnsupportedToolResult(messages); !ok || name != "functions__exec" {
		t.Fatalf("name=%q ok=%v", name, ok)
	}
}

func TestUnsupportedToolResultDetectsAnthropicHistory(t *testing.T) {
	messages := []ChatMessage{{
		Role:    "user",
		Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"unsupported custom tool call: Bash","is_error":true}]`),
	}}
	if name, ok := UnsupportedToolResult(messages); !ok || name != "Bash" {
		t.Fatalf("name=%q ok=%v", name, ok)
	}
}

func TestUnsupportedToolResultIgnoresQuotedOpenAIError(t *testing.T) {
	messages := []ChatMessage{{Role: "tool", Content: rawText(t, `trace output: content="unsupported custom tool call: functions__exec"`)}}
	if name, ok := UnsupportedToolResult(messages); ok {
		t.Fatalf("unexpected unsupported tool %q", name)
	}
}

func TestUnsupportedToolResultIgnoresSuccessfulAnthropicToolResult(t *testing.T) {
	messages := []ChatMessage{{
		Role:    "user",
		Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"trace output: unsupported custom tool call: functions__exec","is_error":false}]`),
	}}
	if name, ok := UnsupportedToolResult(messages); ok {
		t.Fatalf("unexpected unsupported tool %q", name)
	}
}

func TestUnsupportedToolResultIgnoresNormalToolErrors(t *testing.T) {
	messages := []ChatMessage{{Role: "tool", Content: rawText(t, "command failed with exit code 1")}}
	if name, ok := UnsupportedToolResult(messages); ok {
		t.Fatalf("unexpected unsupported tool %q", name)
	}
}
