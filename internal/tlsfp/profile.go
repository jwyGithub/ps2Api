// Package tlsfp 提供出站 TLS 指纹伪装能力：把一套可配置的 ClientHello 参数
// （cipher suites / 椭圆曲线 / 扩展顺序 / ALPN 等）抽象成 Profile，运行时用
// uTLS 的 HelloCustom 握手接管 net/http.Transport 的 TLS 层，从而在不改业务
// 逻辑的前提下，把出站握手指纹伪装成目标客户端（默认 Node.js / Claude Code）。
//
// 设计要点：
//   - 只替换握手层（DialTLSContext）。ALPN 提供 h2+http/1.1：协商到 h2 时由 tlsfp
//     内的自写 h2 客户端接管（SETTINGS 帧顺序 / 窗口增量 / 伪头顺序本身也是指纹，
//     官方 http2.Transport 改不动），协商到 http/1.1 时回落最小 h1 路径。
//   - 默认模板走 uTLS 内置 Chrome parrot（ClientHelloID=HelloChrome_133）：现代
//     Chrome 的 ClientHello 含后量子 key_share(X25519MLKEM768)、ALPS、
//     compress_certificate(brotli) 与扩展乱序，手搓极易失真，交给 uTLS 逐版校准更稳。
//   - ClientHelloID 为空时回落到下面的自定义字段走 HelloCustom（逐字节可控）。
//   - 所有参数收敛进 Profile，便于入库/按账号切换/对照抓包校准。
package tlsfp

import (
	"fmt"

	utls "github.com/refraction-networking/utls"
)

// Profile 描述一套完整的 ClientHello 指纹模板。字段全部可序列化，便于持久化到
// settings 表并在面板热切换。零值不可用，请以 ChromiumDefault() 为基准派生。
type Profile struct {
	// Name 模板名（面板展示 / 日志标识），如 "nodejs" / "claude-code"。
	Name string `json:"name"`

	// ClientHelloID 指定 uTLS 内置指纹预设的名称（如 "HelloChrome_133"）。
	// 非空时 dialer 直接用该 parrot 握手（见 resolveClientHelloID 登记表），忽略下面
	// 的自定义 cipher/曲线/扩展字段；为空时才用自定义字段走 HelloCustom。
	// 默认模板置为 "HelloChrome_133" 以对齐应用层 UA 与 h2 层的 Chromium 指纹。
	ClientHelloID string `json:"client_hello_id,omitempty"`

	// CipherSuites 密码套件列表，顺序即指纹的一部分（JA3 的密码段）。
	CipherSuites []uint16 `json:"cipher_suites"`

	// CurvePreferences 支持的椭圆曲线 / 命名组（supported_groups 扩展），顺序敏感。
	CurvePreferences []uint16 `json:"curve_preferences"`

	// SignatureAlgorithms signature_algorithms 扩展内容，顺序敏感。
	SignatureAlgorithms []uint16 `json:"signature_algorithms"`

	// ALPN 应用层协议协商列表。为保持指纹自洽，默认仅 ["http/1.1"]。
	ALPN []string `json:"alpn"`

	// KeyShareCurves key_share 扩展预置的曲线（须为 CurvePreferences 前缀子集）。
	KeyShareCurves []uint16 `json:"key_share_curves"`

	// EnableECH 是否附带 GREASE ECH 扩展（Node.js 新版本会带）。
	EnableECH bool `json:"enable_ech"`

	// EnableGREASE 是否在 cipher / 扩展 / 曲线 / key_share / supported_versions
	// 中注入 GREASE 占位（对应 sub2api 模版的 enable_grease）。uTLS 握手时会把
	// 占位替换成随机 GREASE 值。真实抓包带 GREASE 时必须置 true，否则 JA3 首字段对不上。
	EnableGREASE bool `json:"enable_grease"`

	// MinVersion / MaxVersion TLS 版本区间。默认 1.2 ~ 1.3。
	MinVersion uint16 `json:"min_version"`
	MaxVersion uint16 `json:"max_version"`

	// H2 描述当 ALPN 协商到 h2 时，自写 h2 客户端应注入的 HTTP/2 层指纹
	// （Akamai h2 指纹：SETTINGS 值与顺序、连接级窗口增量、伪头顺序）。
	// 仅在 ALPN 含 "h2" 时生效；为空时 h2 引擎回退到一组保守默认值。
	H2 H2Fingerprint `json:"h2"`
}

