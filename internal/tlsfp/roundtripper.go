package tlsfp

// FingerprintRoundTripper 是替代裸 *http.Transport 的传输层：它自己用 uTLS 拨号
// （直连或经代理隧道，复用 dialer.go / proxydialer.go），握手后按 ALPN 结果分流——
//   - 协商到 "h2"：交给自写最小 h2 客户端（注入 Chromium h2 指纹），并按 host 池化连接复用；
//   - 协商到 "http/1.1"：在同一条已拨连接上直接跑最小 h1（单次使用，不池化）。
//
// 对上层保持 http.RoundTripper 接口不变，因此 postman.go / proxy.go 里的
// http.Client{Transport: ...} 无需改动。

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"
)

// FingerprintRoundTripper 实现 http.RoundTripper。零值不可用，请用
// NewFingerprintRoundTripper / NewFingerprintProxyRoundTripper 构造。
type FingerprintRoundTripper struct {
	profile Profile
	dialTLS DialTLSFunc

	mu    sync.Mutex
	conns map[string]*h2Conn // authority(host:port) -> 复用中的 h2 连接
}

// NewFingerprintRoundTripper 构造直连场景的 RoundTripper。
func NewFingerprintRoundTripper(p Profile) *FingerprintRoundTripper {
	return &FingerprintRoundTripper{
		profile: p,
		dialTLS: NewDirectDialTLS(p),
		conns:   make(map[string]*h2Conn),
	}
}

// NewFingerprintProxyRoundTripper 构造「经出口代理出站」场景的 RoundTripper，
// 握手在代理隧道之上完成，指纹与直连一致。
func NewFingerprintProxyRoundTripper(p Profile, proxyURL *url.URL) *FingerprintRoundTripper {
	return &FingerprintRoundTripper{
		profile: p,
		dialTLS: NewProxyDialTLS(p, proxyURL),
		conns:   make(map[string]*h2Conn),
	}
}

// RoundTrip 实现 http.RoundTripper。因为我方主动声明了 accept-encoding（见
// provider.buildHeaders），响应可能带 Content-Encoding；标准库 Transport 会在自己补
// accept-encoding 时自动解压，而这里是自写传输层，需在出口统一按 Content-Encoding 解压。
func (rt *FingerprintRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.roundTripInner(req)
	if err != nil {
		return nil, err
	}
	return decompressResponse(resp), nil
}

func (rt *FingerprintRoundTripper) roundTripInner(req *http.Request) (*http.Response, error) {
	authority := authorityAddr(req.URL)

	// 1) 尝试复用池中的 h2 连接；若发现连接不可用则重拨一次。
	if c := rt.getConn(authority); c != nil {
		resp, err := c.roundTrip(req)
		if err != errConnUnusable {
			return resp, err
		}
		rt.dropConn(authority, c)
	}

	// 2) 新拨一条连接，按 ALPN 分流。
	return rt.dialAndRoundTrip(req, authority)
}

// decompressResponse 按 Content-Encoding 把响应体换成流式解压 reader（gzip/deflate/br/zstd）。
// 解码器全部流式，SSE（text/event-stream，服务端不压）会因无 Content-Encoding 而原样透传，
// 不影响流式；仅错误页/非流响应被压缩时才解码。解码失败或未知编码则原样返回，避免误伤。
func decompressResponse(resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil {
		return resp
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return resp
	}
	var dec io.ReadCloser
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return resp
		}
		dec = zr
	case "deflate":
		dec = flate.NewReader(resp.Body)
	case "br":
		dec = io.NopCloser(brotli.NewReader(resp.Body))
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return resp
		}
		dec = zr.IOReadCloser()
	default:
		return resp
	}
	resp.Body = &decompressBody{dec: dec, orig: resp.Body}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp
}

// decompressBody 把解压 reader 与原始 body 绑定：Read 走解压流，Close 同时关闭两者。
type decompressBody struct {
	dec  io.ReadCloser
	orig io.ReadCloser
}

func (b *decompressBody) Read(p []byte) (int, error) { return b.dec.Read(p) }

func (b *decompressBody) Close() error {
	decErr := b.dec.Close()
	origErr := b.orig.Close()
	if decErr != nil {
		return decErr
	}
	return origErr
}

func (rt *FingerprintRoundTripper) dialAndRoundTrip(req *http.Request, authority string) (*http.Response, error) {
	ctx := req.Context()
	conn, err := rt.dialTLS(ctx, "tcp", authority)
	if err != nil {
		return nil, err
	}

	switch negotiatedProto(conn) {
	case "h2":
		hc, err := newH2Conn(ctx, conn, authority, rt.profile.H2)
		if err != nil {
			conn.Close()
			return nil, err
		}
		rt.storeConn(authority, hc)
		return hc.roundTrip(req)
	default:
		// http/1.1（或未协商到 ALPN）：在这条连接上直接跑最小 h1，单次使用。
		return roundTripH1(conn, req)
	}
}

// ---- 连接池 ----

func (rt *FingerprintRoundTripper) getConn(authority string) *h2Conn {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c := rt.conns[authority]
	if c != nil && !c.usable() {
		delete(rt.conns, authority)
		return nil
	}
	return c
}

// storeConn 把新建 h2 连接放入池；若期间已有其它 goroutine 存入同 authority 的连接，
// 则关闭本次多拨的连接、复用既有的（此处返回，由调用方在既有连接上发请求）。
func (rt *FingerprintRoundTripper) storeConn(authority string, c *h2Conn) {
	rt.mu.Lock()
	if existing := rt.conns[authority]; existing != nil && existing.usable() {
		rt.mu.Unlock()
		// 已有可用连接，放弃本次新连接。为简化，仍保留本连接自然消亡；
		// 但优先登记既有连接的关闭回调不受影响。
		c.onClose = func() { rt.dropConn(authority, c) }
		return
	}
	rt.conns[authority] = c
	c.onClose = func() { rt.dropConn(authority, c) }
	rt.mu.Unlock()
}

func (rt *FingerprintRoundTripper) dropConn(authority string, c *h2Conn) {
	rt.mu.Lock()
	if rt.conns[authority] == c {
		delete(rt.conns, authority)
	}
	rt.mu.Unlock()
}

// ---- HTTP/1.1 回退（在已拨连接上单次使用）----

func roundTripH1(conn net.Conn, req *http.Request) (*http.Response, error) {
	// 直接把请求写到连接（origin-form 请求行 + Host 头），再读回响应。
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// 响应体读取需要连接存活；调用方关闭 body 时一并关闭连接（不做 h1 连接复用）。
	resp.Body = &connClosingBody{rc: resp.Body, conn: conn}
	return resp, nil
}

// connClosingBody 包装 h1 响应体，Close 时同时关闭底层连接。
type connClosingBody struct {
	rc   io.ReadCloser
	conn net.Conn
	once sync.Once
}

func (b *connClosingBody) Read(p []byte) (int, error) { return b.rc.Read(p) }

func (b *connClosingBody) Close() error {
	err := b.rc.Close()
	b.once.Do(func() { b.conn.Close() })
	return err
}

// ---- 辅助 ----

// negotiatedProto 从 uTLS 连接读取 ALPN 协商结果。
func negotiatedProto(conn net.Conn) string {
	if uc, ok := conn.(*utls.UConn); ok {
		return uc.ConnectionState().NegotiatedProtocol
	}
	return ""
}

// authorityAddr 返回 URL 的 "host:port"，https 缺省端口补 443。
func authorityAddr(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return net.JoinHostPort(host, port)
}

// 编译期确认接口实现。
var _ http.RoundTripper = (*FingerprintRoundTripper)(nil)
