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

func EstimateMessagesTokens(messages []ChatMessage) int {
	total := 0
	for _, m := range messages {
		total += EstimateTokens(ExtractText(m.Content)) + 4
	}
	return total
}
