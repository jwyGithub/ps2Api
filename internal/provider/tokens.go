package provider

// ---------- token 估算 ----------

func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	n := (len(text) + 3) / 4
	if n < 1 {
		return 1
	}
	return n
}

// EstimateCompletionTokens 估算助手输出的 token，涵盖正文、思考内容以及
// 工具调用的函数名+参数（上游 Postman 不返回 output_tokens，只能本地估算）。
func EstimateCompletionTokens(res *Result) int {
	if res == nil {
		return 0
	}
	total := EstimateTokens(res.Content + res.ReasoningContent)
	for _, tc := range res.ToolCalls {
		total += EstimateTokens(tc.Function.Name+tc.Function.Arguments) + 4
	}
	return total
}

func EstimateMessagesTokens(messages []ChatMessage) int {
	total := 0
	for _, m := range messages {
		total += EstimateTokens(ExtractText(m.Content)) + 4
	}
	return total
}
