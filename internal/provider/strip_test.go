package provider

import (
	"encoding/json"
	"testing"
)

// TestWafStripBreak 钉住回写污染防线：响应侧（textChunk / toolCall arguments）必须
// 剥离中和引入的 ZWSP——模型把读到的 `cu​rl` 原样抄进 Edit.new_string 时，不剥离
// 就会随 agent 写回落进源码（2026-09-18 风险评审）。
func TestWafStripBreak(t *testing.T) {
	zw := zwsp
	cases := []struct{ in, want string }{
		{"clean text", "clean text"},                                  // 快路径：无 ZWSP 零拷贝
		{"edit cu" + zw + "rl -x http://a b", "edit curl -x http://a b"}, // 单点
		{"<" + zw + "script>" + "a" + zw + "b", "<script>ab"},          // 多点
		{zw + zw, ""},                                                     // 纯 ZWSP
		{"", ""},                                                          // 空串
	}
	for _, c := range cases {
		if got := wafStripBreak(c.in); got != c.want {
			t.Errorf("wafStripBreak(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStreamTextChunkStripsZWSP 端到端：SSE textChunk 事件带 ZWSP → Delta 内容干净。
func TestStreamTextChunkStripsZWSP(t *testing.T) {
	r := NewStreamReader()
	payload := `data: {"eventType":"textChunk","data":{"textContent":"use ` + zwsp + `curl here"}}`
	var got string
	for _, d := range r.Feed(payload) {
		got += d.Content
	}
	if got != "use curl here" {
		t.Fatalf("textChunk content should be ZWSP-stripped, got %q", got)
	}
}

// TestNormalizeArgumentsStripsZWSP 端到端：工具参数（Edit new_string 形态）带 ZWSP → 剥离。
func TestNormalizeArgumentsStripsZWSP(t *testing.T) {
	raw := json.RawMessage(`"` + "new_code_cu" + zwsp + "rl()" + `"`)
	got := normalizeArguments(raw)
	if got != "new_code_curl()" {
		t.Fatalf("arguments should be ZWSP-stripped, got %q", got)
	}
}
