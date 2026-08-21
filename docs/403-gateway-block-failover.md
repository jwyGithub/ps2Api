# 生产环境 403（Cloudflare）挂起问题：分析与修复

> 本文档记录一次生产事故的根因分析与修复方案，并以**语言无关**的方式描述设计，方便后续迁移到其他语言（Node/Python/Rust 等）时作为参考。
> 涉及服务：Postman → API 的 HTTP 代理/网关（Go 实现）。

---

## 1. 现象（Symptom）

- 生产服务在命中 `Postman gateway rejected request (403, Cloudflare)` 后**永久挂起**。
- 客户端（终端 / 模型）拿不到任何响应，也拿不到明确的失败信号，会话卡死。
- 重试无效：同一请求反复失败。

---

## 2. 根因分析（Root Cause）

分析历经三个阶段，最终修正了对 403 触发条件与修复手段的认知：

### 2.1 403 的真正原因 = 请求体里累积的类 HTML/JS 文本触发 Cloudflare WAF 托管规则

> **修正说明一**：早期版本据「69 次 403 集中在 4 个账号」推断为**账号身份**风控。随后用真实 403 日志复核，该结论被推翻——见下。

- **不是字节数**：真实 403 日志显示，被拦与成功请求的**请求体字节数区间高度重叠**，同等体积（乃至更小）的请求也会被拦。字节大小**不是**触发条件。
- **不是账号身份**：同一账号既有成功也有被拦；换出口 IP 无效。账号并非判定维度。
- **真正的判别式 = tool_use ↔ tool_result 往返轮次**：观测中 **13 轮左右**的对话能干净地把「被拦」与「成功」分开。随着往返增多，请求体里累积的**原始文件转储 / 网页抓取等 tool_result 正文**里的**类 HTML/JS 标记文本**越来越多，命中 Cloudflare WAF 的**托管内容规则（managed content rule）**而返回 403（HTML 挑战/阻断页）。
- 换言之：**诱因是请求体的「内容形状」（累积的标记文本），而非体积、也非账号或出口 IP。** 观测到的 Cf-Ray 均为 `-LAX`，与网络维度无关，符合「内容规则命中」而非「网络/账号封禁」。

### 2.2 修正说明二：压缩 + 换号是错误的修复，会导致「上下文丢失/失忆」

> **修正说明二**：曾据 2.1 引入「压缩 tool_result 正文后重试、压不动再换号」的方案。用 52 条真实 trace 复核后，该方案被推翻——它**制造了新的、更严重的故障：会话降级失忆**。

- **续聊只发增量，压缩历史无用**：Postman 的 **TOOL_RESPONSE** 续聊路径，真正出站的只有**最新一轮的 tool_result 增量**，完整历史留在 Postman **服务端会话**里。压缩「历史里的旧 tool_result」根本不进入出站请求体，对减小 403 命中面**毫无帮助**。
- **压缩改写 `req.Messages` → 破坏会话指纹 → 静默换号**：会话粘性 / 会话查找都对每条消息的正文做 `sha256` 指纹。压缩一旦截断 tool_result 正文，指纹即变 → `StickyAccount` / `LookupConversation` 全部落空 → 请求被轮询到**另一个账号**。
- **换号后必然降级为 USER_QUERY + 截断**：新账号没有该 `conversationId` 的服务端缓存，native TOOL_RESPONSE 匹配不上 → 整段历史被拍扁进单条 `seedingMessages` 并截断到 `MaxQueryLen`（约 9500 字）。205 条消息 → 1 条 9500 字残段，模型**直接失忆**。
- 结论：对**续聊**而言，换号本身就是错误——它必然丢失服务端会话上下文。**续聊遇 403 必须钉住原账号原样重试，绝不换号、绝不改写请求体。** 换号 failover 只对**没有服务端会话可丢的新对话**才安全。

### 2.3 为什么会永久挂起

两个缺陷叠加：

1. **重试策略无效**：旧逻辑遇 403 只对**同一粘性账号**、用**同一份请求体**重试 3 次；后续「压缩 + 换号」版本又引入了 2.2 的降级失忆问题。
2. **流式协议未干净收尾**：流式在联系上游**之前**就先发了 `message_start`；失败后只发 `event: error` 就 `return`，**缺少 `message_stop`**。客户端等不到流终止事件 → 永久挂起。

---

## 3. 修复方案（Fix）

### 3.1 按「是否续聊」分流：续聊钉住原号重试，新对话才换号

核心是用 `HasReusableHistory(messages)`（消息里是否含 assistant / tool / Anthropic tool_result）把请求分成两类，对 `GatewayBlocked` 采取**截然不同**的策略：

- **续聊（有可复用历史）** —— 服务端已有会话、绑定在原账号：
  - **钉住原账号**（`selectAccount` 的 pinned 参数），只允许一次退避重试；
  - **绝不换号**（换号必丢服务端会话 → 降级 USER_QUERY + 历史截断 → 失忆）；
  - **绝不改写 `req.Messages`**（改写会破坏会话指纹 → 触发静默换号与降级）；
  - 第一次 403 后，保留 `conversationId`、`toolCallGroupId` 和工具结果，只移除摘要源码并发送紧凑 third-party schema；第二次仍被拦则返回**明确的网关拦截错误**。
- **新对话（无可复用历史）** —— 无服务端会话可丢，换号安全：
  1. **排除当前账号** + 给它打**冷却标记**；
  2. 退避（backoff）后 **failover 到下一个健康账号**；
  3. 账号**全部耗尽**时，返回**明确的网关拦截错误**（带 `GatewayBlocked` 标记），而不是笼统的 "no accounts"。

