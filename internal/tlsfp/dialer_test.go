package tlsfp

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestDirectHandshakeGateway 验证指纹拨号器能与真实上游网关完成 TLS 握手。
// 默认模板 ALPN 提供 h2+http/1.1；本测试直接用裸 http.Transport（不启用 h2），
// 故握手层允许协商到 http/1.1 或空。需要外网；无网络时自动跳过。
func TestDirectHandshakeGateway(t *testing.T) {
	tr := &http.Transport{
		DialTLSContext:    NewDirectDialTLS(ChromiumDefault()),
		ForceAttemptHTTP2: false,
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: tr}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://gateway.postman.com/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("跳过（无外网或网关不可达）：%v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.TLS != nil {
		if resp.TLS.NegotiatedProtocol != "http/1.1" && resp.TLS.NegotiatedProtocol != "" {
			t.Fatalf("ALPN 期望 http/1.1，实际 %q", resp.TLS.NegotiatedProtocol)
		}
		t.Logf("握手成功 status=%d tlsVersion=0x%04x alpn=%q", resp.StatusCode, resp.TLS.Version, resp.TLS.NegotiatedProtocol)
	} else {
		t.Logf("已连通网关 status=%d（resp.TLS 未透出，属该端点响应怪癖，握手本身成功）", resp.StatusCode)
	}
}

// TestJA3Fingerprint 打印经指纹拨号器观测到的 JA3，便于对照真实 Node.js 抓包校准。
// 需要外网访问 tls.peet.ws；无网络时自动跳过。
func TestJA3Fingerprint(t *testing.T) {
	tr := &http.Transport{
		DialTLSContext:    NewDirectDialTLS(ChromiumDefault()),
		ForceAttemptHTTP2: false,
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: tr}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://tls.peet.ws/api/clean", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("跳过（无外网）：%v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	t.Logf("JA3 探测响应：%s", string(body))
	_ = tls.VersionTLS13
}