// H2Setting 是一条 HTTP/2 SETTINGS 参数。ID 用 IANA 编号（0x1=HEADER_TABLE_SIZE、
// 0x2=ENABLE_PUSH、0x4=INITIAL_WINDOW_SIZE、0x6=MAX_HEADER_LIST_SIZE 等）。
// 切片顺序即上线顺序，本身是指纹的一部分，改动务必对照真实抓包。
type H2Setting struct {
	ID  uint16 `json:"id"`
	Val uint32 `json:"val"`
}

// H2Fingerprint 汇总 HTTP/2 连接建立阶段的可指纹面。所有字段顺序敏感。
type H2Fingerprint struct {
	// Settings 客户端 SETTINGS 帧内容，按声明顺序逐条写出。
	Settings []H2Setting `json:"settings"`

	// ConnWindowUpdate 连接级（stream 0）WINDOW_UPDATE 增量。0 表示不额外发送。
	// Chromium 在 SETTINGS 之后紧跟一条 15663105 的连接级窗口增量。
	ConnWindowUpdate uint32 `json:"conn_window_update"`

	// HeaderTableSize HPACK 编码器动态表上限，须与 Settings 中的
	// HEADER_TABLE_SIZE 一致（用于 hpack.Encoder.SetMaxDynamicTableSize）。
	HeaderTableSize uint32 `json:"header_table_size"`

	// PseudoHeaderOrder 请求伪头的发送顺序，如
	// [":method", ":authority", ":scheme", ":path"]（Chromium 的 m,a,s,p）。
	PseudoHeaderOrder []string `json:"pseudo_header_order"`
}

// chromiumH2 返回 Chromium 家族（Chrome / Edge / Electron）当前的 HTTP/2 指纹。
//
// 对应 Akamai 指纹串：1:65536;2:0;4:6291456;6:262144|15663105|0|m,a,s,p
//   - SETTINGS 仅 4 项，顺序：HEADER_TABLE_SIZE → ENABLE_PUSH → INITIAL_WINDOW_SIZE
//     → MAX_HEADER_LIST_SIZE（不含 MAX_CONCURRENT_STREAMS）
//   - 连接级 WINDOW_UPDATE 增量 15663105
//   - 无 PRIORITY 帧
//   - 伪头顺序 :method,:authority,:scheme,:path
func chromiumH2() H2Fingerprint {
	return H2Fingerprint{
		Settings: []H2Setting{
			{ID: 0x1, Val: 65536},   // SETTINGS_HEADER_TABLE_SIZE
			{ID: 0x2, Val: 0},       // SETTINGS_ENABLE_PUSH = 0
			{ID: 0x4, Val: 6291456}, // SETTINGS_INITIAL_WINDOW_SIZE
			{ID: 0x6, Val: 262144},  // SETTINGS_MAX_HEADER_LIST_SIZE
		},
		ConnWindowUpdate:  15663105,
		HeaderTableSize:   65536,
		PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"},
	}
}

