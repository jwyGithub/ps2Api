# TLS 指纹改造：三层对齐 Chromium（Chrome 133 parrot）

> 本文档记录一次出站 TLS 指纹的改造：把握手层从「手搓 Node/OpenSSL ClientHello」切换为 **uTLS 内置 Chrome parrot（`HelloChrome_133`）**，使应用层 UA、HTTP/2 指纹、TLS ClientHello（JA3/JA4）**三层自洽对齐 Chromium/Edge**，规避 Cloudflare 的关联比对。
> 涉及服务：Postman → API 的 HTTP 代理/网关（Go 实现）。设计以**语言无关**方式描述，便于迁移到其他语言时参考。

---

## 1. 背景与动机（Why）

出站请求会经过 Cloudflare。Cloudflare 的机器人识别不只看单层特征，而是把**三层指纹交叉关联**比对：

| 层 | 指纹 | 观测手段 |
|---|---|---|
| 应用层 | `User-Agent` / `sec-ch-ua` | HTTP 头 |
| HTTP/2 | SETTINGS 值与顺序、连接级窗口增量、伪头顺序 | Akamai h2 指纹 |
| TLS | ClientHello 的 cipher / 曲线 / 扩展顺序 / key_share | JA3 / JA4 / JA4_r |

改造前的状态是**三层不自洽**：

- 应用层 UA 自称 **Edge/Chromium**；
- HTTP/2 层已复刻 **Chromium**；
- 但 TLS 层是**手搓的 Node.js / OpenSSL** ClientHello。

现代 Chrome 的 ClientHello 含**后量子 key_share（X25519MLKEM768）**、**ALPS（application_settings）**、**compress_certificate（brotli）**、**ECH GREASE** 以及**扩展乱序**等特征。手工逐字节复刻这些特征极易失真——一旦 TLS 层暴露为「非浏览器」，就与自称浏览器的 UA 矛盾，触发 Cloudflare 关联比对而被识别。

**结论**：TLS 层不应手搓，应交给 uTLS 按真实 Chrome 版本校准的内置 parrot 维护。

---

## 2. 改造目标（What）

1. 默认出站握手使用 uTLS 内置 **`HelloChrome_133`** parrot，让 JA3/JA4 呈现真实 Chrome 特征。
2. 保留原手搓规格作为「回落备用」，随时可切回逐字节可控的 `HelloCustom` 路径。
3. 直连与经代理（HTTP CONNECT / SOCKS5）两条出口共用同一套指纹，保证自洽。
4. 未登记的指纹名必须**显式报错**，杜绝静默退化为非预期指纹。

---

## 3. 设计与实现（How）

### 3.1 Profile 增加 `ClientHelloID` 字段

`Profile` 是一套可序列化的 ClientHello 模板。新增字段：

```go
// ClientHelloID 指定 uTLS 内置指纹预设的名称（如 "HelloChrome_133"）。
// 非空时 dialer 直接用该 parrot 握手（见 resolveClientHelloID 登记表），忽略下面
// 的自定义 cipher/曲线/扩展字段；为空时才用自定义字段走 HelloCustom。
ClientHelloID string `json:"client_hello_id,omitempty"`
```

**语义**：
- `ClientHelloID` **非空** → 走 uTLS 内置 parrot，`CipherSuites / CurvePreferences / SignatureAlgorithms / KeyShareCurves / ECH / GREASE` 等自定义字段**不参与握手**；
- `ClientHelloID` **为空** → 回落 `HelloCustom`，由 `buildSpec()` 用上述自定义字段逐字节构造 ClientHello。

### 3.2 默认模板改为 Chromium

`NodeDefault()` → **`ChromiumDefault()`**（`Name = "Chromium_133"`），挂 `ClientHelloID: "HelloChrome_133"`。

原 Node/OpenSSL 的 cipher / 曲线 / 签名等自定义字段**保留**在结构体里，但降级为「清空 `ClientHelloID` 后回落 `HelloCustom`」的备用规格（历史模版 `MacOS_arm64_node_v2430`），默认路径不生效。

`ALPN` 保持 `["h2", "http/1.1"]`（h2 优先，与真实浏览器一致）；`H2` 字段继续走 `chromiumH2()`。

### 3.3 指纹名登记表 + 解析函数

用一张显式登记表把字符串名映射到 uTLS 预设，避免拼写错误静默退化：

```go
var clientHelloIDs = map[string]utls.ClientHelloID{
    "HelloChrome_133":  utls.HelloChrome_133,
    "HelloChrome_131":  utls.HelloChrome_131,
    "HelloChrome_120":  utls.HelloChrome_120,
    "HelloChrome_Auto": utls.HelloChrome_Auto,
}

// resolveClientHelloID：
//   - 空字符串 → (零值, false, nil)，走 HelloCustom；
//   - 已登记名 → (对应预设, true, nil)，直接用 parrot 握手；
//   - 未登记名 → 报错，杜绝静默退化。
func resolveClientHelloID(name string) (utls.ClientHelloID, bool, error) { ... }
```

新增版本时，只需在登记表追加一行。

### 3.4 两个 dialer 认 `ClientHelloID`

`NewDirectDialTLS`（直连）与 `NewProxyDialTLS`（经代理）现在都遵循同一分支逻辑：

