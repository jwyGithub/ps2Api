// web.go —— 新版 Web Component 面板（/v2 入口），与 internal/dashboard 旧版并行。
package web

import "embed"

// Files contains the Lit-based dashboard frontend shipped with the binary.
// Source lives under dist/, served at /v2.
//
//go:embed all:dist
var Files embed.FS
