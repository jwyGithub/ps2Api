package provider

import (
	"strings"
	"testing"
)

// TestSplitMessagesContextSeedReplacesTailWithSummaryInstruction:
// ContextSeed 请求走折叠路径时，tail 不再是最新 user 消息，而是摘要指令；
// 折叠历史 context 与 [User (task)] 前置渲染保持不变（复用三条契约）。
func TestSplitMessagesContextSeedReplacesTailWithSummaryInstruction(t *testing.T) {
	p := New()
	msgs := []ChatMessage{mustMsg(t, "user", "TASK_MARKER 原始任务描述")}
	for i := 0; i < 3; i++ {
		res := &Result{Content: strings.Repeat("历史结论. ", 100)}
		msgs = append(msgs, *assistantFollowup(res))
		msgs = append(msgs, mustMsg(t, "user", "第"+strings.Repeat("x", 50)+"轮指令"))
	}
	msgs = append(msgs, mustMsg(t, "user", "最新一轮消息"))

	split := p.splitMessagesSeed(msgs, "", false, true)
	q := split.Query

	if !strings.Contains(q, "TASK_MARKER") {
		t.Fatal("seed query lost folded history (task marker)")
	}
	if !strings.Contains(q, seedSummaryInstruction[:20]) {
		t.Fatalf("seed query must end with summary instruction, got: %q", q[len(q)-120:])
	}
	if strings.Contains(q, "最新一轮消息") {
		t.Fatal("seed query must NOT include the latest user message in tail")
	}
}
