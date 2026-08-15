package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustMsg(t *testing.T, role, text string) ChatMessage {
	t.Helper()
	raw, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return ChatMessage{Role: role, Content: raw}
}

func TestLookupConversationNewChatReturnsEmpty(t *testing.T) {
	p := New()
	msgs := []ChatMessage{mustMsg(t, "user", "hello")}
	if got := p.LookupConversation(1, msgs); got != "" {
		t.Fatalf("new chat should not reuse conversation, got %q", got)
	}
}

func TestConversationIsolationAcrossAgents(t *testing.T) {
	p := New()
	// agent A 首轮
	msgsA := []ChatMessage{mustMsg(t, "user", "write me a poem")}
	resA := &Result{ConversationID: "conv-A", Content: "Here is a poem."}
	p.RememberConversation(1, msgsA, resA)

	// agent B 新对话，同一账号
	msgsB := []ChatMessage{mustMsg(t, "user", "what is 2+2?")}
	if got := p.LookupConversation(1, msgsB); got != "" {
		t.Fatalf("agent B new chat leaked convA: %q", got)
	}
	resB := &Result{ConversationID: "conv-B", Content: "4."}
	p.RememberConversation(1, msgsB, resB)

	// agent A 继续：user + assistant + user
	contA := []ChatMessage{
		msgsA[0],
		{Role: "assistant", Content: rawText(t, "Here is a poem.")},
		mustMsg(t, "user", "make it longer"),
	}
	if got := p.LookupConversation(1, contA); got != "conv-A" {
		t.Fatalf("agent A continuation should reuse conv-A, got %q", got)
	}

	// agent B 继续
	contB := []ChatMessage{
		msgsB[0],
		{Role: "assistant", Content: rawText(t, "4.")},
		mustMsg(t, "user", "explain"),
	}
	if got := p.LookupConversation(1, contB); got != "conv-B" {
		t.Fatalf("agent B continuation should reuse conv-B, got %q", got)
	}
}