// ChromiumDefault 返回一套完整对齐 Chromium 家族（Chrome / Edge / Electron）的默认
// 指纹模板：TLS ClientHello 走 uTLS 内置 parrot HelloChrome_133，h2 层走 chromiumH2()，
// 与应用层 UA/sec-ch-ua 自称的 Edge/Chromium 三层一致，避免 Cloudflare 关联比对露馅。
//
// 关键：ClientHelloID 一旦置为 "HelloChrome_133"，dialer 即直接使用该 parrot 握手，
// 下面的 CipherSuites/CurvePreferences/SignatureAlgorithms/KeyShareCurves/ECH/GREASE
// 等自定义字段【不再参与握手】——它们仅作为清空 ClientHelloID 后回落 HelloCustom 的
// 备用规格保留（当前为 Node/OpenSSL 抓包的历史模版，不代表实际上线指纹）。
func ChromiumDefault() Profile {
	return Profile{
		Name: "Chromium_133",
		// 走内置 Chrome parrot；PQ key_share / ALPS / compress_certificate / 扩展乱序
		// 全部由 uTLS 按真实 Chrome 133 生成，无需在此手工维护。
		ClientHelloID: "HelloChrome_133",
		// ↓↓↓ 以下均为「清空 ClientHelloID 后」HelloCustom 回落用的历史自定义规格
		// （Node/OpenSSL 抓包模版 MacOS_arm64_node_v2430），默认路径不生效。
		CipherSuites: []uint16{
			0x1301, // TLS_AES_128_GCM_SHA256            4865
			0x1302, // TLS_AES_256_GCM_SHA384            4866
			0x1303, // TLS_CHACHA20_POLY1305_SHA256      4867
			0xc02b, // ECDHE_ECDSA_AES_128_GCM_SHA256    49195
			0xc02f, // ECDHE_RSA_AES_128_GCM_SHA256      49199
			0xc02c, // ECDHE_ECDSA_AES_256_GCM_SHA384    49196
			0xc030, // ECDHE_RSA_AES_256_GCM_SHA384      49200
			0xcca9, // ECDHE_ECDSA_CHACHA20_POLY1305     52393
			0xcca8, // ECDHE_RSA_CHACHA20_POLY1305       52392
			0xc009, // ECDHE_ECDSA_AES_128_CBC_SHA       49161
			0xc013, // ECDHE_RSA_AES_128_CBC_SHA         49171
			0xc00a, // ECDHE_ECDSA_AES_256_CBC_SHA       49162
			0xc014, // ECDHE_RSA_AES_256_CBC_SHA         49172
			0x009c, // RSA_AES_128_GCM_SHA256            156
			0x009d, // RSA_AES_256_GCM_SHA384            157
			0x002f, // RSA_AES_128_CBC_SHA               47
			0x0035, // RSA_AES_256_CBC_SHA               53
		},
		CurvePreferences: []uint16{
			0x001d, // X25519   29
			0x0017, // P-256    23
			0x0018, // P-384    24
		},
		SignatureAlgorithms: []uint16{
			0x0403, // ecdsa_secp256r1_sha256   1027
			0x0804, // rsa_pss_rsae_sha256      2052
			0x0401, // rsa_pkcs1_sha256         1025
			0x0503, // ecdsa_secp384r1_sha384   1283
			0x0805, // rsa_pss_rsae_sha384      2053
			0x0501, // rsa_pkcs1_sha384         1281
			0x0806, // rsa_pss_rsae_sha512      2054
			0x0601, // rsa_pkcs1_sha512         1537
			0x0201, // rsa_pkcs1_sha1           513
		},
		// ALPN 同时提供 h2 与 http/1.1：应用层 UA 自称 Edge/Chromium，其网络栈
		// 首选 h2；协商到 h2 时由自写 h2 客户端注入 Chromium 指纹（见 H2 字段），
		// 协商到 http/1.1 时回落现有 h1 路径。顺序 h2 优先与真实浏览器一致。
		ALPN:           []string{"h2", "http/1.1"},
		KeyShareCurves: []uint16{0x001d}, // 仅 X25519，与模版 key_share_groups 一致
		EnableECH:      false,            // 模版无 ECH 扩展
		EnableGREASE:   true,             // 模版 enable_grease=true
		MinVersion:     utls.VersionTLS12,
		MaxVersion:     utls.VersionTLS13, // supported_versions [772,771]
		// h2 层复刻 Chromium（Edge/Electron）——与应用层 UA/sec-ch-ua 对齐，避免
		// Cloudflare 把 h2 指纹与 UA 关联比对时露馅。
		H2: chromiumH2(),
	}
}

// clientHelloIDs 把 Profile.ClientHelloID 的字符串名映射到 uTLS 内置指纹预设。
// 仅登记本项目会用到的预设；需要新版本时在此追加一行即可。
var clientHelloIDs = map[string]utls.ClientHelloID{
	"HelloChrome_133":  utls.HelloChrome_133,
	"HelloChrome_131":  utls.HelloChrome_131,
	"HelloChrome_120":  utls.HelloChrome_120,
	"HelloChrome_Auto": utls.HelloChrome_Auto,
}

