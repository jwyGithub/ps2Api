package provider

import (
	"strings"
	"testing"
)

func TestSplitQueryIntoChunks_ShortReturnsSingle(t *testing.T) {
	got := splitQueryIntoChunks("hello world", 100)
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("short query should stay single, got %d chunks: %q", len(got), got)
	}
}

func TestSplitQueryIntoChunks_RespectsBudgetAndReassembles(t *testing.T) {
	// 20 段、每段 50 字符，用段落分隔，总长远超预算。
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString(strings.Repeat("x", 50))
		b.WriteString("\n\n")
	}
	full := b.String()
	budget := 120
	chunks := splitQueryIntoChunks(full, budget)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > budget {
			t.Fatalf("chunk %d exceeds budget: %d > %d", i, n, budget)
		}
	}
	// 分片必须无损可拼回原文（不丢字符、不重复）。
	if strings.Join(chunks, "") != full {
		t.Fatalf("chunks do not reassemble into original query")
	}
}

func TestSplitQueryIntoChunks_NoNewlineHardCuts(t *testing.T) {
	full := strings.Repeat("a", 500) // 无换行
	budget := 100
	chunks := splitQueryIntoChunks(full, budget)
	if len(chunks) != 5 {
		t.Fatalf("expected 5 hard-cut chunks, got %d", len(chunks))
	}
	if strings.Join(chunks, "") != full {
		t.Fatalf("hard-cut chunks do not reassemble")
	}
}

func TestChunkWrappersFitLimit(t *testing.T) {
	budget := (MaxUpstreamQueryRunes - 100) - QueryChunkWrapperReserve
	chunk := strings.Repeat("测", budget) // 满预算的多字节正文
	prime := wrapPrimingChunk(chunk, 3, 9)
	final := wrapFinalChunk(chunk, 9)
	limit := MaxUpstreamQueryRunes - 100
	if n := len([]rune(prime)); n > limit {
		t.Fatalf("priming wrapper overflows limit: %d > %d (reserve too small)", n, limit)
	}
	if n := len([]rune(final)); n > limit {
		t.Fatalf("final wrapper overflows limit: %d > %d (reserve too small)", n, limit)
	}
}