func TestToolResultContinuationKeepsConversation(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "check weather")}
	res := &Result{ConversationID: "conv-tool", Content: ""}
	res.ToolCalls = []ToolCall{{ID: "call_1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "get_weather", Arguments: "{}"}}}
	p.RememberConversation(1, first, res)

	followup := []ChatMessage{
		first[0],
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]`)},
		{Role: "tool", ToolCallID: "call_1", Content: rawText(t, `{"temp":20}`)},
	}
	if got := p.LookupConversation(1, followup); got != "conv-tool" {
		t.Fatalf("tool followup should reuse conv-tool, got %q", got)
	}
}

func TestResetConversationClearsAllKeys(t *testing.T) {
	p := New()
	msgs := []ChatMessage{mustMsg(t, "user", "hello")}
	p.RememberConversation(1, msgs, &Result{ConversationID: "conv-A", Content: "hi"})
	p.RememberConversation(2, msgs, &Result{ConversationID: "conv-B", Content: "hi"})
	cont := []ChatMessage{
		msgs[0],
		{Role: "assistant", Content: rawText(t, "hi")},
		mustMsg(t, "user", "again"),
	}
	p.ResetConversation(1)
	if got := p.LookupConversation(1, cont); got != "" {
		t.Fatalf("account 1 should be reset, got %q", got)
	}
	if got := p.LookupConversation(2, cont); got != "conv-B" {
		t.Fatalf("account 2 should keep conv-B, got %q", got)
	}
}

func rawText(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bodyConversationID(t *testing.T, p *Provider, accID int64, msgs []ChatMessage) string {
	t.Helper()
	tok := &Tokens{AccessToken: "x", UserID: "u1", WorkspaceID: "w1"}
	body := p.buildBody(&ChatRequest{Messages: msgs}, tok, "gpt-test", accID)
	input, _ := body["input"].(map[string]interface{})
	if input == nil {
		t.Fatal("body has no input")
	}
	id, _ := input["conversationId"].(string)
	return id
}

// 端到端：同一账号两个 agent 交错对话，新对话的 HTTP 请求体不得携带旧的 conversationId。
func TestBuildBodyConversationIsolationEndToEnd(t *testing.T) {
	p := New()

	// Agent A 首轮
	msgsA1 := []ChatMessage{mustMsg(t, "user", "write a poem")}
	if got := bodyConversationID(t, p, 1, msgsA1); got != "" {
		t.Fatalf("A first turn must have empty conversationId, got %q", got)
	}
	resA := &Result{ConversationID: "conv-A", Content: "rose are red"}
	p.RememberConversation(1, msgsA1, resA)

	// Agent B 新对话（同一账号）——修复前这里会拿到 conv-A
	msgsB1 := []ChatMessage{mustMsg(t, "user", "what is 2+2?")}
	if got := bodyConversationID(t, p, 1, msgsB1); got != "" {
		t.Fatalf("B new chat must NOT carry conv-A, got %q", got)
	}
	p.RememberConversation(1, msgsB1, &Result{ConversationID: "conv-B", Content: "4"})

	// Agent A 续聊要能拿回 conv-A
	msgsA2 := []ChatMessage{
		msgsA1[0],
		{Role: "assistant", Content: rawText(t, "rose are red")},
		mustMsg(t, "user", "make it longer"),
	}
	if got := bodyConversationID(t, p, 1, msgsA2); got != "conv-A" {
		t.Fatalf("A continuation should carry conv-A, got %q", got)
	}

	// Agent B 续聊要能拿回 conv-B
	msgsB2 := []ChatMessage{
		msgsB1[0],
		{Role: "assistant", Content: rawText(t, "4")},
		mustMsg(t, "user", "explain"),
	}
	if got := bodyConversationID(t, p, 1, msgsB2); got != "conv-B" {
		t.Fatalf("B continuation should carry conv-B, got %q", got)
	}

	// 第三个全新对话永不复用
	msgsC1 := []ChatMessage{mustMsg(t, "user", "hi")}
	if got := bodyConversationID(t, p, 1, msgsC1); got != "" {
		t.Fatalf("C new chat must be empty, got %q", got)
	}
}

func rawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("invalid json: %s", s)
	}
	return json.RawMessage(s)
}

func TestStickyAccountFollowsConversationOwner(t *testing.T) {
	p := New()
	msgsA := []ChatMessage{mustMsg(t, "user", "hello agent A")}
	p.RememberConversation(1, msgsA, &Result{ConversationID: "conv-A", Content: "hi A"})

	if id, ok := p.StickyAccount(msgsA); ok {
		t.Fatalf("new chat must not be sticky, got account %d", id)
	}
	contA := []ChatMessage{
		msgsA[0],
		{Role: "assistant", Content: rawText(t, "hi A")},
		mustMsg(t, "user", "more"),
	}
	if id, ok := p.StickyAccount(contA); !ok || id != 1 {
		t.Fatalf("continuation of A should stick to account 1, got %d/%v", id, ok)
	}

	msgsB := []ChatMessage{mustMsg(t, "user", "hello agent B")}
	p.RememberConversation(2, msgsB, &Result{ConversationID: "conv-B", Content: "hi B"})
	contB := []ChatMessage{
		msgsB[0],
		{Role: "assistant", Content: rawText(t, "hi B")},
		mustMsg(t, "user", "more B"),
	}
	if id, ok := p.StickyAccount(contB); !ok || id != 2 {
		t.Fatalf("continuation of B should stick to account 2, got %d/%v", id, ok)
	}
	if id, ok := p.StickyAccount(contA); !ok || id != 1 {
		t.Fatalf("A still sticks to account 1 after B, got %d/%v", id, ok)
	}
}

func TestStickyAccountToolFollowup(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "run tool")}
	res := &Result{ConversationID: "conv-T", Content: ""}
	res.ToolCalls = []ToolCall{{ID: "c1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "do_it", Arguments: "{}"}}}
	p.RememberConversation(5, first, res)

	followup := []ChatMessage{
		first[0],
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"c1","type":"function","function":{"name":"do_it","arguments":"{}"}}]`)},
		{Role: "tool", ToolCallID: "c1", Content: rawText(t, "done")},
	}
	if id, ok := p.StickyAccount(followup); !ok || id != 5 {
		t.Fatalf("tool followup should stick to account 5, got %d/%v", id, ok)
	}
}

