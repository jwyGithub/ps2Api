package provider

import (
	"encoding/json"
	"testing"
)

func TestToolCallOSSShape(t *testing.T) {
	tc := ToolCall{ID: "call_1", Type: "function"}
	tc.Function.Name = "get_weather"
	tc.Function.Arguments = `{"city":"Tokyo"}`
	b, _ := json.Marshal(tc)
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["id"] != "call_1" || m["type"] != "function" {
		t.Fatalf("tool_calls item shape: %s", b)
	}
	fn, _ := m["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("function.name: %s", b)
	}
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments must be a JSON string, got %T", fn["arguments"])
	}
	if !json.Valid([]byte(args)) {
		t.Fatalf("arguments must be valid JSON: %q", args)
	}
}

func TestNormalizeArgumentsHandlesObjectAndString(t *testing.T) {
	raw := json.RawMessage(`{"city":"Tokyo","unit":"c"}`)
	if got, want := normalizeArguments(raw), `{"city":"Tokyo","unit":"c"}`; got != want {
		t.Fatalf("object args = %q, want %q", got, want)
	}
	if got, want := normalizeArguments(json.RawMessage(`"{\"a\":1}"`)), `{"a":1}`; got != want {
		t.Fatalf("string args = %q, want %q", got, want)
	}
	if got := normalizeArguments(nil); got != "" {
		t.Fatalf("nil args = %q", got)
	}
}

func TestAppendToolArgumentsSupportsStringAndObjectChunks(t *testing.T) {
	if got := appendToolArguments(`{"city"`, `:"Tokyo"}`); got != `{"city":"Tokyo"}` {
		t.Fatalf("string chunks = %q", got)
	}
	if got := appendToolArguments(`{"city":"Tokyo"}`, `{"unit":"c"}`); got != `{"city":"Tokyo","unit":"c"}` {
		t.Fatalf("object chunks = %q", got)
	}
}

func TestToolCallChunkWithoutRepeatedIDUsesPreviousCall(t *testing.T) {
	r := NewStreamReader()
	r.Feed(`data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"id":"call_1","function":{"name":"lookup","arguments":"{\"q\""}}]}}`)
	deltas := r.Feed(`data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"function":{"arguments":":\"go\"}"}}]}}`)
	if len(deltas) != 1 || deltas[0].ToolCalls[0].Index != 0 || deltas[0].ToolCalls[0].Function.Arguments != `:"go"}` {
		t.Fatalf("continuation without id = %+v", deltas)
	}
}

func TestToolCallChunkStreamShape(t *testing.T) {
	r := NewStreamReader()
	first := `data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"id":"call_1","function":{"name":"get_weather","arguments":{"city":"Tokyo"}}}]}}`
	deltas := r.Feed(first)
	if len(deltas) != 1 || len(deltas[0].ToolCalls) != 1 {
		t.Fatalf("first chunk deltas: %+v (sawToolCall=%v)", deltas, r.sawToolCall)
	}
	firstDtc := deltas[0].ToolCalls[0]
	if firstDtc.Index != 0 || firstDtc.ID != "call_1" || firstDtc.Type != "function" {
		t.Fatalf("first chunk must carry id/type: %+v", firstDtc)
	}
	if firstDtc.Function == nil || firstDtc.Function.Name != "get_weather" {
		t.Fatalf("first chunk must carry name: %+v", firstDtc.Function)
	}
	if firstDtc.Function.Arguments != `{"city":"Tokyo"}` {
		t.Fatalf("first chunk arguments = %q", firstDtc.Function.Arguments)
	}

	second := `data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"id":"call_1","function":{"arguments":{"unit":"c"}}}]}}`
	deltas = r.Feed(second)
	if len(deltas) != 1 || len(deltas[0].ToolCalls) != 1 {
		t.Fatalf("second chunk deltas: %+v", deltas)
	}
	secondDtc := deltas[0].ToolCalls[0]
	if secondDtc.Index != 0 || secondDtc.ID != "" || secondDtc.Type != "" {
		t.Fatalf("follow-up chunk must omit id/type, only index+arguments: %+v", secondDtc)
	}
	if secondDtc.Function == nil || secondDtc.Function.Arguments != `{"unit":"c"}` {
		t.Fatalf("second chunk arguments = %+v", secondDtc.Function)
	}

	end := r.Finish()
	if len(end) != 1 || !end[0].HasFinish || end[0].FinishReason != "tool_calls" {
		t.Fatalf("finish for tool call: %+v", end)
	}
}

func TestBuildThirdPartyToolsAcceptsAnthropicShape(t *testing.T) {
	p := New()
	got := p.buildThirdPartyTools([]interface{}{map[string]interface{}{
		"name":         "lookup",
		"description":  "find things",
		"input_schema": map[string]interface{}{"type": "object"},
	}})
	proxy, _ := got["proxy-tools"].(map[string]interface{})
	tools, _ := proxy["tools"].([]map[string]interface{})
	if len(tools) != 1 || tools[0]["name"] != "lookup" || tools[0]["parameters"] == nil {
		t.Fatalf("third-party tools = %#v", got)
	}
}

func TestToolCallIndexSkippingCollectedInOrder(t *testing.T) {
	acc := map[int]*ToolCall{
		0: {ID: "a", Type: "function"},
		4: {ID: "e", Type: "function"},
		2: {ID: "c", Type: "function"},
	}
	got := collectToolCalls(acc)
	if len(got) != 3 {
		t.Fatalf("want 3 calls, got %d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "c" || got[2].ID != "e" {
		t.Fatalf("order by index: %+v", got)
	}
}
