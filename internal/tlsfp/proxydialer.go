package tlsfp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	utls "github.com/refraction-networking/utls"
)

// NewProxyDialTLS 基于给定 Profile 与出口代理 URL，生成「经代理出站」场景的
// DialTLSContext。它先按代理协议（http/https/socks5）与代理建立到目标主机的隧道，
// 再在隧道之上用 uTLS（内置 parrot 或 HelloCustom）完成对【目标主机】的握手。
//
// 这样一来，无论直连还是走代理，上游看到的 ClientHello 都是同一套伪装指纹——
// 标准库 http.Transport.Proxy 会用 Go 默认指纹完成目标握手，无法满足这一点，
// 因此这里自己实现 CONNECT/SOCKS5 拨号并接管 TLS。
//
// 用法：
//
//	tr := &http.Transport{ForceAttemptHTTP2: false}
//	tr.DialTLSContext = tlsfp.NewProxyDialTLS(tlsfp.ChromiumDefault(), proxyURL)
func NewProxyDialTLS(p Profile, proxyURL *url.URL) DialTLSFunc {
	helloID, usePreset, idErr := resolveClientHelloID(p.ClientHelloID)
	var spec *utls.ClientHelloSpec
	if !usePreset {
		spec = p.buildSpec()
	}
	netDialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if idErr != nil {
			return nil, idErr
		}
		// 1) 经代理建立到目标 addr 的裸隧道（TCP 之上，未加 TLS）。
		tunnel, err := dialProxyTunnel(ctx, netDialer, proxyURL, addr)
		if err != nil {
			return nil, err
		}

		// 2) 在隧道之上，用 uTLS 对目标主机完成 TLS 握手，注入伪装指纹
		//    （ClientHelloID 非空走内置 parrot，否则 HelloCustom + spec）。
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			tunnel.Close()
			return nil, err
		}
		cfg := &utls.Config{
			ServerName: host,
			MinVersion: p.MinVersion,
			MaxVersion: p.MaxVersion,
			NextProtos: p.ALPN,
		}
		var uconn *utls.UConn
		if usePreset {
			uconn = utls.UClient(tunnel, cfg, helloID)
		} else {
			uconn = utls.UClient(tunnel, cfg, utls.HelloCustom)
			if err := uconn.ApplyPreset(spec); err != nil {
				tunnel.Close()
				return nil, err
			}
		}
		if err := uconn.HandshakeContext(ctx); err != nil {
			tunnel.Close()
			return nil, err
		}
		return uconn, nil
	}
}

// dialProxyTunnel 按代理 URL 的 scheme 与代理建立到 targetAddr 的隧道，
// 返回可直接在其上进行 TLS 握手的裸连接。支持 http/https/socks5/socks5h。
func dialProxyTunnel(ctx context.Context, d *net.Dialer, proxyURL *url.URL, targetAddr string) (net.Conn, error) {
	switch proxyURL.Scheme {
	case "http", "https":
		return dialHTTPConnect(ctx, d, proxyURL, targetAddr)
	case "socks5", "socks5h":
		return dialSOCKS5(ctx, d, proxyURL, targetAddr)
	default:
		return nil, fmt.Errorf("tlsfp: 不支持的代理协议 %q", proxyURL.Scheme)
	}
}