func TestToolResultReplaysCompleteHistoryWithoutPendingConversation(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "read file")}
	res := &Result{ConversationID: "conv-tool"}
	res.ToolCalls = []ToolCall{{ID: "call_1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "shell", Arguments: `{"command":"cat file"}`}}}
	p.RememberConversation(1, first, res)
	followup := []ChatMessage{
		first[0],
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"cat file\"}"}}]`)},
		{Role: "tool", ToolCallID: "call_1", Content: rawText(t, "file contents")},
	}
	body := p.buildBody(&ChatRequest{Messages: followup}, &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}, "test", 1)
	input := body["input"].(map[string]interface{})
	if input["conversationId"] != nil {
		t.Fatalf("tool result must not reuse pending conversation: %#v", input["conversationId"])
	}
	seed := input["seedingMessages"].([]map[string]string)[0]["content"]
	if !strings.Contains(seed, "[Assistant Tool Call id=call_1 name=shell]") || !strings.Contains(input["query"].(string), "file contents") {
		t.Fatalf("incomplete replay: seed=%q query=%q", seed, input["query"])
	}
}

func TestToolResultWithTrailingTokenMetadataReplaysHistory(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "read file")}
	res := &Result{ConversationID: "conv-anthropic-tool"}
	res.ToolCalls = []ToolCall{{ID: "toolu_1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "Read", Arguments: `{"file_path":"/tmp/a"}`}}}
	p.RememberConversation(1, first, res)
	followup := []ChatMessage{
		first[0],
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"toolu_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/a\"}"}}]`)},
		{Role: "user", Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"file contents"}]`)},
		{Role: "system", Content: rawText(t, "<total_tokens>14999302 tokens left</total_tokens>")},
	}
	body := p.buildBody(&ChatRequest{Messages: followup}, &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}, "test", 1)
	input := body["input"].(map[string]interface{})
	if input["conversationId"] != nil {
		t.Fatalf("tool result followed by token metadata must not reuse pending conversation: %#v", input["conversationId"])
	}
	query := input["query"].(string)
	if !strings.Contains(query, "file contents") || strings.Contains(query, "<total_tokens>") {
		t.Fatalf("bad tool result query: %q", query)
	}
}

func TestBuildBodyDoesNotInjectTextToolProtocol(t *testing.T) {
	p := New()
	body := p.buildBody(&ChatRequest{
		Messages: []ChatMessage{mustMsg(t, "user", "read file")},
		Tools: []interface{}{map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Read",
				"description": "Read a file",
				"parameters":  map[string]interface{}{"type": "object"},
			},
		}},
	}, &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}, "test", 1)
	input := body["input"].(map[string]interface{})
	if strings.Contains(input["query"].(string), "<tool_call>") {
		t.Fatalf("request must rely on registered native tools, query=%q", input["query"])
	}
	thirdParty := body["clientTools"].(map[string]interface{})["thirdParty"].(map[string]interface{})
	proxyTools := thirdParty["proxy-tools"].(map[string]interface{})["tools"].([]map[string]interface{})
	if len(proxyTools) != 1 || proxyTools[0]["name"] != "Read" {
		t.Fatalf("native tool registration missing: %#v", proxyTools)
	}
}

func TestBuildBodyUsesNativeToolResponse(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "read file")}
	res := &Result{ConversationID: "conv-native-tool"}
	res.ToolCalls = []ToolCall{{ID: "toolu_1", Type: "function", GroupID: "group_1", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "Read", Arguments: `{"file_path":"/tmp/a"}`}}}
	p.rememberToolGroups(1, res.ToolCalls)
	p.RememberConversation(1, first, res)
	followup := []ChatMessage{
		first[0],
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"toolu_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/a\"}"}}]`)},
		{Role: "tool", ToolCallID: "toolu_1", Content: rawText(t, `{"status":"SUCCESS","message":"file contents"}`)},
	}
	body := p.buildBody(&ChatRequest{Messages: followup}, &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}, "test", 1)
	input := body["input"].(map[string]interface{})
	if input["chatType"] != "TOOL_RESPONSE" || input["conversationId"] != "conv-native-tool" || input["toolCallGroupId"] != "group_1" {
		t.Fatalf("native tool response input = %#v", input)
	}
	responses := input["toolResponses"].([]map[string]interface{})
	if len(responses) != 1 || responses[0]["toolCallId"] != "toolu_1" || responses[0]["toolResponseStatus"] != "SUCCESS" {
		t.Fatalf("native tool responses = %#v", responses)
	}
	if _, exists := input["seedingMessages"]; exists {
		t.Fatalf("native tool response must not replay history: %#v", input)
	}

	followup[2].Content = rawText(t, `{"status":"FAILED","message":"command rejected"}`)
	body = p.buildBody(&ChatRequest{Messages: followup}, &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}, "test", 1)
	responses = body["input"].(map[string]interface{})["toolResponses"].([]map[string]interface{})
	if responses[0]["toolResponseStatus"] != "FAILED" || responses[0]["toolResponseFailureType"] != "UNHANDLED_ERROR" {
		t.Fatalf("native failed tool response = %#v", responses[0])
	}
}

