package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// fingerprint 把若干片段各自以 NUL(0) 分隔后做 sha256，返回 hex 字符串。
// 用于把结构化片段拍扁成稳定指纹（片段间加分隔符避免 "ab"+"c" 与 "a"+"bc" 碰撞）。
func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashHex 允许调用方把任意内容直接写入 sha256（如 json.Encoder 编码结构体），返回 hex。
// 用于无法简单表达成字符串片段、需要流式写入的指纹场景。
func hashHex(write func(w io.Writer)) string {
	h := sha256.New()
	write(h)
	return hex.EncodeToString(h.Sum(nil))
}
