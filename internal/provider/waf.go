package provider

import (
	"os"
	"regexp"
)

// wafBreak 是特征破坏字符（零宽空格 U+200B）。实测（2026-09-09，经网关直连上游验证）：
// Cloudflare 匹配前做归一化——剥离空白与反斜杠，所以插空格/反斜杠全部无效：
//   "< script>" / "<scr ipt>" / "</script>" 单独在场即 403，"<\script" 同样 403；
// 而零宽空格与全角字符保留："<"+零宽空格+"template>…" 类形式正常放行。
// 另证：裸 "script"、"alert(1)"、"< img src=x onerror=alert(1)>"、"< div>" 均放行——
// 真正的特征只有 script 家族标签（开/闭都算）；其余规则是保险，零宽字符不可见无损。
const wafBreak = "\u200b"

// wafNeutralizeRules 按特征类别（而非逐个特征）破坏形态：在特征内部的组边界插入
// 零宽空格（wafBreak），归一化后 "<"+零宽空格+"script" 不再是 "<script"，而模型 tokenizer
// 对零宽空格基本不可见，阅读无损。每条规则用两个捕获组夹住破坏点，replace 模板
// "${1}"+wafBreak+"${2}" 在组间插入——扩特征只需往交替组里加词。分四类：
//   1. 危险标签开头（含 json.Marshal HTML 转义产生的 u003c 字面量形）
//   2. 行内事件处理器 on任意=
//   3. 危险 URI 指令
//   4. Vue 指令（v-on: 与 @ 简写；@ 后只认 click/冒号，不误伤邮箱）
var wafNeutralizeRules = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`(?i)(<|\\u003c)([!/?]?(?:script|iframe|svg|template|object|embed|form|style|link|meta|base|img|input|body|html|doctype)\b)`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(\bon[a-z]+)(\s*=)`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(javascript|vbscript)(:)`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(v-on|@)(:|click)`), "${1}" + wafBreak + "${2}"},
}

// wafNeutralizeEnabled 是中和的 kill-switch：GATEWAY_DISABLE_WAF_NEUTRALIZE=1 时关闭
// （与 GATEWAY_DISABLE_THIRD_PARTY 同款先例）。默认开启。
func wafNeutralizeEnabled() bool {
	return os.Getenv("GATEWAY_DISABLE_WAF_NEUTRALIZE") != "1"
}

// wafNeutralize 破坏字符串里的 WAF 特征形态（大小写不敏感），用于出站请求体的文本
// 出口（input.query / toolResponses[].content）。不含特征的字符串原样返回。不做
// WafSignatureHitCount 预判：探测表（如 "<script"）认不出 "</script>"-only 的载荷，
// 门会放走只有闭合标签的截断 tool result（实测 CF 对 "</script>" 单独在场即 403），
// 4 条规则对干净文本近零开销，直接全跑。中和只作用于出站副本，req.Messages 原文
// 绝不动——会话指纹在原文上计算，出站序列化不属于指纹层。
func wafNeutralize(s string) string {
	for _, rule := range wafNeutralizeRules {
		s = rule.pattern.ReplaceAllString(s, rule.replace)
	}
	return s
}
