package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactMessagesTruncatesOversizedAnthropicToolResult(t *testing.T) {
	big := strings.Repeat("<div>x</div>", 2000) // ~24KB 类 HTML 文本
	msgs := []ChatMessage{
		{Role: "user", Content: rawJSON(t, `"hello"`)},
		{Role: "user", Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":`+jsonString(big)+`}]`)},
	}
	out, changed := CompactMessages(msgs, 2048)
	if !changed {
		t.Fatalf("expected compaction to change oversized tool_result")
	}
	if len(out) != len(msgs) {
		t.Fatalf("message count must be preserved: got %d want %d", len(out), len(msgs))
	}
	// 原切片不被改动（返回新切片）
	if string(msgs[1].Content) == string(out[1].Content) {
		t.Fatalf("original slice content must not be mutated")
	}
	text := ExtractText(out[1].Content)
	if !strings.Contains(text, "bytes omitted") {
		t.Fatalf("compacted tool_result should carry omission marker, got: %.120q", text)
	}
	if len(out[1].Content) >= len(msgs[1].Content) {
		t.Fatalf("compacted content should be smaller: got %d orig %d", len(out[1].Content), len(msgs[1].Content))
	}
	// tool_use_id 配对必须保留
	var blocks []map[string]interface{}
	if json.Unmarshal(out[1].Content, &blocks) != nil || blocks[0]["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool_use_id pairing must be preserved: %s", out[1].Content)
	}
}

func TestCompactMessagesTruncatesOpenAIToolRole(t *testing.T) {
	big := strings.Repeat("A", 20000)
	msgs := []ChatMessage{
		{Role: "tool", ToolCallID: "call_1", Content: rawJSON(t, jsonString(big))},
	}
	out, changed := CompactMessages(msgs, 4096)
	if !changed {
		t.Fatalf("expected compaction of oversized tool-role message")
	}
	if out[0].ToolCallID != "call_1" {
		t.Fatalf("tool_call_id must be preserved")
	}
	if len(out[0].Content) >= len(msgs[0].Content) {
		t.Fatalf("compacted content should be smaller")
	}
}

func TestCompactMessagesNoChangeWhenUnderBudget(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: rawJSON(t, `"short question"`)},
		{Role: "user", Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"small result"}]`)},
	}
	_, changed := CompactMessages(msgs, 4096)
	if changed {
		t.Fatalf("under-budget messages should not be compacted → lets router fall back to failover")
	}
}

// jsonString 把任意字符串安全编码为 JSON 字符串字面量（含引号），供内联到测试用 JSON 里。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
