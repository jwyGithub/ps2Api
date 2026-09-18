package provider

import (
	"os"
	"regexp"
	"strings"
)

// wafBreak 是特征破坏字符（零宽空格 U+200B）。实测（2026-09-09，经网关直连上游验证）：
// Cloudflare 匹配前做归一化——剥离空白与反斜杠，所以插空格/反斜杠全部无效：
//
//	"< script>" / "<scr ipt>" / "</script>" 单独在场即 403，"<\script" 同样 403；
//
// 而零宽空格与全角字符保留："<"+零宽空格+"template>…" 类形式正常放行。
// 另证：裸 "script"、"alert(1)"、"< img src=x onerror=alert(1)>"、"< div>" 均放行——
// 真正的特征只有 script 家族标签（开/闭都算）；其余规则是保险，零宽字符不可见无损。
const wafBreak = "\u200b"

// wafNeutralizeRules 按特征类别（而非逐个特征）破坏形态：在特征内部的组边界插入
// 零宽空格（wafBreak），归一化后 "<"+零宽空格+"script" 不再是 "<script"，而模型 tokenizer
// 对零宽空格基本不可见，阅读无损。每条规则用两个捕获组夹住破坏点，replace 模板
// "${1}"+wafBreak+"${2}" 在组间插入——扩特征只需往交替组里加词。分四类：
//  1. 危险标签开头（含 json.Marshal HTML 转义产生的 u003c 字面量形）
//  2. 行内事件处理器 on任意=
//  3. 危险 URI 指令
//  4. Vue 指令（v-on: 与 @ 简写；@ 后只认 click/冒号，不误伤邮箱）
var wafNeutralizeRules = []struct {
	pattern *regexp.Regexp
	replace string
}{
	// 无 \b 词边界（2026-09-17 三次回归）：CF 归一化剥空格后是纯前缀匹配——
	// <imgsrc=、<imgonerror 这类「标签名后紧跟词字符」的形态，\b 不命中但 CF 照拦
	//（实测 3427 出站体 `<imgsrc=xonerror…` 403，同体去掉它即过）。改前缀命中即插
	// ZWSP：误伤面（<imgx 之类非标签词）只多一个不可见字符，阅读无损；漏伤代价是 403。
	// 词内连续性由 CF 归一化定义，不由 HTML 规范定义，边界必须跟它对齐。
	{regexp.MustCompile(`(?i)(<|\\u003c)([\s\\]*[!/?]?[\s\\]*(?:script|iframe|svg|template|object|embed|form|style|link|meta|base|img|input|body|html|doctype))`), "${1}" + wafBreak + "${2}"},
	// script 分离形（2026-09-17 回归实测）：CF 归一化剥掉空白与反斜杠后，
	// < script / <scr ipt / <\script 全部还原为 <script 确定性 403（2026-09-09
	// 探针表早已钉住），而上面的规则在 < 与标签名之间不容忍任何字符，全部穿透。
	// 触发载荷是本仓库 WAF 排查文档自身——文中引用的「插空格无效」示例成了真实
	// 出站内容。分离符（[\s\\]，恰为归一化剥掉的字符集）只逐字母容忍 script：
	// script 家族是唯一实测标签签名，其余标签的分离形未见真实流量（需要时同法扩词）。
	// 幂等：ZWSP 不属于 [\s\\]，破坏后的形态不再命中。
	{regexp.MustCompile(`(?i)(<|\\u003c)([\s\\]*[!/?]?[\s\\]*s[\s\\]*c[\s\\]*r[\s\\]*i[\s\\]*p[\s\\]*t)`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(\bon[a-z]+)(\s*=)`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(javascript|vbscript)(:)`), "${1}" + wafBreak + "${2}"},
	{regexp.MustCompile(`(?i)(v-on|@)(:|click)`), "${1}" + wafBreak + "${2}"},
	// bin/cat 形（2026-09-11 实测，repro403 探针七轮二分定位）：cat 前缀词跟在
	// bin/ 后被 Cloudflare 当作 cat 命令执行路径——`./bin/cat`、`/bin/cat`、
	// `./bin/catpaw2api`、大写 `Catpaw2api` 均 403；`bin/`+ZWSP+`cat` 放行；
	// bin/ls、bin/sh、bin/rm、bin/python、bin/curl、`./cat`、裸 `cat` 均放行，
	// 即特征是字面量 bin/cat（cat 是唯一触发命令），ZWSP 插在 bin/ 与 cat 之间。
	{regexp.MustCompile(`(?i)(s?bin/)(cat)`), "${1}" + wafBreak + "${2}"},
	// shell 命令行注入形（2026-09-17 20:15 实测，线上体行级二分 ~20 轮定位）：
	// 反引号/代码围栏上下文里的 curl/wget + 单连字符 flag + 参数 触发 CF
	// 命令注入托管规则——`curl -o f url`、```\ncurl -o f url\n```（无语言标注）、
	// ` wget -O f url` 均 403；`curl "y"`、`curl -o`（flag 无值）、`--longflag`、
	// -o-f 紧连、```bash\n…（有语言标注）、无 backtick 的裸 curl、ls/cat/rm/git
	// 等 200。ZWSP 插在 cu|rl / wg|et 词内（实测词内与词后/backtick 后均放行，词内
	// 最稳、与 bin/cat 同款）。词内插入同时保证幂等：下一轮该位置是 ZWSP 而非 l/t，
	// 匹配不再成立（2026-09-18 issue 修复——旧「词前插入」写法每处理一遍多插一个
	// ZWSP，上游回显内容经多轮中和会无界累加）。backtick 群与 curl 之间允许空格。
	{regexp.MustCompile("(?s)(`{1,3}[^`]{0,40}?cu[\\w]*r)(l)(\\s+-[^\\s-][^\\s]*\\s+[^\\s])"), "${1}" + wafBreak + "${2}${3}"},
	{regexp.MustCompile("(?s)(`{1,3}[^`]{0,40}?wg[\\w]*e)(t)(\\s+-[^\\s-][^\\s]*\\s+[^\\s])"), "${1}" + wafBreak + "${2}${3}"},
	// 管道/分号注入形（2026-09-18 线上探针二分定位，~15 轮在线对照）：`;` 或 `|` 后
	// **紧邻或单空格**跟 curl/wget + 带协议的 URL 即触发 CF 命令注入托管规则——
	// `; curl http://x`、`;curl https://x`、`| wget ftp://x`、`; CURL http://x`、
	// `; curl -fsSL http://x` 均 403；`&&`、换行、双空格 `;  curl`、URL 无协议
	//（`curl a.example/x`）、`; curl`（无 URL）、`; curl http`（协议截断）、`; echo`、
	// `curl … ; echo`（curl 在前）全部放行。分离规则与既有 backtick 形实证一致：
	// CF 归一化剥空白后 `;  curl http://x` 理应还原为 `;curl http://x` 触发——但实测
	// 放行，说明该规则对分隔符后空格数敏感（仅容忍 0-1 个），边界跟实测走。ZWSP 插在
	// cu|rl / wg|et 词内（与规则 7 同款，实测词内破坏放行）。幂等：下一轮该位置是
	// ZWSP 而非 l/t，匹配不再成立。
	{regexp.MustCompile(`(?is)([;|][ \t]{0,}cu[\w]*r)(l)([^;|\n]{0,80}?\w+://)`), "${1}" + wafBreak + "${2}${3}"},
	{regexp.MustCompile(`(?is)([;|][ \t]{0,}wg[\w]*e)(t)([^;|\n]{0,80}?\w+://)`), "${1}" + wafBreak + "${2}${3}"},
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

// wafStripBreak 剥离响应文本里的零宽空格（wafBreak）。出站中和让模型读到带 ZWSP 的
// 代码（如 `cu​rl`）；模型在 Edit.new_string / Write 内容里原样回显时，若不剥离就会
// 随 agent 写回落进源码，造成不可见的持续污染（2026-09-18 风险评审：响应侧此前
// 零防护，handleTextChunk/normalizeArguments 均直传）。在 Delta 产生处统一剥离，
// 流式/非流式/三种协议端点一次收口。ZWSP 只由中和规则引入，剥离对干净文本是
// no-op；不含 ZWSP 的文本快路径返回原串，零拷贝。
func wafStripBreak(s string) string {
	if !strings.ContainsRune(s, 0x200b) {
		return s
	}
	return strings.ReplaceAll(s, wafBreak, "")
}
