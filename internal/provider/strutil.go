package provider

// truncateRunes 按 rune 数截断字符串，超长时在尾部追加省略号 "…"。
// 按 rune（而非 byte）截断，避免把多字节 UTF-8 字符切成乱码。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// truncateMiddleRunes 在 s 超过 max 个 rune 时保留开头与结尾、省略中段（tool 输出的
// 头尾往往都携带关键信号：命令/请求在前、结果/报错在后）。未超限或 max<=0 时原样返回。
// 按 rune 截断，避免切碎多字节 UTF-8 字符。
func truncateMiddleRunes(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	const marker = "…[truncated]…"
	keep := max - len([]rune(marker))
	if keep <= 0 {
		return string(r[:max])
	}
	head := keep / 2
	tail := keep - head
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}
