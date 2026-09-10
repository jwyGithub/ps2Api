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
	// INPUT_VALIDATION_ERROR 拒收。折叠路径现在对 system 消息截预算（保头保尾、
	// 中段省略，见 FoldedSystemBudgetRunes），50K 的 system 不再需要落到
	// capUpstreamQuery 的兜底截断，原始内容也无中段 omit 标记。
	query := input["query"].(string)
	if n := len([]rune(query)); n > MaxUpstreamQueryRunes {
		t.Fatalf("query exceeds upstream limit: %d > %d", n, MaxUpstreamQueryRunes)
	}
	if !strings.Contains(query, "HEAD") || !strings.Contains(query, "TAIL") {
		t.Fatalf("folded system must keep head and tail")
	}
	if !strings.Contains(query, "hello") {
		t.Fatalf("capped query lost latest turn: %q...", query[:80])
	}
	if strings.Contains(query, strings.Repeat("x", 1000)) {
		t.Fatalf("oversized system must be middle-truncated at fold time, not rendered in full")
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

// TestFingerprintIgnoresClientCutoffNotice 复现 2026-09-10 线上事故缺陷1：客户端流被切断后
// 自动重试时，往本轮 user 消息里注入一次性续写提示块（"Your response above was cut off
// mid-stream. ..."）——下一轮同一消息里该块即消失（system-reminder 块已被 stableFingerprintText
// 剥掉，这个纯文本块没有）。计入指纹 → 该轮指纹带毒 → 下一轮前缀匹配必失配。修复：指纹
// 计算按块跳过该提示，带毒轮回存的会话干净轮仍能命中。
func TestFingerprintIgnoresClientCutoffNotice(t *testing.T) {
	const toolResult = `[{"type":"tool_result","tool_use_id":"toolu_1","content":"res"}]`
	withNotice := ChatMessage{Role: "user", Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":"res"},
		{"type":"text","text":"<system-reminder>\n<total_tokens>1 tokens left</total_tokens>\n</system-reminder>"},
		{"type":"text","text":"Your response above was cut off mid-stream. Resume directly from where it stops — no apology, no recap. If none of it survived, answer the request from the start."}]`)}
	clean := ChatMessage{Role: "user", Content: rawJSON(t, toolResult)}
	polluted := []ChatMessage{mustMsg(t, "user", "task"), mustMsg(t, "assistant", "ok"), withNotice}
	cleaned := []ChatMessage{mustMsg(t, "user", "task"), mustMsg(t, "assistant", "ok"), clean}
	if conversationFingerprint(polluted) != conversationFingerprint(cleaned) {
		t.Fatalf("cutoff notice must not change the conversation fingerprint")
	}
	p := New()
	p.setConversationID(1, polluted, "conv-x")
	// 前缀游走只查严格前缀，下一轮历史 = 干净版 + 后续消息，其前缀即带毒轮的完整列表。
	followup := append(append([]ChatMessage{}, cleaned...), mustMsg(t, "user", "next"))
	if got := p.LookupConversation(1, followup); got != "conv-x" {
		t.Fatalf("clean follow-up must still hit the conversation stored from the polluted turn, got %q", got)
	}
}

// TestFoldedReplayKeepsOriginalTaskUnderOversizedHistory 复现 2026-09-10 线上事故形态：
// 会话粘性断裂后每轮降级为全历史重放，42K 的 system 消息在折叠时不设预算全量渲染，
// 把原始任务（首条 user 消息）挤进 capUpstreamQuery 的「中段省略」区——模型只见系统
// 提示开头与最近的 tool results，于是回复「没有收到实际任务请求」。
// 修复契约：重放折叠时 system 截预算、原始任务后置渲染（紧贴最新一轮，永远落在尾部
// 保留区）、待处理 tool-tail 也截预算（防单条巨结果吃光尾部窗口）。
func TestFoldedReplayKeepsOriginalTaskUnderOversizedHistory(t *testing.T) {
	p := New()
	sys := "SYS_HEAD " + strings.Repeat("s", 42000) + " SYS_TAIL"
	sdk := strings.Repeat("d", 6500)
	task := "TASK_MARKER 分析 openapi 调用返回 data 为 null 的原因"
	userBlocks, err := json.Marshal([]map[string]interface{}{
		{"type": "text", "text": sdk},
		{"type": "text", "text": task},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldRes, _ := json.Marshal([]map[string]interface{}{
		{"type": "tool_result", "tool_use_id": "toolu_1", "content": strings.Repeat("r", 10000)},
	})
	tailRes, _ := json.Marshal([]map[string]interface{}{
		{"type": "tool_result", "tool_use_id": "toolu_2", "content": strings.Repeat("t", 26000)},
	})
	msgs := []ChatMessage{
		{Role: "user", Content: userBlocks},
		mustMsg(t, "system", sys),
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"toolu_1","type":"function","function":{"name":"Read","arguments":"{}"}}]`)},
		{Role: "user", Content: oldRes},
		{Role: "assistant", ToolCalls: rawJSON(t, `[{"id":"toolu_2","type":"function","function":{"name":"Read","arguments":"{}"}}]`)},
		{Role: "user", Content: tailRes},
	}
	body := p.buildBody(&ChatRequest{Messages: msgs}, &Tokens{PostmanSID: "sid", UserID: "u", WorkspaceID: "w", WorkspaceSubdomain: "sub"}, "test", 1)
	query := body["input"].(map[string]interface{})["query"].(string)
	if n := len([]rune(query)); n > MaxUpstreamQueryRunes {
		t.Fatalf("query exceeds upstream limit: %d > %d", n, MaxUpstreamQueryRunes)
	}
	// 核心回归断言：原始任务必须活在出站 query 里。
	if !strings.Contains(query, "TASK_MARKER") {
		t.Fatalf("folded replay lost the original task: %q...", query[:200])
	}
	if !strings.Contains(query, "[User (original task)]") {
		t.Fatalf("original task should be rendered with its own label: %q...", query[:200])
	}
	// system 消息截预算：保留头尾、省略中段。
	if !strings.Contains(query, "SYS_HEAD") || !strings.Contains(query, "SYS_TAIL") {
		t.Fatalf("folded system must keep head and tail")
	}
	if strings.Contains(query, strings.Repeat("s", 1000)) {
		t.Fatalf("folded system must be middle-truncated, not rendered in full")
	}
	// 26K 的待处理 tool-tail 截预算：不把尾部保留区整个吃掉。阈值取 2000：预算 4000 的
	// 保头段本身约 1990 连续字符，再低会误伤合法的保头段。
	if strings.Contains(query, strings.Repeat("t", 2000)) {
		t.Fatalf("oversized pending tool result must be middle-truncated in replay mode")
	}
	if !strings.Contains(query, "Tool Result id=toolu_2") {
		t.Fatalf("pending tool result should still be labeled in replay query")
	}
}

