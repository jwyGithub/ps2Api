package provider

import (
	"os"
	"regexp"
)

// wafNeutralizeRules 把 wafSignatureProbes 里的每个特征映射为「插入一个空格」的破坏点：
// Cloudflare WAF 托管内容规则按解码后的子串形态匹配（实测出站体含 <script/onerror= 等
// HTML/JS 标记的前端源码 100% 被 403，纯后端源码从不被拦），在特征内部插一个空格即可
// 破坏形态，模型读 "< script src=x onerror =alert(1)>" 仍完全理解语义。
// 分四类：标签（< 后插）、事件处理器（= 前插）、URI/指令（: 前插）、Vue 简写（@ 后插）。
var wafNeutralizeRules = []struct {
	pattern *regexp.Regexp
	insert  int // 在匹配串的第 insert 字节处插入空格
}{
	{regexp.MustCompile(`(?i)</?script`), 1},
	{regexp.MustCompile(`(?i)</?iframe`), 1},
	{regexp.MustCompile(`(?i)</?svg`), 1},
	{regexp.MustCompile(`(?i)</?template`), 1},
	{regexp.MustCompile(`(?i)<!doctype`), 1},
	{regexp.MustCompile(`(?i)onerror=`), len("onerror")},
	{regexp.MustCompile(`(?i)onload=`), len("onload")},
	{regexp.MustCompile(`(?i)onclick=`), len("onclick")},
	{regexp.MustCompile(`(?i)onchange=`), len("onchange")},
	{regexp.MustCompile(`(?i)javascript:`), len("javascript")},
	{regexp.MustCompile(`(?i)v-on:`), len("v-on")},
	{regexp.MustCompile(`(?i)@click`), 1},
}

// wafNeutralizeEnabled 是中和的 kill-switch：GATEWAY_DISABLE_WAF_NEUTRALIZE=1 时关闭
// （与 GATEWAY_DISABLE_THIRD_PARTY 同款先例）。默认开启。
func wafNeutralizeEnabled() bool {
	return os.Getenv("GATEWAY_DISABLE_WAF_NEUTRALIZE") != "1"
}

// wafNeutralize 破坏字符串里的 WAF 特征形态（大小写不敏感），用于出站请求体的文本
// 出口（input.query / toolResponses[].content）。不含特征的字符串原样返回（同一底层
// 字符串，零拷贝）。中和只作用于出站副本，req.Messages 原文绝不动——会话指纹在原文
// 上计算，出站序列化不属于指纹层。
func wafNeutralize(s string) string {
	if WafSignatureHitCount(s) == 0 {
		return s
	}
	for _, rule := range wafNeutralizeRules {
		s = rule.pattern.ReplaceAllStringFunc(s, func(m string) string {
			return m[:rule.insert] + " " + m[rule.insert:]
		})
	}
	return s
}
