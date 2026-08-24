package provider

import (
	"net/http"
	"net/url"

	"ps2api/internal/tlsfp"
)

// activeProfile 返回当前出站指纹模板。当前固定为默认模板（三层对齐 Chromium/Edge：
// TLS ClientHello 走 uTLS 内置 Chrome parrot，ALPN 提供 h2+http/1.1，h2 层复刻 Chromium）；
// 后续可改为从 settings 读取以支持面板热切换（Profile 已可 JSON 序列化）。
func activeProfile() tlsfp.Profile {
	return tlsfp.ChromiumDefault()
}

// newFingerprintTransport 构造直连场景的出站传输层：一个把 TLS 握手替换为指纹伪装、
// 并按 ALPN 分流（h2 走自写 Chromium 指纹 h2 客户端 / http1.1 回落）的 http.RoundTripper。
//
// 返回 http.RoundTripper（而非裸 *http.Transport）：h2 的 SETTINGS/窗口增量/伪头顺序
// 也是指纹，官方 http2.Transport 改不动，故由 tlsfp 内的最小 h2 客户端接管。
// 对上层 http.Client{Transport: ...} 接口不变。
func newFingerprintTransport() http.RoundTripper {
	return tlsfp.NewFingerprintRoundTripper(activeProfile())
}

// newFingerprintProxyTransport 构造「经出口代理出站」且仍携带同一套指纹的 http.RoundTripper。
// 与直连不同的是握手在代理隧道之上完成（见 tlsfp.NewProxyDialTLS），两条出口指纹自洽。
func newFingerprintProxyTransport(proxyURL *url.URL) http.RoundTripper {
	return tlsfp.NewFingerprintProxyRoundTripper(activeProfile(), proxyURL)
}
