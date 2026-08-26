package api

import (
	"encoding/json"
	"strings"
	"testing"

	"ps2api/internal/provider"
)

// execCmdOf 从 exec custom tool 的 input(`await tools.exec_command({...})`)里抠出 cmd 字段。
func execCmdOf(t *testing.T, input string) string {
	t.Helper()
	if !strings.Contains(input, "await tools.exec_command(") {
		t.Fatalf("exec input missing exec_command wrapper: %s", input)
	}
	// 返回值必须经 text() 吐回,否则模型收到空输出(isolate 无 console)。
	if !strings.Contains(input, "text(") {
		t.Fatalf("exec input missing text() to surface output: %s", input)
	}
	i := strings.Index(input, "{")
	j := strings.LastIndex(input, "}")
	if i < 0 || j <= i {
		t.Fatalf("exec input has no JSON object: %s", input)
	}
	var obj struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(input[i:j+1]), &obj); err != nil {
		t.Fatalf("exec input JSON parse failed: %s", input)
	}
	return obj.Cmd
}

// 非流式输出必须把 ReasoningContent 作为 reasoning 项排在 message 之前,
// 且 reasoning -> message -> function_call 的顺序正确。
func TestResponsesObjectReasoningOrder(t *testing.T) {
	res := &provider.Result{
		Success: true, ReasoningContent: "let me think", Content: "hello",
		ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function"}},
	}
	res.ToolCalls[0].Function.Name = "executeShellCommand"
	obj := responsesObject(res, "gpt-5.6-sol", "completed", false)
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
	output := responsesObject(res, "m", "completed", false)["output"].([]interface{})
	for _, it := range output {
		if it.(map[string]interface{})["type"] == "reasoning" {
			t.Fatal("must not emit reasoning item when ReasoningContent is empty")
		}
	}
}

// exec 翻译:executeShellCommand args → exec 的 JS 输入(await tools.exec_command),
// 以及 input 反向 best-effort 还原成 executeShellCommand 参数。
func TestExecInputRoundTrip(t *testing.T) {
	input, ok := execShellInput(`{"projectPath":"/tmp","command":"ls -la | wc -l"}`)
	if !ok {
		t.Fatal("execShellInput failed")
	}
	if execCmdOf(t, input) != "ls -la | wc -l" {
		t.Fatalf("exec cmd wrong: %s", input)
	}
	// workdir 应带进 JS 对象。
	if !strings.Contains(input, `"workdir":"/tmp"`) {
		t.Fatalf("workdir missing: %s", input)
	}
	if _, ok := execShellInput(`{"projectPath":"/tmp"}`); ok {
		t.Fatal("empty command must fail")
	}

	// 入站 input → executeShellCommand 参数(best-effort)
	args := execInputToArgs(`await tools.exec_command({"cmd":"echo hi","workdir":"/tmp"})`)
	var a map[string]interface{}
	if json.Unmarshal([]byte(args), &a) != nil || a["command"] != "echo hi" || a["projectPath"] != "/tmp" {
		t.Fatalf("execInputToArgs wrong: %s", args)
	}
}

// exec 探测:只有客户端声明了 type:custom name:exec 才开启 exec 翻译。
func TestCodexExecDeclared(t *testing.T) {
	yes := []map[string]interface{}{
		{"type": "function", "name": "wait"},
		{"type": "custom", "name": "exec"},
	}
	if !codexExecDeclared(yes, nil) {
		t.Fatal("exec custom tool must be detected in top-level tools")
	}
	no := []map[string]interface{}{
		{"type": "function", "name": "executeShellCommand"},
		{"type": "custom", "name": "web__run"},
	}
	if codexExecDeclared(no, nil) {
		t.Fatal("must not detect exec when absent")
	}

	// 实测形态:exec 声明藏在 input 的 additional_tools → functions → tools[] 里,顶层 tools 为空。
	nestedInput := json.RawMessage(`[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	  {"type":"additional_tools","role":"developer","tools":[
	    {"name":"functions","description":"","tools":[
	      {"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark"}},
	      {"type":"function","name":"wait"}
	    ]}
	  ]}
	]`)
	if !codexExecDeclared(nil, nestedInput) {
		t.Fatal("exec custom tool must be detected when nested in additional_tools within input")
	}

	// 负例:input 里只有 custom_tool_call 回显(type=custom_tool_call,非 custom 声明),不得误判为声明。
	echoInput := json.RawMessage(`[
	  {"type":"custom_tool_call","call_id":"c1","name":"exec","input":"await tools.exec_command({})"}
	]`)
	if codexExecDeclared(nil, echoInput) {
		t.Fatal("custom_tool_call echo must not be treated as an exec declaration")
	}
}