// resolveClientHelloID 解析 Profile.ClientHelloID：
//   - 空字符串 → (零值, false, nil)，表示走 HelloCustom 自定义 spec；
//   - 已登记名 → (对应预设, true, nil)，dialer 直接用该 parrot 握手；
//   - 未登记名 → 报错，避免静默退化成非预期指纹。
func resolveClientHelloID(name string) (utls.ClientHelloID, bool, error) {
	if name == "" {
		return utls.ClientHelloID{}, false, nil
	}
	id, ok := clientHelloIDs[name]
	if !ok {
		return utls.ClientHelloID{}, false, fmt.Errorf("tlsfp: 未登记的 ClientHelloID %q", name)
	}
	return id, true, nil
}

// buildSpec 由 Profile 构造 uTLS 的 ClientHelloSpec。扩展顺序按 OpenSSL/Node.js
// 常见排布组织，顺序本身是指纹的一部分，改动需对照抓包。
func (p Profile) buildSpec() *utls.ClientHelloSpec {
	// GREASE 注入：uTLS 会把 GREASE_PLACEHOLDER 在握手时替换为随机 GREASE 值。
	// 曲线 / key_share / supported_versions 的 GREASE 占位一并前置，
	// 保证 JA3 的曲线段与首位 GREASE 与真实抓包结构一致。
	curves := make([]utls.CurveID, 0, len(p.CurvePreferences)+1)
	if p.EnableGREASE {
		curves = append(curves, utls.CurveID(utls.GREASE_PLACEHOLDER))
	}
	for _, c := range p.CurvePreferences {
		curves = append(curves, utls.CurveID(c))
	}

	sigAlgs := make([]utls.SignatureScheme, 0, len(p.SignatureAlgorithms))
	for _, s := range p.SignatureAlgorithms {
		sigAlgs = append(sigAlgs, utls.SignatureScheme(s))
	}

	keyShares := make([]utls.KeyShare, 0, len(p.KeyShareCurves)+1)
	if p.EnableGREASE {
		keyShares = append(keyShares, utls.KeyShare{Group: utls.CurveID(utls.GREASE_PLACEHOLDER), Data: []byte{0}})
	}
	for _, c := range p.KeyShareCurves {
		keyShares = append(keyShares, utls.KeyShare{Group: utls.CurveID(c)})
	}

	supportedVersions := []uint16{}
	if p.EnableGREASE {
		supportedVersions = append(supportedVersions, utls.GREASE_PLACEHOLDER)
	}
	if p.MaxVersion >= utls.VersionTLS13 {
		supportedVersions = append(supportedVersions, utls.VersionTLS13)
	}
	if p.MinVersion <= utls.VersionTLS12 {
		supportedVersions = append(supportedVersions, utls.VersionTLS12)
	}

	exts := []utls.TLSExtension{}
	if p.EnableGREASE {
		exts = append(exts, &utls.UtlsGREASEExtension{})
	}
	exts = append(exts,
		&utls.SNIExtension{},
		&utls.ExtendedMasterSecretExtension{},
		&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
		&utls.SupportedCurvesExtension{Curves: curves},
		&utls.SupportedPointsExtension{SupportedPoints: []byte{0x00}}, // uncompressed
		&utls.SessionTicketExtension{},
		&utls.ALPNExtension{AlpnProtocols: p.ALPN},
		&utls.StatusRequestExtension{},
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: sigAlgs},
		&utls.SCTExtension{},
		&utls.KeyShareExtension{KeyShares: keyShares},
		&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
		&utls.SupportedVersionsExtension{Versions: supportedVersions},
	)
	if p.EnableECH {
		exts = append(exts, utls.BoringGREASEECH())
	}
	if p.EnableGREASE {
		exts = append(exts, &utls.UtlsGREASEExtension{})
	}

	// cipher 段同样前置 GREASE 占位。
	ciphers := make([]uint16, 0, len(p.CipherSuites)+1)
	if p.EnableGREASE {
		ciphers = append(ciphers, utls.GREASE_PLACEHOLDER)
	}
	ciphers = append(ciphers, p.CipherSuites...)

	return &utls.ClientHelloSpec{
		CipherSuites:       ciphers,
		CompressionMethods: []byte{0x00}, // null
		Extensions:         exts,
	}
}
