# 超长 USER_QUERY 分片续传（Large-Query Chunked Continuation）

> 本文档记录一次功能实现的设计与取舍，并以**语言无关**的方式描述行为契约，方便后续迁移到其他语言（Node/Python/Rust 等）时作为参考。
> 涉及服务：Postman → API 的 HTTP 代理/网关（Go 实现）。

---

## 1. 背景与问题（Problem）

- Postman 上游 `/_gw/chat` 对 `input.query` 有**硬上限 ≈ 10000 rune**（按字符数而非字节数计；`MaxUpstreamQueryRunes = 10000`）。超限返回 `"Invalid input query. If you have a large file, try importing it..."` 被拒收。
- 现有兜底 `capUpstreamQuery` 对超长 query 做**中段截断**（保头留尾、中间插标记），会**丢失中间内容**。用户 `@` 引用大文件时，中段信息静默丢失，模型据残缺输入作答。
- 目标：在不改变上游硬上限的前提下，把一条超长 query **完整**送达模型。

---

## 2. 方案（Solution）：按 conversationId 跨轮喂片

把超长 query 按 rune 切成 N 片，顺序喂进**同一个 `conversationId`**，利用 Postman 上游「按会话跨轮保留上下文」的能力把完整内容拼回模型侧：

- **前 N-1 片＝前置片（priming）**：每片用提示语包裹（`[大输入分片 K/N] …请只回 "ACK"、先别作答也别调工具…`），顺序发出。
  - 用 **no-op emit 丢弃模型的 ACK 回复**，只取回该轮的 `conversationId`。
  - 下一片的 `conversationId` **链式续接**上一片的返回值（首片为 `null`，即冷启动）。
- **最后一片＝终片（final）**：用提示语包裹（`[大输入分片 N/N，最后一部分] …请结合前面各片正式作答（可正常调用工具）…`），作为**真实流式响应正常 emit 给客户端**。
  - `res.ConversationID` 落在**终片返回的真实会话 ID** 上，外层 `RememberConversation` 据此把消息指纹映射到它 → **续聊无缝衔接**。

### 2.1 触发条件（收窄范围）

分片**只对「普通 USER_QUERY」**开放（`plan.chunkable == true`），即：

```
chunkable = !useNativeResponse && !toolTail(req.Messages)
```

- ✅ 全新 / 续聊的用户文本（典型：`@` 大文件引用）。
- ❌ 原生 `TOOL_RESPONSE`（工具结果续期）——**不分片**。
- ❌ tool-tail（把工具结果作为 USER_QUERY 交回）——**不分片**。

> **为什么排除工具路径**：工具结果跨多轮 USER_QUERY 喂送**语义可疑**，且会改变上游往返次数，与 **403 网关钉号重试 / 会话粘性** 机制相互干扰（这正是实现过程中两个 router 测试变红的原因，据此收窄后恢复绿）。这些路径继续走单发 + `capUpstreamQuery` 中段截断。

### 2.2 上限与兜底（Fallback）

- `MaxQueryChunks = 8`。每个分片都是一次独立上游 HTTP 往返 + 一次模型 ACK（耗时、耗额度、每次都过一遍 Cloudflare 风控），故必须设上限。
- 8 × (≈9600 rune/片) ≈ **7.7 万字符**，覆盖绝大多数 `@` 大文件引用。
- **分片数 ≥ 2 且 ≤ 8** 才走分片；**超过 8** 就回退到 `capUpstreamQuery` 中段截断，绝不无限膨胀成几十次往返。

### 2.3 切分策略（不切碎结构）

`splitQueryIntoChunks(q, budget)`：

