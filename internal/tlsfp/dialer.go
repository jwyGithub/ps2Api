package tlsfp

import (
	"context"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// DialTLSFunc 与 net/http.Transport.DialTLSContext 的签名一致：给定已解析的
// "host:port"，返回一个已完成 TLS 握手的连接。
type DialTLSFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// NewDirectDialTLS 基于给定 Profile 生成直连场景的 DialTLSContext。
// 它先用标准库拨出 TCP，再在其上用 uTLS 完成握手，从而让 net/http.Transport 走我们
// 伪装后的 ClientHello：Profile.ClientHelloID 非空时用 uTLS 内置 parrot（默认 Chrome），
// 为空时回落 HelloCustom + buildSpec 的逐字节自定义规格。
//
// 用法：
//
//	tr := &http.Transport{ForceAttemptHTTP2: false}
//	tr.DialTLSContext = tlsfp.NewDirectDialTLS(tlsfp.ChromiumDefault())
func NewDirectDialTLS(p Profile) DialTLSFunc {
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
		rawConn, err := netDialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			rawConn.Close()
			return nil, err
		}

		cfg := &utls.Config{
			ServerName: host,
			MinVersion: p.MinVersion,
			MaxVersion: p.MaxVersion,
			// 与 ClientHello 声明的 ALPN 一致（默认 h2+http/1.1），由上层按协商结果分流。
			NextProtos: p.ALPN,
		}

		var uconn *utls.UConn
		if usePreset {
			// 内置 parrot：ALPN/扩展/PQ key_share 等全部由 uTLS 依据预设生成。
			uconn = utls.UClient(rawConn, cfg, helloID)
		} else {
			uconn = utls.UClient(rawConn, cfg, utls.HelloCustom)
			if err := uconn.ApplyPreset(spec); err != nil {
				rawConn.Close()
				return nil, err
			}
		}
		if err := uconn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		return uconn, nil
	}
}