> **压缩边界**：不改写历史消息，也不截断 `req.Messages`。仅在 native `TOOL_RESPONSE` 的降级重试中移除重复的摘要源码并压缩 third-party schema；首轮请求仍保留完整 Web 工具定义。

### 3.2 错误分类

区分三类，分别处置：

- `GatewayBlocked`（Cloudflare/网关拦截）：按 §3.1 分流（续聊原号重试；新对话换号）。
- `RequestRejected`（请求本身非法，如坏请求/工具名冲突）：账号健康，直接返回、不重试、不换号、不标记。
- 其余（额度耗尽 / 限流 / 鉴权 / 瞬时错误）：按既有逻辑标记账号并换号。

### 3.3 账号池冷却（自动降级 + 自动恢复）

- 账号池新增**网关冷却表**与 `MarkGatewayBlocked(accountID)`。
- 选号 `Next()` 在冷却期内**优先跳过**被烧账号；若全部处于冷却，则兜底仍可返回（避免完全无号可用）。
- 冷却到期后账号**自动恢复**参与调度 → 被烧号自动降级、自愈。

### 3.4 流式统一「延迟开流 + 干净收尾」

三个流式处理器（Anthropic / OpenAI Chat / Responses）统一改为：

- **延迟开流（deferred start）**：等**首个真实增量**到达后，才提交 HTTP `200` 与首个流事件。
- **产出前失败**：
  - **Anthropic**：**修正**——不再回退为 HTTP 503 JSON。Anthropic 协议的 agent 终端已开着流式连接等 SSE 生命周期事件，并不把 503 JSON body 当作流终止信号，于是「一直在请求中」等不到 `message_stop` 而永久挂起。故对 Anthropic 流式请求，产出前失败也照常开流（补 `message_start`）再走下方「已开流后失败」的终止序列，**保证终端必定收到 `message_stop`**。
  - **OpenAI Chat / Responses**：仍可回退为**干净的 HTTP 503 JSON 错误**（此时还没发任何流事件，OpenAI 客户端会把非 200 当作终止）。
- **已开流后失败**：补发协议对应的终止事件干净收尾：
  - Anthropic：`message_stop`
  - OpenAI Responses：`response.failed`
  - OpenAI Chat（SSE）：`[DONE]`
- 效果：终端能明确「任务结束」，模型知晓「本次未产生任何输出、可安全重做」，不再挂起。

### 3.5 可配置项

- 配置：**网关拦截冷却(秒)**（`gateway_cooldown_seconds`），默认 `300`。仅作用于**新对话**换号 failover 后对被拦账号的冷却。
- 账号 Cookie 按 `account + upstream host + egress` 隔离，响应中的 `Set-Cookie` 仅回收到对应账号和出口。

---

## 4. 迁移到其他语言时的要点（Checklist）

移植时请保留以下**行为契约**，而非具体 Go 实现：

1. **错误分类可区分**：至少区分网关拦截、请求非法和认证失败，避免无意义换号。
2. **续聊单次降级重试**：保持账号、会话 ID、工具调用配对和原始消息；只净化摘要、压缩 third-party schema，并固定首次出口。
3. **新对话才允许 failover**：网关拦截后排除账号并冷却，账号耗尽返回明确错误。
4. **Cookie 隔离**：Cookie 按账号、上游主机和出口隔离，禁止复用抓包中的旧 Cloudflare Cookie。
5. **流式必须延迟开流并干净收尾**：失败后发送协议规定的终止事件。

---

## 5. 已知取舍与后续建议（Trade-offs）

- **降级重试会丢弃 third-party schema 的描述和参数细节**：保留工具名和宽松 object 参数，以维持模型继续发起调用；极端依赖参数 schema 的工具可能需要客户端自行修正参数。
- **续聊不会换号**：原账号连续被拦时直接终止，由客户端稍后继续，避免服务端会话丢失。
- **Cookie 只在进程内保存**：重启后需要重新从上游获取，不持久化敏感 Cloudflare Cookie。

---

## 6. 涉及文件（Go 参考实现）

| 文件 | 改动 |
|------|------|
| `internal/router/router.go` | `Chat` / `Stream` 遇 `GatewayBlocked` 时：续聊固定原账号，仅一次降级重试并固定首次出口；第二次失败明确终止；新对话才排除账号并 failover。**不就地改写 `req.Messages`** |
| `internal/pool/pool.go` | 新增网关冷却表与 `MarkGatewayBlocked`；`Next` 冷却期内跳过被烧账号，全冷却兜底可用，到期自动恢复 |
| `internal/provider/postman.go` | 工具结果摘要净化、降级重试时压缩 third-party schema、按账号/出口复用 Cookie |
| `internal/provider/proxy.go` | 按账号、上游主机、出口隔离 Cookie Jar |
| `internal/api/api.go` | Anthropic / OpenAI Chat 流式处理器改为延迟开流 + 干净收尾 |
| `internal/api/responses.go` | OpenAI Responses 流式处理器改为延迟开流 + 干净收尾 |
| `internal/api/metrics.go` | 新增可配置项「网关拦截冷却(秒)」（默认 300） |
| `internal/router/router_test.go` | 更新旧 403 用例语义；新增「403 → 原号退避重试恢复（不换号、账号仍健康）」「续聊 403 → 钉住原账号重试、绝不换号、`req.Messages` 不被改写」「无历史可复用 → failover 到健康账号成功」「全部被拦 → 返回明确错误且不 emit 任何增量」用例 |

验证：`go build`、`go vet`、`go test ./...` 全部通过。