func TestBuildBodyUsesNativeAnthropicToolResponseGroup(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "inspect files")}
	calls := []ToolCall{
		{ID: "toolu_1", Type: "function", GroupID: "group_1", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "Read", Arguments: `{}`}},
		{ID: "toolu_2", Type: "function", GroupID: "group_1", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "Read", Arguments: `{}`}},
	}
	p.rememberToolGroups(1, calls)
	p.RememberConversation(1, first, &Result{ConversationID: "conv-anthropic-native", ToolCalls: calls})
	followup := []ChatMessage{
		first[0],
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"toolu_1","type":"function","function":{"name":"Read","arguments":"{}"}},{"id":"toolu_2","type":"function","function":{"name":"Read","arguments":"{}"}}]`)},
		{Role: "user", Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"one"},{"type":"tool_result","tool_use_id":"toolu_2","content":"two"}]`)},
		{Role: "system", Content: rawText(t, "<total_tokens>14999302 tokens left</total_tokens>")},
	}
	body := p.buildBody(&ChatRequest{Messages: followup}, &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}, "test", 1)
	input := body["input"].(map[string]interface{})
	responses := input["toolResponses"].([]map[string]interface{})
	if input["chatType"] != "TOOL_RESPONSE" || len(responses) != 2 || responses[0]["toolCallId"] != "toolu_1" || responses[1]["toolCallId"] != "toolu_2" {
		t.Fatalf("anthropic native tool response = %#v", input)
	}
}

func TestStickyAccountAfterReset(t *testing.T) {
	p := New()
	msgs := []ChatMessage{mustMsg(t, "user", "hi")}
	p.RememberConversation(3, msgs, &Result{ConversationID: "conv-3", Content: "hey"})
	p.ResetConversation(3)
	cont := []ChatMessage{
		msgs[0],
		{Role: "assistant", Content: rawText(t, "hey")},
		mustMsg(t, "user", "you there?"),
	}
	if id, ok := p.StickyAccount(cont); ok {
		t.Fatalf("after ResetConversation account 3 must not be sticky, got %d", id)
	}
}