// 入站 custom_tool_call / custom_tool_call_output 还原成内部 executeShellCommand 往返。
func TestResponsesInboundCustomToolItems(t *testing.T) {
	input := `[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"run ls"}]},
	  {"type":"custom_tool_call","call_id":"call_x","name":"exec","input":"await tools.exec_command({\"cmd\":\"ls\"})"},
	  {"type":"custom_tool_call_output","call_id":"call_x","output":"a\nb"}
	]`
	req := responsesToOpenAI(ResponsesReq{Model: "gpt-5.6-sol", Input: json.RawMessage(input)})
	if len(req.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(req.Messages))
	}
	var calls []provider.ToolCall
	if err := json.Unmarshal(req.Messages[1].ToolCalls, &calls); err != nil || len(calls) != 1 || calls[0].Function.Name != "executeShellCommand" || calls[0].ID != "call_x" {
		t.Fatalf("custom_tool_call not mapped to executeShellCommand: %s", req.Messages[1].ToolCalls)
	}
	tm := req.Messages[2]
	if tm.Role != "tool" || tm.ToolCallID != "call_x" || provider.ExtractText(tm.Content) != "a\nb" {
		t.Fatalf("custom_tool_call_output wrong: %+v", tm)
	}
}

// execMode 开启时,responsesObject 把 executeShellCommand 发成 custom_tool_call(name:exec)。
func TestResponsesObjectExecMode(t *testing.T) {
	res := &provider.Result{Success: true, ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function"}}}
	res.ToolCalls[0].Function.Name = "executeShellCommand"
	res.ToolCalls[0].Function.Arguments = `{"command":"ls"}`
	output := responsesObject(res, "m", "completed", true)["output"].([]interface{})
	if len(output) != 1 {
		t.Fatalf("want 1 output item, got %d", len(output))
	}
	item := output[0].(map[string]interface{})
	if item["type"] != "custom_tool_call" || item["name"] != "exec" || item["call_id"] != "call_1" {
		t.Fatalf("custom_tool_call item wrong: %#v", item)
	}
	if execCmdOf(t, item["input"].(string)) != "ls" {
		t.Fatalf("exec input wrong: %v", item["input"])
	}
	// execMode 关闭时应发普通 function_call。
	off := responsesObject(res, "m", "completed", false)["output"].([]interface{})
	if off[0].(map[string]interface{})["type"] != "function_call" {
		t.Fatalf("execMode off must emit function_call: %#v", off[0])
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

// TestExecReadFileInput 锁定 readFile → exec 的 JS 输入翻译:
// offset+limit → sed 行区间;仅 limit → head;无 limit → cat;缺路径 → 失败。
// 路径带空格/单引号时必须被单引号安全包裹。
func TestExecReadFileInput(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"offset+limit", `{"filePath":"/a/b.go","limit":500,"offset":10}`, "sed -n '10,509p' '/a/b.go'"},
		{"limit only", `{"filePath":"/a/b.go","limit":40}`, "head -n 40 '/a/b.go'"},
		{"offset=1 limit", `{"filePath":"/a/b.go","limit":40,"offset":1}`, "head -n 40 '/a/b.go'"},
		{"no limit", `{"filePath":"/a/b.go"}`, "cat '/a/b.go'"},
		{"path with space+quote", `{"filePath":"/a b/it's.md"}`, `cat '/a b/it'\''s.md'`},
		{"path alias", `{"path":"/a/b.go"}`, "cat '/a/b.go'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			input, ok := execReadFileInput(c.args)
			if !ok {
				t.Fatalf("execReadFileInput(%s) failed", c.args)
			}
			if got := execCmdOf(t, input); got != c.want {
				t.Fatalf("cmd = %q, want %q", got, c.want)
			}
		})
	}
	if _, ok := execReadFileInput(`{"limit":10}`); ok {
		t.Fatal("missing filePath must fail")
	}
	// 只有 executeShellCommand / readFile 可映射,其余原生工具不走 exec。
	if !execMappable("executeShellCommand") || !execMappable("readFile") {
		t.Fatal("executeShellCommand/readFile must be mappable")
	}
	if execMappable("listCollections") {
		t.Fatal("listCollections must NOT be mappable")
	}
}
