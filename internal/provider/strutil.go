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
