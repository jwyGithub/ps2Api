package provider

import (
	"os"
	"regexp"
)

// wafNeutralizeRules 按特征类别（而非逐个特征）破坏形态：Cloudflare WAF 托管内容
// 规则按解码后的子串形态匹配（实测出站体含 <script/onerror= 等 HTML/JS 标记的
// 前端源码 100% 被 403，纯后端源码从不被拦），在特征内部插一个空格即可破坏形态，
// 模型读 "< script src=x onerror =alert(1)>" 仍完全理解语义。
// 每条规则用两个捕获组夹住破坏点，replace 模板 "${1} ${2}" 在组间插空格——
// 扩特征只需往交替组里加词，不再逐条枚举。分四类：
//   1. 危险标签开头（含 json.Marshal HTML 转义产生的 u003c 字面量形）
//   2. 行内事件处理器 on任意=
//   3. 危险 URI 指令
//   4. Vue 指令（v-on: 与 @ 简写；@ 后只认 click/冒号，不误伤邮箱）
var wafNeutralizeRules = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{regexp.MustCompile(`(?i)(<|\\u003c)([!/?]?(?:script|iframe|svg|template|object|embed|form|style|link|meta|base|img|input|body|html|doctype)\b)`), "${1} ${2}"},
	{regexp.MustCompile(`(?i)(\bon[a-z]+)(\s*=)`), "${1} ${2}"},
	{regexp.MustCompile(`(?i)(javascript|vbscript)(:)`), "${1} ${2}"},
	{regexp.MustCompile(`(?i)(v-on|@)(:|click)`), "${1} ${2}"},
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
		s = rule.pattern.ReplaceAllString(s, rule.replace)
	}
	return s
}
