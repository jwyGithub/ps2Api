package api

import (
	"encoding/json"
	"testing"

	"ps2api/internal/provider"
)

// 非流式输出必须把 ReasoningContent 作为 reasoning 项排在 message 之前,
// 且 reasoning -> message -> function_call 的顺序正确。
func TestResponsesObjectReasoningOrder(t *testing.T) {
	res := &provider.Result{
		Success: true, ReasoningContent: "let me think", Content: "hello",
		ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function"}},
	}
	res.ToolCalls[0].Function.Name = "executeShellCommand"
	obj := responsesObject(res, "gpt-5.6-sol", "completed")
	output := obj["output"].([]interface{})
	if len(output) != 3 {
		t.Fatalf("want 3 output items (reasoning/message/function_call), got %d", len(output))
	}
	types := make([]string, 3)
	for i, it := range output {
		types[i] = it.(map[string]interface{})["type"].(string)
	}
	if types[0] != "reasoning" || types[1] != "message" || types[2] != "function_call" {
		t.Fatalf("output order wrong: %v", types)
	}
	summary := output[0].(map[string]interface{})["summary"].([]interface{})
	if len(summary) != 1 || summary[0].(map[string]interface{})["text"] != "let me think" {
		t.Fatalf("reasoning summary wrong: %v", summary)
	}
}

// 无思考内容时不应产出空的 reasoning 项。
func TestResponsesObjectNoReasoning(t *testing.T) {
	res := &provider.Result{Success: true, Content: "hi"}
	output := responsesObject(res, "m", "completed")["output"].([]interface{})
	for _, it := range output {
		if it.(map[string]interface{})["type"] == "reasoning" {
			t.Fatal("must not emit reasoning item when ReasoningContent is empty")
		}
	}
}

// local_shell 翻译:executeShellCommand args → local_shell_call action(bash -lc 包裹),
// 以及 local_shell_call / local_shell_call_output 入站还原成内部 executeShellCommand 往返。
func TestLocalShellActionRoundTrip(t *testing.T) {
	action, ok := toLocalShellAction(`{"projectPath":"/tmp","command":"ls -la | wc -l","blockUntilMs":30000}`)
	if !ok {
		t.Fatal("toLocalShellAction failed")
	}
	cmd, _ := action["command"].([]string)
	if len(cmd) != 3 || cmd[0] != "bash" || cmd[1] != "-lc" || cmd[2] != "ls -la | wc -l" {
		t.Fatalf("command wrong: %#v", cmd)
	}
	if action["working_directory"] != "/tmp" || action["timeout_ms"] != 30000 {
		t.Fatalf("wd/timeout wrong: %#v", action)
	}
	if _, ok := toLocalShellAction(`{"projectPath":"/tmp"}`); ok {
		t.Fatal("empty command must fail")
	}

	// 入站 action → executeShellCommand 参数
	args := localShellActionToArgs(json.RawMessage(`{"command":["bash","-lc","echo hi"],"working_directory":"/tmp","timeout_ms":5000}`))
	var a map[string]interface{}
	if json.Unmarshal([]byte(args), &a) != nil || a["command"] != "echo hi" || a["projectPath"] != "/tmp" {
		t.Fatalf("localShellActionToArgs wrong: %s", args)
	}
}

func TestResponsesInboundLocalShellItems(t *testing.T) {
	input := `[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"run ls"}]},
	  {"type":"local_shell_call","call_id":"call_x","action":{"command":["bash","-lc","ls"],"working_directory":"/tmp"}},
	  {"type":"local_shell_call_output","call_id":"call_x","output":"a\nb"}
	]`
	req := responsesToOpenAI(ResponsesReq{Model: "gpt-5.6-sol", Input: json.RawMessage(input)})
	if len(req.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(req.Messages))
	}
	var calls []provider.ToolCall
	if err := json.Unmarshal(req.Messages[1].ToolCalls, &calls); err != nil || len(calls) != 1 || calls[0].Function.Name != "executeShellCommand" || calls[0].ID != "call_x" {
		t.Fatalf("local_shell_call not mapped to executeShellCommand: %s", req.Messages[1].ToolCalls)
	}
	tm := req.Messages[2]
	if tm.Role != "tool" || tm.ToolCallID != "call_x" || provider.ExtractText(tm.Content) != "a\nb" {
		t.Fatalf("local_shell_call_output wrong: %+v", tm)
	}
}

func TestResponsesToOpenAIStringInput(t *testing.T) {
	rr := ResponsesReq{Model: "gpt-5.6-sol", Instructions: "be terse", Input: json.RawMessage(`"hello"`)}
	req := responsesToOpenAI(rr)
	if len(req.Messages) != 2 {
		t.Fatalf("want system+user, got %d messages", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || provider.ExtractText(req.Messages[0].Content) != "be terse" {
		t.Fatalf("system message wrong: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || provider.ExtractText(req.Messages[1].Content) != "hello" {
		t.Fatalf("user message wrong: %+v", req.Messages[1])
	}
}

// 工具往返是整件事的关键:function_call_output 必须变成 role:"tool" + tool_call_id,
// 这样网关的 nativeToolResponse 才能按 call_id 找到 groupID 回传。
func TestResponsesToOpenAIToolRoundTrip(t *testing.T) {
	input := `[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"run ls"}]},
	  {"type":"function_call","call_id":"call_abc","name":"executeShellCommand","arguments":"{\"command\":\"ls\"}"},
	  {"type":"function_call_output","call_id":"call_abc","output":"total 0"}
	]`
	rr := ResponsesReq{Model: "gpt-5.6-sol", Input: json.RawMessage(input)}
	req := responsesToOpenAI(rr)
	if len(req.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "user" || provider.ExtractText(req.Messages[0].Content) != "run ls" {
		t.Fatalf("user msg wrong: %+v", req.Messages[0])
	}
	// assistant function_call -> ToolCalls
	var calls []provider.ToolCall
	if err := json.Unmarshal(req.Messages[1].ToolCalls, &calls); err != nil || len(calls) != 1 {
		t.Fatalf("assistant tool_calls wrong: %s", req.Messages[1].ToolCalls)
	}
	if calls[0].ID != "call_abc" || calls[0].Function.Name != "executeShellCommand" {
		t.Fatalf("tool call fields wrong: %+v", calls[0])
	}
	// function_call_output -> tool message keyed by call_id
	tm := req.Messages[2]
	if tm.Role != "tool" || tm.ToolCallID != "call_abc" || provider.ExtractText(tm.Content) != "total 0" {
		t.Fatalf("tool result message wrong: %+v", tm)
	}
}

// Responses 的扁平 function 工具应被内部 extractToolName 直接识别(无需转 nested)。
func TestResponsesToolsFlatShapeRecognized(t *testing.T) {
	rr := ResponsesReq{
		Model: "gpt-5.6-sol",
		Input: json.RawMessage(`"hi"`),
		Tools: []map[string]interface{}{
			{"type": "function", "name": "executeShellCommand", "parameters": map[string]interface{}{"type": "object"}},
		},
	}
	req := responsesToOpenAI(rr)
	if len(req.Tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(req.Tools))
	}
}
