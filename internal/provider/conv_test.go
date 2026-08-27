package provider

import (
	"encoding/json"
	"fmt"
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

func TestSafeToolResponseSummaryDoesNotCopyContent(t *testing.T) {
	content := "<template>secret source</template>\n" + strings.Repeat("x", 700)
	got := safeToolResponseSummary("SUCCESS", content)
	if got != "Tool result: SUCCESS, 735 bytes" {
		t.Fatalf("summary = %q", got)
	}
	if strings.Contains(got, "<template>") || strings.Contains(got, "secret source") {
		t.Fatalf("summary leaked tool content: %q", got)
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
	// 冷启动/未命中会话：历史折叠进单条 query（不再走 seedingMessages）。
	query := input["query"].(string)
	if _, hasSeed := input["seedingMessages"]; hasSeed {
		t.Fatalf("seedingMessages must not be used anymore: %#v", input["seedingMessages"])
	}
	if !strings.Contains(query, "[Assistant Tool Call id=call_1 name=shell]") || !strings.Contains(query, "file contents") {
		t.Fatalf("incomplete replay in folded query: %q", query)
	}
}

// #1 修复回归：首轮续聊必须命中，即便客户端回发的 assistant 轮结构与 res.Content 重构不一致
// （content 数组化 / 追加正文等）——此时 +assistant 精确前缀失配，须靠无条件存下的裸 user
// 前缀兜底命中，而不是落空降级为 conversationId=null。
func TestFirstTurnContinuationHitsDespiteAssistantDrift(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "write me a poem")}
	p.RememberConversation(1, first, &Result{ConversationID: "conv-A", Content: "Here is a poem."})

	// 客户端回发的 assistant 轮：正文被重塑（数组块 + 追加的推荐行动），与重构的
	// "Here is a poem." 不逐字相同 → [user, assistant] 前缀指纹失配。
	driftedAssistant := ChatMessage{Role: "assistant", Content: rawJSON(t,
		`[{"type":"text","text":"Here is a poem.\n\n[Recommended next actions: make it longer]"}]`)}
	followup := []ChatMessage{
		first[0],
		driftedAssistant,
		mustMsg(t, "user", "make it longer"),
	}
	if got := p.LookupConversation(1, followup); got != "conv-A" {
		t.Fatalf("first-turn continuation should fall back to bare user prefix and reuse conv-A, got %q", got)
	}
}

// #1 修复不得破坏隔离：全新单条 user 对话（无可复用历史）即使裸前缀恰好与既有会话首句相同，
// 也一律开新会话（读侧 hasReusableHistory 门槛）。
func TestBareUserPrefixDoesNotLeakToNewChat(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "shared opening")}
	p.RememberConversation(1, first, &Result{ConversationID: "conv-A", Content: "ok"})

	// 另一段全新对话，首条 user 逐字相同，但没有 assistant/tool 历史 → 必须返回空。
	fresh := []ChatMessage{mustMsg(t, "user", "shared opening")}
	if got := p.LookupConversation(1, fresh); got != "" {
		t.Fatalf("new single-user chat must not reuse conv-A via bare prefix, got %q", got)
	}
}

func TestBuildBodyCapsOversizedQueryToUpstreamLimit(t *testing.T) {
	p := New()
	body := p.buildBody(&ChatRequest{Messages: []ChatMessage{
		mustMsg(t, "system", "HEAD"+strings.Repeat("x", 50000)+"TAIL"),
		mustMsg(t, "user", "hello"),
	}}, &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "w", WorkspaceSubdomain: "sub"}, "test", 1)
	input := body["input"].(map[string]interface{})
	if _, hasSeed := input["seedingMessages"]; hasSeed {
		t.Fatalf("seedingMessages must not be used anymore")
	}
	// 上游对 input.query 有 10000 字符硬校验（实测），超限请求会被
	// INPUT_VALIDATION_ERROR 拒收。封顶必须保头（系统提示）保尾（最新一轮）。
	query := input["query"].(string)
	if n := len([]rune(query)); n > MaxUpstreamQueryRunes {
		t.Fatalf("query exceeds upstream limit: %d > %d", n, MaxUpstreamQueryRunes)
	}
	if !strings.Contains(query, "HEAD") || !strings.Contains(query, "hello") {
		t.Fatalf("capped query lost head or latest turn: %q...", query[:80])
	}
	if !strings.Contains(query, "middle context omitted") {
		t.Fatalf("capped query missing omission marker")
	}
}