// TestInvalidateConversationKeepsStickyOwner 验证会话失效的株连范围：
// 死会话只删 (账号,指纹)→conversationId 映射，保留 指纹→账号 的粘性归属——
// 会话损坏丢的是服务端上下文，不该连带换号。下一轮仍粘回原账号重放一轮、
// 回存干净指纹后即恢复增量模式（一轮自愈）。
func TestInvalidateConversationKeepsStickyOwner(t *testing.T) {
	p := New()
	turn1 := []ChatMessage{mustMsg(t, "user", "task"), mustMsg(t, "assistant", "ok")}
	p.setConversationID(1, turn1, "conv-1")
	// 粘性查找走前缀游走（fp(messages[:i])），命中需要下一轮历史把 turn1 作为前缀。
	next := append(append([]ChatMessage{}, turn1...), mustMsg(t, "user", "continue"))
	if owner, ok := p.StickyAccount(next); !ok || owner != 1 {
		t.Fatalf("sticky owner should be 1, got %d ok=%v", owner, ok)
	}
	p.InvalidateConversation(1, next)
	if got := p.LookupConversation(1, next); got != "" {
		t.Fatalf("dead conversation must not be reused, got %q", got)
	}
	if owner, ok := p.StickyAccount(next); !ok || owner != 1 {
		t.Fatalf("sticky owner must survive conversation invalidation, got %d ok=%v", owner, ok)
	}
	// 同账号重放成功后回存新会话：下一轮即恢复命中。
	p.setConversationID(1, turn1, "conv-2")
	if got := p.LookupConversation(1, next); got != "conv-2" {
		t.Fatalf("replay on the sticky account should re-bind the conversation, got %q", got)
	}
}