// dialHTTPConnect 通过 HTTP(S) 代理用 CONNECT 方法打通到目标的隧道。
// 若代理本身是 https，则先与代理完成一层标准 TLS（这层用代理证书，不影响目标指纹）。
func dialHTTPConnect(ctx context.Context, d *net.Dialer, proxyURL *url.URL, targetAddr string) (net.Conn, error) {
	proxyHost := proxyURL.Host
	if proxyURL.Port() == "" {
		if proxyURL.Scheme == "https" {
			proxyHost = net.JoinHostPort(proxyURL.Hostname(), "443")
		} else {
			proxyHost = net.JoinHostPort(proxyURL.Hostname(), "80")
		}
	}

	conn, err := d.DialContext(ctx, "tcp", proxyHost)
	if err != nil {
		return nil, err
	}

	// 与代理之间这一跳若为 https，用标准库 TLS 握手（校验代理证书，与目标指纹无关）。
	if proxyURL.Scheme == "https" {
		tconn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname()})
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("tlsfp: 与 https 代理握手失败: %w", err)
		}
		conn = tconn
	}

	// 若 ctx 有 deadline，作用到 CONNECT 交互上，避免代理无响应时永久阻塞。
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}
	if u := proxyURL.User; u != nil {
		pass, _ := u.Password()
		auth := u.Username() + ":" + pass
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
	}
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
	// 只需状态行；丢弃可能的响应体。
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("tlsfp: 代理 CONNECT 失败: %s", resp.Status)
	}
	if br.Buffered() > 0 {
		// CONNECT 正常时代理不应在 200 后抢跑数据；若有则说明代理行为异常。
		conn.Close()
		return nil, errors.New("tlsfp: 代理在 CONNECT 响应后发送了多余数据")
	}
	// 清除临时 deadline，后续 TLS/读写由上层 ctx 控制。
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// dialSOCKS5 通过 SOCKS5 代理建立到目标的 CONNECT 隧道（最小实现，支持无认证与用户名/密码认证）。
// socks5 与 socks5h 一致处理：始终把域名交给代理解析，不在本地解析。
func dialSOCKS5(ctx context.Context, d *net.Dialer, proxyURL *url.URL, targetAddr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("tlsfp: 非法目标端口 %q: %w", portStr, err)
	}

	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	fail := func(e error) (net.Conn, error) { conn.Close(); return nil, e }

	user := ""
	pass := ""
	if u := proxyURL.User; u != nil {
		user = u.Username()
		pass, _ = u.Password()
	}

	// 1) 方法协商。
	if user != "" {
		if _, err := conn.Write([]byte{0x05, 0x02, 0x00, 0x02}); err != nil {
			return fail(err)
		}
	} else {
		if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
			return fail(err)
		}
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(err)
	}
	if reply[0] != 0x05 {
		return fail(errors.New("tlsfp: SOCKS5 版本不匹配"))
	}
	switch reply[1] {
	case 0x00:
		// 无需认证。
	case 0x02:
		// 用户名/密码认证（RFC 1929）。
		auth := []byte{0x01}
		auth = append(auth, byte(len(user)))
		auth = append(auth, user...)
		auth = append(auth, byte(len(pass)))
		auth = append(auth, pass...)
		if _, err := conn.Write(auth); err != nil {
			return fail(err)
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(conn, ar); err != nil {
			return fail(err)
		}
		if ar[1] != 0x00 {
			return fail(errors.New("tlsfp: SOCKS5 认证被拒绝"))
		}
	default:
		return fail(errors.New("tlsfp: SOCKS5 代理要求不支持的认证方法"))
	}

	// 2) CONNECT 请求，域名类型（ATYP=0x03），交由代理解析。
	if len(host) > 255 {
		return fail(errors.New("tlsfp: SOCKS5 目标主机名过长"))
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return fail(err)
	}

	// 3) 读取回复：VER REP RSV ATYP BND.ADDR BND.PORT。
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fail(err)
	}
	if head[1] != 0x00 {
		return fail(fmt.Errorf("tlsfp: SOCKS5 CONNECT 失败 (rep=0x%02x)", head[1]))
	}
	var addrLen int
	switch head[3] {
	case 0x01: // IPv4
		addrLen = 4
	case 0x04: // IPv6
		addrLen = 16
	case 0x03: // 域名
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fail(err)
		}
		addrLen = int(l[0])
	default:
		return fail(errors.New("tlsfp: SOCKS5 回复地址类型未知"))
	}
	if _, err := io.ReadFull(conn, make([]byte, addrLen+2)); err != nil { // 丢弃 BND.ADDR + BND.PORT
		return fail(err)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
