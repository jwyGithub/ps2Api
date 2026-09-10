# 会话粘性断裂与重放折叠丢任务：分析与修复

> 排查日期：2026-09-10。现象：agent 对话进行 2-3 轮后，模型回复「没有收到实际任务请求」然后结束。完整 trace 在 `data/traces/anthropic/2026-09-10`（7 个 jsonl，id 2542-2548 与线上 request_logs 一一对应）。

---

## 1. 事故链（三个缺陷叠加）

| # | 缺陷 | 位置 | 作用 |
|---|---|---|---|
| 1 | 指纹污染：客户端流切断重试时注入一次性纯文本块「Your response above was cut off mid-stream. Resume...」，`stableFingerprintText` 只剥 `<system-reminder>`/`<total_tokens>` 包装、不剥它 | conversation.go | 该轮指纹带毒，下一轮前缀匹配必失配 |
| 2 | 失效株连：TOOL_RESPONSE 被上游消费后回空流 → `SessionCorrupt` → `InvalidateConversation` 把该会话全部前缀映射**连同 owner 归属映射一起删** | convstore.go / redisstore.go | 干净旧前缀也被删光，粘性彻底断裂 |
| 3 | 折叠丢任务（症状直接根因）：降级重放时 42K 的 system 消息在 splitMessages 折叠分支**不设预算全量渲染**，独占 capUpstreamQuery 的头部 30%，原始任务被推进中段省略区 | messages.go | 模型只见系统提示开头与最近 tool results，回复「没收到任务」 |

时间线：第4轮客户端流切断 → 第5轮注入续写提示（缺陷1，指纹带毒）+ TOOL_RESPONSE 空流 → SessionCorrupt 失效（缺陷2，株连删光）→ 第6/7轮 StickyAccount 永久失配，每轮换号开新会话全历史重放（缺陷3），出站 query 9900 rune 且任务文本完全缺失。

注：第6轮账号 118 恰好烧光额度（quota_remaining=0 → exhausted）、第7轮号池选 124，属设计内行为，非 bug。

## 2. 修复

### 修复A：重放折叠保住原始任务（messages.go + types.go）

折叠路径（conversationId=null）逐段设预算 + 任务后置渲染：

- **预算**（types.go 新常量，truncateMiddleRunes 保头保尾省中段）：
  - `FoldedSystemBudgetRunes = 2000`：单条 system 消息；
  - `FoldedTextMsgBudgetRunes = 2000`：单条历史 user/assistant 文本；
  - `FoldedTailToolResultRunes = 4000`：重放模式下待处理 tool-tail（仅重放路径；命中已有会话时 tool-tail 仍不截断）。
- **原始任务后置渲染**：折叠范围内首条非 tool-result 的 user 消息从时间序拎出，以 `[User (original task)]` 标注渲染在紧贴最新一轮之前。
- **不变量**：任务(≤2000) + tail(≤4000) + 指令(≈150) ≈ 6150 < capUpstreamQuery 尾部保留窗口 ≈ 6800 → 任务永远存活，不会被任何单段挤进省略区。

### 修复B：会话失效保留粘性归属（convstore.go / redisstore.go / conversation.go）

`InvalidateConversation` 只删 (账号,指纹)→conversationId 的会话映射与 pending toolCallId 组映射，**保留 指纹→账号 的 owner 归属映射**（memory 与 redis 两实现同步改）。

理由：会话损坏丢的只是 Postman 服务端上下文，账号本身没坏。保留粘性 → 下一轮仍回原账号、`LookupConversation` 落空降级为 USER_QUERY 重放一轮 → `RememberConversation` 回存干净指纹 → 恢复增量模式（**一轮自愈**），而不是每次会话损坏都换号冷启动。Redis 侧 owner 键保留后反向索引(ownSet)条目依然准确，无需收缩。

## 3. 修复：指纹忽略客户端续写提示块（缺陷1）

污染文本不在本仓库注入——是客户端 agent（Claude Code 等）流被切断后自动重试时自己写进本轮 user 消息的一次性纯文本块（"Your response above was cut off mid-stream. ..."），下一轮同一消息里即消失。trace 比对证实：第5/6轮 msg[6] 的 tool_result 内容逐字节一致，唯一指纹差异就是该块（system-reminder 块 stableFingerprintText 本就剥掉）。

修复（content.go + conversation.go）：指纹计算改走 `ExtractStableText`——按块跳过匹配 `^\s*your response above was cut off`（大小写不敏感、只锚定开头短语、整块跳过）的 text 块，客户端后续措辞调整不影响；不影响发往上游的正文。带毒轮回存的会话，干净轮前缀仍命中。

## 4. 设计说明：capUpstreamQuery 仍保留

修复A 的折叠预算只覆盖「降级重放」路径；「直接命中已有会话」时 query 只发最新一轮原文（不折叠、不预算），若客户端单轮输入本身超长（如用户贴一大段日志），仍由 capUpstreamQuery 兜底压回 10000 rune，否则上游 INPUT_VALIDATION_ERROR 拒收。中段省略标记属设计内兜底，非缺陷。

| 路径 | 谁负责压长度 |
|---|---|
| 降级重放（conversationId=null） | 修复A 的折叠预算，正常到不了 cap |
| 命中已有会话（单轮原文直发） | capUpstreamQuery 兜底 |

## 5. 验证

- 新增 `TestFoldedReplayKeepsOriginalTaskUnderOversizedHistory`：42K system + 26K tool-tail + 任务埋在 6500 字 SDK 输出之后，断言任务/系统头尾/tool 标注全部存活且 query ≤ 10000 rune。
- 新增 `TestInvalidateConversationKeepsStickyOwner`：失效后 conv 映射清空、owner 粘性保留、回存即恢复命中。
- 新增 `TestFingerprintIgnoresClientCutoffNotice`：带续写提示块的轮次与干净轮指纹相等；带毒轮回存会话、干净轮前缀仍命中。
- 更新 `TestBuildBodyCapsOversizedQueryToUpstreamLimit` 至新契约（system 折叠时即截预算，不再依赖 cap 的 omit 标记）。
- `go vet` + `go test ./...` 全绿。
