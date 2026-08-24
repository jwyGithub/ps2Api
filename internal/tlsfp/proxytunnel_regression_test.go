package tlsfp

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// startTestConnectProxy 启动一个最小 HTTP CONNECT 代理，仅用于测试隧道路径。
func startTestConnectProxy(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					c.Close()
					return
				}
				up, err := net.DialTimeout("tcp", req.Host, 10*time.Second)
				if err != nil {
					c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
					c.Close()
					return
				}
				c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				go io.Copy(up, br)
				io.Copy(c, up)
				up.Close()
				c.Close()
			}(c)
		}
	}()
	return "http://" + ln.Addr().String(), func() { ln.Close() }
}

// TestProxyTunnelDoesNotHang 回归测试：经 CONNECT 代理出站时，不得读取 CONNECT
// 响应体（否则会与代理透传模式互等而挂起，直到 EOF/超时）。需要外网；无网络自动跳过。
func TestProxyTunnelDoesNotHang(t *testing.T) {
	proxy, stop := startTestConnectProxy(t)
	defer stop()
	pu, _ := url.Parse(proxy)
	rt := NewFingerprintProxyRoundTripper(ChromiumDefault(), pu)
	client := &http.Client{Transport: rt}

	// GET（无 body）
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://gateway.postman.com/", nil)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("经代理 GET 挂起/超时（回归）：%v", err)
		}
		t.Skipf("跳过（无外网或网关不可达）：%v", err)
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("期望经代理协商到 HTTP/2，实际 %s", resp.Proto)
	}

	// POST（带 body，模拟真实 /_gw/chat 流量）
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	body := strings.NewReader(strings.Repeat("x", 2048))
	req2, _ := http.NewRequestWithContext(ctx2, http.MethodPost, "https://gateway.postman.com/", body)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("经代理 POST 失败（回归）：%v", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp2.Body, 1<<12))
	resp2.Body.Close()
}

// resolveRealTestProxy 解析用于测试的真实出口代理：优先环境变量 TLSFP_TEST_PROXY，
// 否则回退到本机常见代理 http://127.0.0.1:7890（Clash/mihomo 默认）。返回解析后的 URL；
// 若代理端口不可达（未开代理/CI 环境）则返回 nil，由调用方 skip。
func resolveRealTestProxy(t *testing.T) *url.URL {
	t.Helper()
	raw := os.Getenv("TLSFP_TEST_PROXY")
	if raw == "" {
		raw = "http://127.0.0.1:7890"
	}
	pu, err := url.Parse(raw)
	if err != nil {
		t.Skipf("跳过（TLSFP_TEST_PROXY=%q 无法解析）：%v", raw, err)
		return nil
	}
	// 快速探测代理端口可达性，避免在没开代理时白等一个完整超时。
	c, err := net.DialTimeout("tcp", pu.Host, 500*time.Millisecond)
	if err != nil {
		t.Skipf("跳过（代理 %s 不可达，本地未开代理或 CI 环境）：%v", pu.Host, err)
		return nil
	}
	c.Close()
	return pu
}

// TestProxyTunnelRealProxy 使用真实出口代理（默认 127.0.0.1:7890）做端到端验证：
// 经代理出站不得挂起、须协商到 HTTP/2，且并发多请求下连接复用正常。需要本地代理 +
// 外网；不可达时自动跳过。可用 TLSFP_TEST_PROXY 覆盖代理地址。
func TestProxyTunnelRealProxy(t *testing.T) {
	pu := resolveRealTestProxy(t)
	if pu == nil {
		return
	}
	rt := NewFingerprintProxyRoundTripper(ChromiumDefault(), pu)
	client := &http.Client{Transport: rt}

	// 单请求：验证不挂起 + HTTP/2。给足超时，一旦「握手前挂起」的回归复现会以超时失败。
	doOnce := func(method string, body io.Reader) *http.Response {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, method, "https://gateway.postman.com/", body)
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("经真实代理 %s 挂起/超时（回归）：%v", method, err)
			}
			t.Skipf("跳过（经代理请求失败，可能无外网）：%v", err)
			return nil
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("经真实代理 %s 耗时异常 %v（疑似握手前挂起）", method, d)
		}
		return resp
	}

	resp := doOnce(http.MethodGet, nil)
	if resp == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("期望经真实代理协商到 HTTP/2，实际 %s", resp.Proto)
	}

	resp2 := doOnce(http.MethodPost, strings.NewReader(strings.Repeat("x", 2048)))
	if resp2 == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp2.Body, 1<<12))
	resp2.Body.Close()

	// 并发：同一 RoundTripper 下多请求并发，验证连接建立/复用无死锁、无粘包错乱。
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://gateway.postman.com/", nil)
			r, err := client.Do(req)
			if err != nil {
				errs[i] = err
				return
			}
			io.Copy(io.Discard, io.LimitReader(r.Body, 1<<12))
			r.Body.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发请求 #%d 经真实代理失败：%v", i, err)
		}
	}
}