func TestCapUpstreamQueryKeepsShortQueriesIntact(t *testing.T) {
	q := strings.Repeat("字", 9000)
	if got := capUpstreamQuery(q); got != q {
		t.Fatalf("short query must pass through unchanged")
	}
	long := "HEAD" + strings.Repeat("中", 20000) + "TAIL"
	got := capUpstreamQuery(long)
	if n := len([]rune(got)); n > MaxUpstreamQueryRunes {
		t.Fatalf("capped query still oversized: %d runes", n)
	}
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("cap must keep head and tail")
	}
}

func TestBuildBodyFoldsHistoricalToolResultsIntoQuery(t *testing.T) {
	p := New()
	body := p.buildBody(&ChatRequest{Messages: []ChatMessage{
		mustMsg(t, "user", "start"),
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"cat notes\"}"}}]`)},
		{Role: "tool", ToolCallID: "call_1", Content: rawText(t, "historical command output")},
		mustMsg(t, "user", "continue"),
	}}, &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "w", WorkspaceSubdomain: "sub"}, "test", 1)
	query := body["input"].(map[string]interface{})["query"].(string)
	// 历史工具结果内容应被折叠进 query（减少换号/冷启动降级损失），并带上定位标签。
	if !strings.Contains(query, "historical command output") || !strings.Contains(query, "[Tool Result id=call_1]") {
		t.Fatalf("historical tool result should be folded into query: %q", query)
	}
	// 但 assistant 工具调用的 arguments 仍不应出现（只输出工具名，不新增参数泄漏面）。
	if strings.Contains(query, "cat notes") {
		t.Fatalf("assistant tool-call arguments must not leak into folded query: %q", query)
	}
	// 且不应再退回旧的占位标记。
	if strings.Contains(query, "Previous tool result omitted") {
		t.Fatalf("tool result should be included, not omitted: %q", query)
	}
}

func TestFoldedToolResultIsTruncated(t *testing.T) {
	p := New()
	huge := strings.Repeat("A", 5000) + "MIDDLE_MARKER" + strings.Repeat("B", 5000)
	body := p.buildBody(&ChatRequest{Messages: []ChatMessage{
		mustMsg(t, "user", "start"),
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{}"}}]`)},
		{Role: "tool", ToolCallID: "call_1", Content: rawText(t, huge)},
		mustMsg(t, "user", "continue"),
	}}, &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "w", WorkspaceSubdomain: "sub"}, "test", 1)
	query := body["input"].(map[string]interface{})["query"].(string)
	// 单条历史 tool result 应被中段截断：保头保尾、丢中段标记、且总量落在单条上限附近。
	if !strings.Contains(query, strings.Repeat("A", 100)) || !strings.Contains(query, strings.Repeat("B", 100)) {
		t.Fatalf("truncation must keep head and tail of tool output")
	}
	if strings.Contains(query, "MIDDLE_MARKER") {
		t.Fatalf("middle of oversized tool output should be elided")
	}
	if !strings.Contains(query, "[truncated]") {
		t.Fatalf("truncation marker missing: %q", query[:200])
	}
}

func TestFoldedToolResultBudgetIsDynamic(t *testing.T) {
	// 单条（或空）：拿到全部总预算，尽量完整保留。
	if got := foldedToolResultBudget(0); got != FoldedToolResultTotalBudgetRunes {
		t.Fatalf("no results should yield full budget: got %d", got)
	}
	if got := foldedToolResultBudget(1); got != FoldedToolResultTotalBudgetRunes {
		t.Fatalf("single result should get full budget: got %d", got)
	}
	// 多条：公平分摊，且随条数单调递减。
	few := foldedToolResultBudget(4)
	many := foldedToolResultBudget(30)
	if few != FoldedToolResultTotalBudgetRunes/4 {
		t.Fatalf("budget should split evenly: got %d", few)
	}
	if !(few > many) {
		t.Fatalf("per-result budget must shrink as results grow: few=%d many=%d", few, many)
	}
	// 极多条：均分后不足下限时夹到 MinFoldedToolResultRunes，不会被压到不可读。
	if got := foldedToolResultBudget(100000); got != MinFoldedToolResultRunes {
		t.Fatalf("budget must clamp at floor: got %d", got)
	}
}

