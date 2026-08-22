package provider

import "testing"

func mkToolCall(name, args string) ToolCall {
	var tc ToolCall
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func TestEstimateCompletionTokens(t *testing.T) {
	tests := []struct {
		name string
		res  *Result
		want int
	}{
		{
			name: "nil result",
			res:  nil,
			want: 0,
		},
		{
			name: "empty result",
			res:  &Result{},
			want: 0,
		},
		{
			name: "content only",
			res:  &Result{Content: "hello world!"}, // 12 字节 -> 3
			want: EstimateTokens("hello world!"),
		},
		{
			name: "content and reasoning",
			res:  &Result{Content: "abcd", ReasoningContent: "efgh"},
			want: EstimateTokens("abcdefgh"),
		},
		{
			name: "single tool call with empty content",
			res: &Result{
				ToolCalls: []ToolCall{mkToolCall("get_weather", `{"city":"tokyo"}`)},
			},
			// 空正文估算为 0，工具调用 = EstimateTokens(name+args) + 4
			want: EstimateTokens(`get_weather{"city":"tokyo"}`) + 4,
		},
		{
			name: "content plus multiple tool calls",
			res: &Result{
				Content: "sure, let me check",
				ToolCalls: []ToolCall{
					mkToolCall("get_weather", `{"city":"tokyo"}`),
					mkToolCall("get_time", `{"tz":"JST"}`),
				},
			},
			want: EstimateTokens("sure, let me check") +
				EstimateTokens(`get_weather{"city":"tokyo"}`) + 4 +
				EstimateTokens(`get_time{"tz":"JST"}`) + 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateCompletionTokens(tt.res)
			if got != tt.want {
				t.Fatalf("EstimateCompletionTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

// 回归锁：工具调用场景下 completion 估算必须为正，防止再次退化为 0。
func TestEstimateCompletionTokensToolCallNonZero(t *testing.T) {
	res := &Result{
		ToolCalls: []ToolCall{mkToolCall("search", `{"q":"golang"}`)},
	}
	if got := EstimateCompletionTokens(res); got <= 0 {
		t.Fatalf("expected positive completion tokens for tool call, got %d", got)
	}
}
