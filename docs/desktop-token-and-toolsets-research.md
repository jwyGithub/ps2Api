# Postman 桌面端 token 采集与 toolsets 端点逆向研究

> 本文档汇总 2026-09-22 ~ 2026-09-24 期间对 Postman 桌面端/网页端的逆向分析、接口实测与 ps2Api 接入实现。
> 内容分四部分：**桌面 token 产线**（批量注册 + 双 token 采集）、**试用机制**、**toolsets 端点**（claude-opus-5）、**Agent Mode 原生工具目录**。
> 所有结论均经真机实测验证；被证伪的路径也如实记录，避免重复踩坑。

---

## 目录

1. [双认证通道：/chat（Agent Mode）与 toolsets](#1-双认证通道)
2. [批量注册产线（run_pipeline.mjs）](#2-批量注册产线)
3. [桌面 token 采集：PKCE handover 链](#3-桌面-token-采集)
4. [计费与双 token header](#4-计费与双-token-header)
5. [试用机制](#5-试用机制)
6. [toolsets 端点（claude-opus-5）](#6-toolsets-端点)
7. [Agent Mode 原生工具目录（147 个）](#7-原生工具目录)
8. [availableSkills 字段实验](#8-availableskills-实验)
9. [ps2Api 接入实现](#9-ps2api-接入实现)
10. [附属文件索引](#10-附属文件索引)

> **toolsets 端点的独立接入文档**（规格 / 模型枚举 / 限频退避 / ps2Api 行为 / 验证命令）：[toolsets-endpoint.md](toolsets-endpoint.md)。本文档 §6/§9 为其背景与实测记录。

---

## 1. 双认证通道

Postman AI 有两条独立的模型通道，认证方式和模型白名单互不相同：

| | `/chat`（Agent Mode） | `/_gw/toolsets/v1/messages`（Toolsets） |
|---|---|---|
| 端点 | `gateway.postman.com/chat` | `https://<subdomain>.postman.co/_gw/toolsets/v1/messages` |
| 协议 | Postman 自定义（input.query 折叠 + devModeOptions + conversationId 会话） | **Anthropic Messages 原生**（model 三段式 + thinking + tool_use） |
| 必要头 | `x-pstmn-req-service: agent-mode-service` | `x-pstmn-req-service: ai-toolsets`（**缺它一律 404**） |
| 认证 | 桌面型：双 token；web 型：postman.sid cookie | 双 token 或 web cookie 均可 |
| 模型白名单 | CLAUDE_OPUS_48_BEDROCK / GPT_56_SOL 等（见 `internal/provider/models.go`） | claude-opus-5 / claude-sonnet-5 / claude-opus-4-8 |
| 会话粘性 | conversationId（服务端会话） | 无（每次全量 messages，上游自管上下文） |
| credits 计费 | usage 事件（limit/usage/usageState） | 无 credits 事件（仅 token 数） |

**关键坑**：
- `/chat` 对白名单外模型返回流内 `INPUT_VALIDATION_ERROR: Forbidden`，且 **usage 事件先发、failure 随后**——判定模型是否可用必须看到 `textContent`，只看 HTTP 200 / usage 事件会误判。
- toolsets 对未知模型返回标准 `not_found_error` 404。
- toolsets 缺 `ai-toolsets` service 头时 404，与认证无关。

---

## 2. 批量注册产线

产线脚本位于 `Desktop/postman-agent/`（三件套）：

```
run_pipeline.mjs          # 批量编排：注册 → 采 token → 开试用 → 导入 ps2Api
detect_system_edge.mjs    # 注册底座（本目录改版，源出于 postman2api-go 项目）
append_desktop_token.mjs  # PKCE handover 采桌面双 token
```

流程（每轮约 6-8 分钟，全程无人值守）：

```
1. 创建 moemail 邮箱（mail.08050611.xyz API，域名 janone.de5.net / sider.de5.net）
2. 启动隔离 Edge（随机 debug port + 独立 user-data-dir）
3. 注册：自动填表 → Turnstile 物理点击过双关 → moemail 自动收验证码
4. postman.sid + handshake（user_id / workspace_id）落袋
5. PKCE handover 采桌面双 token（见 §3）
6. 开 Solo 试用（400k credits / 7 天，见 §5）
7. 自动导入 ps2Api 号池（POST /api/accounts/import，带 password + 双 token + account_type）
```

**已证伪/勿重试的路径**：
- **自动登录必被风控**（"Reset your password" 拦截，多账号实测）——账号会话必须在注册流程内自然建立。
- **auth_challenge 不能自造**：challenge 必须走 PKCE init 正规签发（见 §3），自造随机值服务端没登记过，confirm 必 401。
- **app_native 描述符直接进注册页会被拒**（"Something went wrong"）——注册用裸入口，handover 用 PKCE 链。
- **handover/preauth/<hash>/consume 是死路**：verify-account?handover= 出现的 hash 打 preauth consume 404 EntityNotFound（那是另一条流）。
- **synthetic mouse event 过不了 Turnstile**：CF checkbox 在 closed shadow DOM，唯一可行 = CDP 物理点击 + MouseEvent.screenX/screenY 伪造。

**邮箱**：moemail 自建实例（`mail.08050611.xyz`，key 见 run_pipeline.mjs），直连超时须走本地代理 7897。catchmail.io 是另一家服务，别配错对。

---

## 3. 桌面 token 采集

### 3.1 token 的两个来源

| 来源 | 形态 | gateway 接受的 header |
|---|---|---|
| 桌面 App 真机登录（chromiumapp 回调流）| `access_token`(hex128) + `multi_login_token`(hex128)，落盘 `%APPDATA%\Postman\storage\userPartitionData.json` | 单发 `x-access-token` 即可（抓包实证） |
| PKCE handover 签发（注册产线） | 同上，consume 响应 `accessToken` + `multiLoginToken` 字段 | **必须加发 `x-multi-login-token`**，否则流内 `Invalid access token` |

ps2Api 按账号是否带 `multi_login_token` 自适应发送（request.go 桌面分支）。

### 3.2 PKCE handover 全链（逆向 app.asar + 接口实测）

来源：逆向 `app-12.28.6/resources/app.asar` 的 `js/scratchpad/scratchpad.js`（EnterpriseSignup 组件 + AuthCodeModal）与 `main.js`（AuthHandler）。

```
1. 本地生成 PKCE 对（纯客户端）:
   verifier  = randomBytes(32).hex          (hex64)
   challenge = sha256(verifier).hex         (hex64)

2. POST https://identity-api.getpostman.com/api/public/pkce/exchange/init
   UA: Mozilla/5.0 (Windows NT 10.0; Win64; x64) PostmanDesktop/<ver> Electron/<ev> Safari/537.36
      （必须完整 Mozilla 形态，"PostmanDesktop/x" 单独不够 → 400 BadUserAgent）
   body: { challenge, redirect_uri: "postman://auth/callback", target: "login" }
      （target 只认 login；signup → 400 "target not allowed"；enterprise 流用 accounts + authTarget）
   → 200 {"success":true,"redirectUrl":"identity.getpostman.com/client-auth/confirm?auth_challenge=<hex64>&auth_device=app_native&auth_device_version=<ver>"}

3. 浏览器（带认证会话 cookie）导航 redirectUrl
   → confirm → accounts（自动点 accountSelect 链接）→ browser-auth/success?code=<hex64>

4. POST https://identity-api.getpostman.com/api/handover/pkce/<verifier>/consume
   body: {"authorizationCode": "<code>"}，同款 UA
   → {"status":"authenticated", "accessToken":"<hex128>", "multiLoginToken":"<hex128>",
      "session":{...,"token":"<hex128>","hashedToken":"..."}, "refreshToken":{...,"expires_in":1296000}}
```

**重大坑：`session.hashedToken` 陷阱**。consume 响应里 `session.hashedToken`（128hex）与 `session.token`（真 token）并存且键序在前——朴素的「找 128 位 hex」解析会误取 hashedToken（服务端校验哈希，对外无效）→ 一律 `Invalid access token`。修复：先扫精确字段名（`token`/`accessToken`/`multiLoginToken`），hex128 兜底跳过 `hashed*` 键。见 `append_desktop_token.mjs` 的 `deepFindTokens`。

**已证伪**：`browser-auth/init` 已登录会话直接导航 403（它是零 cookie 中转页）；隐身上下文复制全部 cookie 后 init 页能加载但页内 JS 不发 consume 探测（IAM 态不被信任）；`handover` 时效只在注册后数分钟内。

---

## 4. 计费与双 token header

- **AI credits 与 team plan 解耦**：`ai_millicredits` 是 teamuser 级 cumulative 计费，FREE=50000/月，与 plan 无关；客户端代码无 `FREE_USER` 字样（纯服务端判定）。
- credits 增量 = 上游 usage 累计用量 - 账号上次快照（`internal/router/observability.go` `creditsConsumed`）。
- toolsets 端点无 credits usage → Credits=0，chargeKey no-op；额度管控退化为 token 估算。
- 双 token header 规则见 §3.1；ps2Api `request.go` 桌面分支按 `MultiLoginToken` 有无自适应。

---

## 5. 试用机制

### 5.1 Solo Trial（已接进产线 ✅）

**额度 50000 → 400000（8 倍），7 天有效。**

```
POST https://bifrost-https-v10.gw.postman.com/ws/proxy
headers: 双 token + x-entity-team-id:<teamId> + x-app-version + Postman UA
body: {"service":"billing","method":"POST","path":"/api/trials","body":{"id":"client_solo_7_days_trial"}}
→ {"result":"success","trial_id":"solo-7-days-trial","trial_period_end":"<start+7d>"}
```

- 200 即成功；**AI 额度传播延迟 1-2 分钟**（期间 usage 接口 entities 为空、chat 流 userType 仍 FREE_USER，之后变 `PAID_USER / limit:400000`）。
- 重复调用幂等（400 "already on trial"）。
- `x-entity-team-id` 头建议带上（抓包有）。
- `bifrost-https-v4` / `v10` 域均有效。

### 5.2 已证伪的试用路径

- **limited-duration-trial**（右上角 Trial 按钮）：可全自动开启（`/api/organizations/<teamId>/start-limited-duration-trial`，首次 500 空响应但实际生效），plan 变 `sync-team-limited-duration-trial`——**但 AI credits 不涨**（teamuser 级计费与 plan 解耦），已弃用。
- **new-pro-trial**（`POST app.getpostman.com/api/users/<id>/new-pro-trial`）：接口存在且双 token 认证通过，但永远 400 "You must upgrade to the latest version"（已试 12.23.7→12.29.2 + app_version 参数）——只对老版本过渡用户开放，FREE 新号不适用。
- `start-free-collab`：可幂等建团队（200），无额度意义。

**结论**：只有 Solo trial 值得自动化（产线已集成，`run_pipeline.mjs` 的 `startSoloTrial`）。

---

## 6. toolsets 端点（claude-opus-5）

### 6.1 端点规格

```
POST https://<subdomain>.postman.co/_gw/toolsets/v1/messages
headers:
  x-pstmn-req-service: ai-toolsets      ← 必须，缺它 404
  anthropic-version: 2023-06-01
  x-access-token + x-multi-login-token  ← 产线双 token
  x-entity-team-id: <teamId>
  x-app-version: 12.29.2-260923-0231
body: 标准 Anthropic Messages API
  model: "claude-opus-5/anthropic/anthropic-messages"   ← 三段式 <name>/<provider>/<api>
  max_tokens / system[] / messages[] / tools[] / tool_choice / thinking{type,budget_tokens} / stream
响应: 标准 Anthropic SSE（message_start/content_block_delta/thinking+signature/tool_use/message_delta）
      或非流式 Anthropic JSON
```

### 6.2 实测模型枚举（2026-09-24）

| model | /chat | toolsets |
|---|---|---|
| claude-opus-5 | ❌ Forbidden | ✅ 200 真实回复 |
| claude-sonnet-5 | 未测 | ✅ 200 |
| claude-opus-4-8 | ✅（对照）| ✅ 200 |
| claude-opus-5-1 / claude-haiku-5 | — | ❌ not_found_error |
| claude-opus-5-5 | ❌ Forbidden | ❌ not_found_error（含裸名/多 provider 后缀）|
| gpt-6-astra | ❌ Forbidden | ❌ not_found_error（含裸名/多 provider 后缀）|
| gpt-5.6 / gemini-3-pro（走 anthropic-messages）| — | ❌ not_found_error |
| claude-opus-5/openai/chat-completions | — | ❌ not_found_error |

### 6.3 限频

连续 ~6 请求后交替 `429 rate_limited` 与 `upstream_unavailable`（后者也可能以 200 + error JSON 出现）。接入侧必须退避——`provider/toolsets.go` 的 `doWithRetry` 做了 2 次重试（8s/16s），API 层另有换号重试（最多 3 号）。

---

## 7. 原生工具目录

Agent Mode 的原生工具目录（`nativeToolsHash` 指纹的内容）**不在桌面 asar 里**（12.28.0/12.28.2/12.28.6/12.29.2 四版本解包全搜过，0 命中）——它是 AI 面板 webview 远程 bundle 的字面量。

**提取来源**：本机运行后 Postman 的 V8 Code Cache：
`%APPDATA%\Postman\Partitions\<uuid>\Code Cache\js\*.js`
（二进制读 → ASCII strings → camelCase 名 + 大写描述对正则抽取 + camelCase 词频交叉验证。）

**完整清单（147 个）**：`Desktop/postman-agent/postman_native_tools.txt`。分组：
- 文件系统/shell：executeShellCommand, readFile, listDirectory, searchInFiles, researchInFiles, createFile, editFile, deleteFile, openFolderPicker
- Postman 实体 CRUD：collections/requests/folders/environments/examples/webhooks/workspaces/specifications/documents/variables
- 数据：datasets 全家桶、datafiles、queryApiCatalog（SQL）
- 运行：collection runs、performance test、Flows、mocks（classic/code/cloud/simulation）
- 消息协议：MQTT/WebSocket/Socket.IO/gRPC/GraphQL
- 其他：askUser, searchPostman, browserTakeScreenshot, getBrowserInteractions

**推论**：`excludedTools` 语义 = 「不给模型看」。网关客户端（Claude Code 等）没有这些工具的本地实现时，必须把文件系统/shell 组加进 excludedTools，否则模型发起调用、客户端回 `No such tool available: searchInFiles`（2026-09-23 线上事故）。已修：`localmode.go` 的 `desktopLocalModeExcludedTools` 头部追加 4 个工具。

---

## 8. availableSkills 实验

试图把客户端（Claude Code）的 47 条 skills 清单放进 `availableSkills` 让上游模型感知——**失败**。

| 形态 | 服务端 | 模型可见 |
|---|---|---|
| `[]`（基线）| 200 | 否 |
| 字符串名字数组 ×47 | 200 | **否**（NONE）|
| `[{name, description}]` ×47 | 200 | **否** |
| `[{id:...}]` | 200 | 同上无理由可见 |
| `'not-an-array'` / `[{foo:'bar'}]` | 200 **无任何 schema 校验** | — |

**结论**：`availableSkills` 是纯透传字段，语义为「云端 workspace 真实存在的 Postman skills 资源引用清单」——服务端按引用从 workspace 拉内容注入 prompt，匹配不到云端资源就静默忽略。桌面 asar 与 webview cache 里该词出现 0 次（客户端恒发 `[]`）。

**模型感知 skills 的唯一可用通路 = `input.query` 头部的 `[System skills]` 段**（ps2Api `messages.go` 已实现，清单+使用指令绑定在 capUpstreamQuery 头部保留区，永不落入中段省略）。

---

## 9. ps2Api 接入实现

toolsets 端点已按**原生透传**方式接入（客户端与上游同为 Anthropic 协议，零转换损耗）：

| 文件 | 内容 |
|---|---|
| `internal/provider/toolsets.go` | `ToolsetsProvider`：Chat/StreamChat 透传、model 三段式改写/回写、限频退避（2 次 8s/16s）、错误分类（AuthFailed/RateLimited/RequestRejected） |
| `internal/api/toolsets.go` | `/v1/messages` 分流 handler：号池选号 → 透传 → 换号重试（≤3 号）→ 流式延迟开流；请求日志直写（endpoint=anthropic-toolsets） |
| `internal/api/anthropic.go` | 入口 `IsToolsetsModel` 分流；`normalizeModel` 特例（防 opus-5 被通用规则错改成 sonnet-4-6） |
| `internal/provider/models.go` | `PostmanModels` 追加 claude-opus-5 / claude-sonnet-5（`/v1/models` 可见） |
| `internal/router/router.go` | `Router.Toolsets` 接线（复用 Provider 代理池/cookie jar） |
| `internal/router/account.go` | 导出 `SelectAccount` 包装 |

行为：
- `/v1/messages` 请求 `claude-opus-5`/`claude-sonnet-5` → 原生透传（thinking/signature/图片 blocks/工具调用零损耗）
- OpenAI/Responses 端点请求这两个模型 → "Invalid model"（未做转换，明确报错）
- toolsets 请求 Credits=0（无 usage 事件），chargeKey no-op；面板请求日志可见（endpoint=anthropic-toolsets）
- 单测：`rewriteModelForUpstream` / `rewriteModelInResponse` / `IsToolsetsModel`

---

## 10. 附属文件索引

均在 `Desktop/postman-agent/`：

| 文件 | 用途 |
|---|---|
| `run_pipeline.mjs` | 批量注册编排器（入口） |
| `detect_system_edge.mjs` | 注册底座（本目录改版：moemail + 流内网络监听） |
| `append_desktop_token.mjs` | PKCE handover 采双 token（deepFindTokens 已修 hashedToken 坑） |
| `postman_native_tools.txt` | 147 个原生工具清单 |
| `tools.txt` | 用户抓包：toolsets /v1/messages 原始 curl + Anthropic SSE 响应 |
| `出站请求体.json` | /chat 出站请求体样本（分析 thirdParty/availableSkills 用） |
| `入站请求体.json` | /v1/messages 入站请求体样本（含 skills 清单原文） |
| `account.json` | 产线产出账号（password + 双 token + account_type） |
| `check_trial.mjs` / `diag_handover.mjs` / `pipe_*.txt` | 排查过程产物，可删 |

Memory 交叉引用（`.claude/projects/.../memory/`）：`toolsets-opus5-endpoint`、`pkce-handover-breakthrough`、`desktop-token-register-pipeline`、`postman-native-tools`、`postman-trial-analysis`、`available-skills-experiment`。