func TestFoldedManyToolResultsShareBudget(t *testing.T) {
	p := New()
	msgs := []ChatMessage{mustMsg(t, "user", "start")}
	// 20 条各 2000 runes 的历史工具结果：固定 1000/条时每条能全留（2000>1000 才截），
	// 动态分摊后 6000/20=300<下限 → 每条夹到 500，明显更小，从而整段落在上游上限内。
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			ChatMessage{Role: "assistant", ToolCalls: rawJSON(t, fmt.Sprintf(`[{"id":%q,"type":"function","function":{"name":"shell","arguments":"{}"}}]`, id))},
			ChatMessage{Role: "tool", ToolCallID: id, Content: rawText(t, strings.Repeat("X", 2000))},
		)
	}
	msgs = append(msgs, mustMsg(t, "user", "continue"))
	body := p.buildBody(&ChatRequest{Messages: msgs}, &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "w", WorkspaceSubdomain: "sub"}, "test", 1)
	query := body["input"].(map[string]interface{})["query"].(string)
	if n := len([]rune(query)); n > MaxUpstreamQueryRunes {
		t.Fatalf("folded query exceeds upstream cap: %d", n)
	}
	// 每条 2000 > 分摊配额 500 → 必然发生中段截断。
	if !strings.Contains(query, "[truncated]") {
		t.Fatalf("many oversized results should be truncated to share budget: %q", query[:200])
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
	entry := responses[0]
	if got := entry["toolResponseSummary"]; got != "Tool result: SUCCESS, 46 bytes" {
		t.Fatalf("safe tool summary = %v", got)
	}

	followup[2].Content = rawText(t, `{"status":"FAILED","message":"command rejected"}`)
	body = p.buildBody(&ChatRequest{Messages: followup}, &Tokens{AccessToken: "x", UserID: "u", WorkspaceID: "w"}, "test", 1)
	responses = body["input"].(map[string]interface{})["toolResponses"].([]map[string]interface{})
	if responses[0]["toolResponseStatus"] != "FAILED" || responses[0]["toolResponseFailureType"] != "UNHANDLED_ERROR" {
		t.Fatalf("native failed tool response = %#v", responses[0])
	}
}

func TestNativeToolResponseGatewayRetryDropsOnlyDuplicateRegistry(t *testing.T) {
	p := New()
	first := []ChatMessage{mustMsg(t, "user", "read file")}
	calls := []ToolCall{{ID: "toolu_1", Type: "function", GroupID: "group_1", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "Read", Arguments: `{"file_path":"/tmp/a"}`}}}
	p.rememberToolGroups(1, calls)
	p.RememberConversation(1, first, &Result{ConversationID: "conv-native-retry", ToolCalls: calls})
	followup := []ChatMessage{
		first[0],
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"toolu_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/a\"}"}}]`)},
		{Role: "tool", ToolCallID: "toolu_1", Content: rawText(t, `{"status":"SUCCESS","message":"file contents"}`)},
	}
	tools := []interface{}{map[string]interface{}{
		"type":     "function",
		"function": map[string]interface{}{"name": "Read", "parameters": map[string]interface{}{"type": "object"}},
	}}
	initial := p.buildBody(&ChatRequest{Messages: followup, Tools: tools}, &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "w", WorkspaceSubdomain: "sub"}, "test", 1)
	initialTools := initial["clientTools"].(map[string]interface{})["thirdParty"].(map[string]interface{})
	if len(initialTools) == 0 {
		t.Fatal("initial Web continuation must retain third-party tool registration")
	}
	retry := p.buildBody(&ChatRequest{Messages: followup, Tools: tools, GatewayRetry: true}, &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "w", WorkspaceSubdomain: "sub"}, "test", 1)
	retryInput := retry["input"].(map[string]interface{})
	if retryInput["chatType"] != "TOOL_RESPONSE" || retryInput["conversationId"] != "conv-native-retry" || retryInput["toolCallGroupId"] != "group_1" {
		t.Fatalf("gateway retry changed native continuation: %#v", retryInput)
	}
	retryTools := retry["clientTools"].(map[string]interface{})["thirdParty"].(map[string]interface{})
	proxy, ok := retryTools["proxy-tools"].(map[string]interface{})
	if !ok {
		t.Fatalf("gateway retry must retain compact third-party registry: %#v", retryTools)
	}
	compactTools := proxy["tools"].([]map[string]interface{})
	if len(compactTools) != 1 || compactTools[0]["name"] != "Read" {
		t.Fatalf("gateway retry lost tool name: %#v", compactTools)
	}
	if got := retry["devModeOptions"].(map[string]interface{})["autoRun"]; got != true {
		t.Fatalf("gateway retry autoRun = %v, want true", got)
	}
	responses := retryInput["toolResponses"].([]map[string]interface{})
	if responses[0]["toolResponseSummary"] != "Tool result: SUCCESS, 46 bytes" {
		t.Fatalf("gateway retry summary leaked content: %#v", responses[0]["toolResponseSummary"])
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