```go
helloID, usePreset, idErr := resolveClientHelloID(p.ClientHelloID)
var spec *utls.ClientHelloSpec
if !usePreset {
    spec = p.buildSpec() // 仅在需要时构建自定义 spec
}
// ...
if usePreset {
    // 内置 parrot：ALPN/扩展/PQ key_share 等全部由 uTLS 依据预设生成
    uconn = utls.UClient(rawConn, cfg, helloID)
} else {
    uconn = utls.UClient(rawConn, cfg, utls.HelloCustom)
    if err := uconn.ApplyPreset(spec); err != nil { ... }
}
```

- **直连**：标准库拨 TCP → uTLS 握手。
- **经代理**：先按代理协议（`http`/`https` 的 CONNECT，或 `socks5`/`socks5h`）建立到**目标主机**的裸隧道，再在隧道之上用 uTLS 对目标主机握手。这样无论直连还是走代理，上游看到的 ClientHello 都是**同一套**伪装指纹（标准库 `http.Transport.Proxy` 会用 Go 默认指纹完成目标握手，无法满足此点，故自行实现 CONNECT/SOCKS5 拨号并接管 TLS）。

### 3.5 provider 层切换

`internal/provider/fingerprint.go` 的 `activeProfile()` 改用 `ChromiumDefault()`。对上层 `http.Client{Transport: ...}` 接口不变——传输层仍是 `FingerprintRoundTripper`，握手后按 ALPN 分流（h2 走自写 Chromium 指纹 h2 客户端并按 host 池化复用；http/1.1 回落最小 h1 路径）。

---

## 4. 涉及文件清单

| 文件 | 改动 |
|---|---|
| `internal/tlsfp/profile.go` | `NodeDefault()`→`ChromiumDefault()`（挂 `HelloChrome_133`）；新增 `ClientHelloID` 字段、`clientHelloIDs` 登记表、`resolveClientHelloID()`；原手搓字段降级为 `HelloCustom` 回落备用；更新包头设计说明 |
| `internal/tlsfp/dialer.go` | 直连 dialer 认 `ClientHelloID`：非空用内置 parrot，为空回落 `HelloCustom + buildSpec`；仅在需要时构建 spec |
| `internal/tlsfp/proxydialer.go` | 经代理 dialer 同上分支逻辑；隧道之上对目标主机握手，与直连指纹一致 |
| `internal/provider/fingerprint.go` | `activeProfile()` 改用 `ChromiumDefault()`，更新注释 |
| `internal/tlsfp/dialer_test.go` | 测试改名；放宽 ALPN 断言说明（默认模板供 h2） |

---

## 5. 验证（Verification）

联网 JA3/JA4 探测（`tls.peet.ws`；无外网自动 skip）实证握手已是**真正的 Chrome 133**：

- **JA4 = `t13d1516h2_8daaf6152771_d8a2da3f94cd`**
  - `t13…h2` = TLS 1.3 + h2 ALPN，Chrome 家族典型值。
- 扩展齐全出现 Chrome 命门：
  - `44cd` = ALPS（application_settings）
  - `fe0d` = ECH GREASE
  - `001b` = compress_certificate（brotli）
  - `002b / 002d / 0033` = supported_versions / psk_key_exchange_modes / key_share
- **key_share group `4588` = X25519MLKEM768** 后量子密钥，正是当前 Chrome 的招牌。

命令：

```bash
go build ./...
go vet ./internal/tlsfp/... ./internal/provider/...
go test ./internal/tlsfp/ ./internal/provider/...
# 联网实证（可选）
go test ./internal/tlsfp/ -run TestJA3Fingerprint -v
```

`build` / `vet` / `tlsfp`+`provider` 测试全绿。

---

## 6. 改造后三层指纹状态

| 层 | 现状 |
|---|---|
| 应用层 UA / sec-ch-ua | Edge/Chromium ✅ |
| HTTP/2 指纹 | Chromium ✅ |
| TLS ClientHello（JA3/JA4） | **Chromium（Chrome 133 parrot）** ✅ |

---

## 7. 方案取舍与已知取舍点（Trade-offs）

- **放弃手搓、改用 parrot**：现代 Chrome 的 PQ key_share、ALPS、compress_certificate、扩展乱序手搓几乎必失真，交给 uTLS 逐版校准更真实。代价是默认路径不再走 `buildSpec` 逐字节掌控——但那套逐字节掌控恰是改造前 TLS 层露馅的根源。**保留 `HelloCustom` 回落**（清空 `ClientHelloID` 即可），随时可切回。
- **版本号差异**：uTLS 内置最高到 `HelloChrome_133`，而应用层 UA 自称 151。JA4 的 `h2` 结构在 Chrome 131→151 间保持稳定，Cloudflare 主要比对**结构化的 JA4 / JA4_r** 而非精确版本号，实战无碍。若后续要追平版本号，待 uTLS 升级后把登记表里的 `HelloChrome_133` 换成更新预设即可。

---

## 8. 后续可选项（Future Work）

- **面板热切换**：`Profile` 已可 JSON 序列化，`activeProfile()` 目前固定返回默认模板；后续可改为从 settings 表读取，实现按账号/运行时切换指纹模板。
- **跟随 uTLS 升级**：uTLS 发布更高版本 Chrome 预设后，在 `clientHelloIDs` 追加一行并更新默认模板，即可追平应用层 UA 版本号。