- 每片正文预算 `budget = (MaxUpstreamQueryRunes-100) - QueryChunkWrapperReserve`（`QueryChunkWrapperReserve = 256`，为包裹提示语预留 rune 余量，保证「包裹语 + 正文」整体仍落在 10000 rune 硬上限内）。
- 优先在预算窗口**后 40%** 内的段落分隔 `\n\n` 处断，其次单个换行 `\n`；窗口内无自然边界才**硬切**到 budget。
- 只在后 40% 找断点，避免为对齐边界把分片切得过短、白白增加往返轮数。
- `LastIndex` 返回字节偏移，多字节字符时换算成 rune 下标；低于窗口下限一律硬切，保证每片实实在在推进、不卡死。

---

## 3. 失败与降级处理（Failure / Degradation）

- **任一前置片失败**（`err != nil || !primeRes.Success`）：把诊断/路由相关字段整体透传给外层 `res`，按普通失败上抛，**绝不 emit 半截内容**。
- **前置片没返回 `conversationId`**：记 trace 但**不中止**。后续片只能各自新开会话、前文丢失——此为**已知降级**，但仍不比单发中段截断更差；终片照常产出答复并为后续轮沉淀一个会话 ID。

---

## 4. 关键前提假设（Assumption — 需线上验证）

分片续传依赖一个**尚未在线上 trace 中证实**的假设：

> **上游对 `USER_QUERY` 也按 `conversationId` 跨轮保留上下文**（与真实客户端续聊行为一致）。

- 代码与既有 memory 都支持它，但**能否真让模型记住前置分片，仍需线上 trace 验证**（按项目规矩，本地 db/日志不算线上证据）。
- 即使假设不成立，`MaxQueryChunks` 上限 + 回退截断保证**不会比现状更差**。

---

## 5. 迁移到其他语言时的行为契约（Checklist）

移植时请保留以下**行为契约**，而非具体 Go 实现：

1. **只对普通 USER_QUERY 分片**：排除原生 TOOL_RESPONSE 与 tool-tail，避免干扰会话粘性 / 403 钉号重试。
2. **链式会话**：前 N-1 片顺序发出、丢弃 ACK 回复、只取 `conversationId` 续接下一片；首片会话为 null。
3. **只 emit 终片**：前置片用 no-op emit；终片作为真实流式响应产出；`res.ConversationID` 取终片返回值。
4. **失败绝不半吐**：任一前置片失败即整体上抛，不向客户端 emit 任何增量。
5. **设分片上限 + 兜底截断**：超过上限回退中段截断，绝不膨胀成几十次往返。
6. **切分不切碎结构**：优先段落/换行边界，预留包裹语余量，按 rune（非字节）计。

---

## 6. 涉及文件（Go 参考实现）

| 文件 | 改动 |
|------|------|
| `internal/provider/types.go` | 新增常量 `MaxQueryChunks = 8`、`QueryChunkWrapperReserve = 256`（附硬上限 `MaxUpstreamQueryRunes = 10000` 说明） |
| `internal/provider/request.go` | `buildBody` 返回 `(body, outboundPlan)`；新增 `outboundPlan{input, fullQuery, chatType, convID, chunkable}`，`chunkable = !useNativeResponse && !toolTail` |
| `internal/provider/messages.go` | 新增 `splitQueryIntoChunks`（按 rune 切、优先自然边界）、`wrapPrimingChunk`（前置片包裹）、`wrapFinalChunk`（终片包裹） |
| `internal/provider/stream.go` | 抽出复用助手 `sendOnce`（单次完整上游往返）；新增编排 `streamChunked`、`setChunkInput`、`noopEmit`；`streamInternal` 在 `plan.chunkable && 超长` 时进入分片，`2..8` 片走分片、否则回退单发 |
| `internal/provider/chunk_test.go` | 切分预算 / 无损拼回 / 多字节包裹不溢出 |
| `internal/provider/chunk_stream_test.go` | 端到端：3 片、ACK 包裹、convID 链 `null→conv-0→conv-1`、只 emit 末片输出、`res.ConversationID=conv-2` |

验证：`go build ./...`、`go vet ./...`、`go test ./...` 全部通过。
