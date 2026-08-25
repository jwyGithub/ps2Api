package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
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

func TestUsageAndRateLimitMetadata(t *testing.T) {
	r := NewStreamReader()
	r.Feed(`data: {"eventType":"usage","data":{"userType":"FREE_USER","usageState":"AVAILABLE","limit":50000,"usage":28713,"overage":0,"spillage":2,"allowOverage":false,"warningThresholds":[{"value":50,"unit":"Percentage"}],"usageCycle":{"start":"2026-08-11T23:22:55Z","end":"2026-09-11T23:22:55Z"},"isTeamPooled":true}}`)
	if r.Usage == nil || r.Usage.Spillage != 2 || r.Usage.UsageCycle == nil || r.Usage.UsageCycle.End.Format(time.RFC3339) != "2026-09-11T23:22:55Z" || !r.Usage.IsTeamPooled {
		t.Fatalf("usage metadata = %+v", r.Usage)
	}

	now := time.Date(2026, 8, 15, 11, 10, 58, 0, time.UTC)
	headers := http.Header{"X-Ratelimit-Limit": {"30"}, "X-Ratelimit-Remaining": {"29"}, "X-Ratelimit-Reset": {"1786792317000"}, "Ratelimit-Policy": {"30;w=60"}}
	rate := parseRateLimit(headers, now)
	if rate == nil || rate.Limit != 30 || rate.Remaining != 29 || rate.WindowSeconds != 60 || rate.ResetAt == nil || rate.ResetAt.UnixMilli() != 1786792317000 {
		t.Fatalf("rate limit metadata = %+v", rate)
	}
}

func TestInputValidationFailureIsRequestRejected(t *testing.T) {
	r := NewStreamReader()
	r.Feed(`data: {"eventType":"failure","data":{"errorType":"INPUT_VALIDATION_ERROR","userMessage":"Forbidden"}}`)
	if r.Err != "Forbidden" || !r.RequestRejected {
		t.Fatalf("input validation failure = err %q, requestRejected %v", r.Err, r.RequestRejected)
	}
}

// 上游自己调模型失败（Postman → Bedrock）：userMessage 只是给终端用户的套话，真正的根因
// 在 message 里。必须 (a) 标记 UpstreamFailure 让 router 别把账号打成 error、续聊也别换号，
// (b) 把 message 细节拼进错误串，否则日志/告警里只剩 "That was unexpected :(" 无从排查。
// 报文取自线上 trace（LLM_STREAM_ERROR / AI_APICallError: Policy Error）。
func TestUpstreamModelFailureIsFlaggedAndKeepsRootCause(t *testing.T) {
	r := NewStreamReader()
	r.Feed(`data: {"eventType":"failure","data":{"errorType":"LLM_STREAM_ERROR","message":"LLM stream error: Failed after 3 attempts. Last error: AI_APICallError: Policy Error","userMessage":"That was unexpected :(. Try starting a new chat, or remove any configured MCP servers."}}`)
	if !r.UpstreamFailure {
		t.Fatalf("LLM_STREAM_ERROR must be flagged as an upstream model failure, err=%q", r.Err)
	}
	if r.RequestRejected || r.QuotaExceeded {
		t.Fatalf("upstream model failure is neither a rejected request nor a quota problem: rejected=%v quota=%v", r.RequestRejected, r.QuotaExceeded)
	}
	if !strings.Contains(r.Err, "Policy Error") {
		t.Fatalf("root cause from `message` must survive into the error string, got %q", r.Err)
	}
	if !strings.Contains(r.Err, "That was unexpected") {
		t.Fatalf("user-facing text should still be present, got %q", r.Err)
	}
}

// 其他 errorType 不得被误判成上游模型故障——否则真正坏掉的账号会被一直留在池子里重试。
func TestNonUpstreamFailureTypesAreNotFlagged(t *testing.T) {
	for _, errorType := range []string{"INPUT_VALIDATION_ERROR", "USAGE_LIMIT_EXCEEDED", "SOMETHING_ELSE", ""} {
		r := NewStreamReader()
		r.Feed(`data: {"eventType":"failure","data":{"errorType":"` + errorType + `","userMessage":"x"}}`)
		if r.UpstreamFailure {
			t.Fatalf("errorType %q must not be treated as an upstream model failure", errorType)
		}
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
	first := `data: {"eventType":"toolCallChunk","data":{"toolCalls":[{"id":"call_1","toolCallGroupId":"group_1","function":{"name":"get_weather","arguments":{"city":"Tokyo"}}}]}}`
	deltas := r.Feed(first)
	if len(deltas) != 1 || len(deltas[0].ToolCalls) != 1 {
		t.Fatalf("first chunk deltas: %+v (sawToolCall=%v)", deltas, r.sawToolCall)
	}
	firstDtc := deltas[0].ToolCalls[0]
	if firstDtc.Index != 0 || firstDtc.ID != "call_1" || firstDtc.Type != "function" {
		t.Fatalf("first chunk must carry id/type: %+v", firstDtc)
	}
	if firstDtc.GroupID != "group_1" {
		t.Fatalf("tool call group = %q", firstDtc.GroupID)
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
