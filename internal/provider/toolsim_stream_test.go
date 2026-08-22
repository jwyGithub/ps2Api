package provider

import (
	"strings"
	"testing"
)

func TestIsViablePrefix(t *testing.T) {
	hold := []string{
		"<", "<t", "<to", "<tool", "<tool_", "<tool_c", "<tool_call", "<tool_call>",
		"<tool_call><name>f</name>", // already opened, keep buffering
		"`", "``", "```",
		"```x", "```xm", "```xml",
		"```tool", "```tool_call",
		"```j", "```js", "```json",
		"```json\n", "```json\n<too", "```xml <tool_call>",
	}
	for _, s := range hold {
		if !isViablePrefix(s) {
			t.Errorf("isViablePrefix(%q) = false, want true (must hold back)", s)
		}
	}

	flush := []string{
		"hello", "plain text", " ",
		"<b>", "<div>", "<tool>", "<template>", "<toolbar",
		"`code`", "``x",
		"```python", "```go\nfmt", "```json\n{\"a\":1}",
		"```xml <div>",
	}
	for _, s := range flush {
		if isViablePrefix(s) {
			t.Errorf("isViablePrefix(%q) = true, want false (safe to flush)", s)
		}
	}
}

func TestSplitStreamSafe(t *testing.T) {
	cases := []struct {
		in          string
		safe, pend  string
	}{
		{"hello world", "hello world", ""},
		{"hello <tool", "hello ", "<tool"},
		{"done <div>x</div>", "done <div>x</div>", ""},
		{"a < b <tool_ca", "a < b ", "<tool_ca"},
		{"text <tool_call><name>f</name><arguments>{}</arguments></tool_call>",
			"text ", "<tool_call><name>f</name><arguments>{}</arguments></tool_call>"},
		{"```", "", "```"},
		{"see ```json\n{\"a\":1}", "see ```json\n{\"a\":1}", ""},
		{"trailing `", "trailing ", "`"},
	}
	for _, c := range cases {
		safe, pend := splitStreamSafe(c.in)
		if safe != c.safe || pend != c.pend {
			t.Errorf("splitStreamSafe(%q) = (%q, %q), want (%q, %q)",
				c.in, safe, pend, c.safe, c.pend)
		}
		if safe+pend != c.in {
			t.Errorf("splitStreamSafe(%q): safe+pending = %q, must equal input", c.in, safe+pend)
		}
	}
}

// simulateStream replays chunks through the same incremental-flush logic used
// in StreamChat: accumulate into a buffer, flush the safe prefix on every
// chunk, then run the simulated-tool parser over whatever remains.
func simulateStream(chunks []string, tools []interface{}) (emitted string, cleanedTail string, calls int) {
	var buf strings.Builder
	var out strings.Builder
	for _, ch := range chunks {
		buf.WriteString(ch)
		safe, pending := splitStreamSafe(buf.String())
		if safe != "" {
			out.WriteString(safe)
			buf.Reset()
			buf.WriteString(pending)
		}
	}
	cleaned, sim := simulatedDeltas(buf.String(), tools)
	return out.String(), cleaned, len(sim)
}

func TestSplitStreamSafeNeverLeaksPartialMarker(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": "do_thing",
			},
		},
	}
	chunks := []string{
		"Sure, I'll ", "help. Calling ", "the too", "l now:\n",
		"<tool", "_call><name>do_", "thing</name><arg", "uments>{\"x\":1}",
		"</arguments></tool_call>",
	}
	emitted, cleanedTail, calls := simulateStream(chunks, tools)

	if calls != 1 {
		t.Fatalf("expected 1 simulated tool call, got %d", calls)
	}
	// The prose must have streamed incrementally and must never contain any
	// fragment of the tool-call marker.
	if strings.Contains(emitted, "<tool") {
		t.Errorf("emitted stream leaked a marker fragment: %q", emitted)
	}
	wantProse := "Sure, I'll help. Calling the tool now:\n"
	if emitted != wantProse {
		t.Errorf("emitted prose = %q, want %q", emitted, wantProse)
	}
	if strings.TrimSpace(cleanedTail) != "" {
		t.Errorf("cleaned tail should have no residual prose, got %q", cleanedTail)
	}
}

func TestSplitStreamSafePlainTextStreamsFully(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": "noop"},
		},
	}
	chunks := []string{"The answer ", "is 42. ", "No tools ", "needed here."}
	emitted, cleanedTail, calls := simulateStream(chunks, tools)
	if calls != 0 {
		t.Fatalf("expected 0 tool calls, got %d", calls)
	}
	full := emitted + cleanedTail
	if full != "The answer is 42. No tools needed here." {
		t.Errorf("reconstructed content = %q", full)
	}
	// Everything except possibly an empty tail should have already streamed.
	if emitted != "The answer is 42. No tools needed here." {
		t.Errorf("plain text did not fully stream incrementally: emitted=%q tail=%q", emitted, cleanedTail)
	}
}
