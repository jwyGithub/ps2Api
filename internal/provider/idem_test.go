package provider

import (
	"strings"
	"testing"
)

// TestWafNeutralizeIdempotent 钉住幂等性（2026-09-18 issue 修复）：backtick+curl/wget
// 规则旧「词前插入」写法每处理一遍多插一个 ZWSP——上游回显内容经多轮中和会无界累加。
// 修复后 ZWSP 插在 cu|rl / wg|et 词内，下一轮该位置是 ZWSP 而非 l/t，匹配不再成立。
func TestWafNeutralizeIdempotent(t *testing.T) {
	cases := []string{
		"`curl -x http://a b`",
		"`wget -O f url`",
		"` curl -o f url",
		"```\ncurl -o f url\n```",
		"see `curl -L url` here",
		"`curl -o /tmp/f https://example.com`",
		"<script>alert(1)</script>",
		"< img src=x onerror=alert(1)>",
		"<imgsrc=xonerror",
		"\\u003cimgsrc=x",
		"< s c r i p t src=x>",
		"./bin/catpaw2api -config x",
		"javascript:void(0)",
	}
	for _, in := range cases {
		once := wafNeutralize(in)
		for i := 0; i < 3; i++ { // 反复多轮，不只两轮
			again := wafNeutralize(once)
			if once != again {
				t.Errorf("NOT IDEMPOTENT: in=%q once=%q again=%q (zwsp %d→%d)", in, once, again,
					strings.Count(once, zwsp), strings.Count(again, zwsp))
			}
			once = again
		}
	}
}
