# 冷启动上下文补种（Context Seed）设计

日期：2026-09-17
状态：已实现（2026-09-17）

## 背景与问题

上游 Postman 对 `input.query` 有 10000 rune 硬性校验上限。命中已有会话
（conversationId）时 query 只带最新一轮，上限无感；但**冷启动**（换号 quota 耗尽、
进程重启丢内存映射、指纹未命中）时，`splitMessages` 走折叠路径，把全部历史线性折叠
进单条 query，超过 10000 由 `capUpstreamQuery` 保头 30% + 保尾截断，中段省略——
长会话的中间轮次（含最近几轮 assistant 承诺）会被切掉，模型答非所问
（2026-09-10、2026-09-17 两次线上事故，详见 types.go / messages.go 契约注释）。

渲染顺序修复（2026-09-17，普通续聊任务前置）只能保证「两头」存活；结构性问题是
10000 rune 装不下长会话。本设计用**两轮补种**根治：先发一轮上下文重建请求，让
Postman 服务端持有完整历史，第二轮恢复增量模式。

## 目标行为

```
触发条件（全部满足）：
  - 冷启动：LookupConversation 落空，splitMessages 将走折叠路径
  - 普通续聊（非 tool-tail 重放）
  - 历史消息数 > 6（短会话折叠产物不超限，补种白花配额）
  - 开关 GATEWAY_CONTEXT_SEED 未设为 "0"（默认开启）

第一轮（seed）：
  query = 折叠历史 context（复用 splitMessages 现有产物，含 [User (task)] 前置）
        + 指令：「以上是此前对话的完整上下文。请用一段话总结当前任务状态与
          最近进展，不要执行任何操作。」
  → 模型生成一句话摘要（中等 completion 消耗）
  → 拿到 ConversationID，按 conversationFingerprint(messages[:queryIdx])
    存 (账号, 前缀指纹) → conversationID 映射（PutOwner 同步）

第二轮：
  原始请求原样重发 → LookupConversation 前缀循环命中 seed 存的映射
  → query 只带最新一轮 → 服务端已持有上下文（含模型自己的摘要）
  → 此后每轮恢复增量模式，10000 上限不再咬人
```

## 失败兜底（核心契约：绝不比现状更差）

- seed 轮任何失败（quota / 403 / 429 / 网络错 / 空回复 / SessionCorrupt）：
  丢弃 seed 结果，回落现有单发折叠路径，行为与改动前完全一致。
- seed 成功、第二轮失败：router 现有重试接管；seed 存的前缀映射让重试直接
  走增量路径（不再二次折叠）。
- seed 请求标记 AuthFailed / QuotaExhausted 时，账号健康状态由 router 按现有
  res 标记逻辑处理，seed 不额外标记（probe 已有同类先例：只消费不标记）。
  注意：seed 自身的 res 不返回给上层，其配额/认证失败必须**复制**到主 res
  的判断里（否则上层无从决策换号）。实现：seed 失败时若 res.AuthFailed /
  QuotaExhausted / RateLimited，直接以 seed res 取代主 res 返回（等价于
  没有补种时第一跳就失败的形态），否则回落折叠路径。

## 实现落点

1. **新增 `internal/provider/seed.go`**：
   `seedConversation(ctx, acc, req, tokens, postmanModel, split) (*Result, bool)`
   - 构造 seedReq：Messages = req.Messages（buildBody 内部会因无映射走折叠，
     指纹天然一致），外加一个控制字段（见下）
   - 直接调 streamInternal（emit 丢弃），消费 ConversationID
   - 按前缀指纹存映射
2. **ChatRequest 新增 `ContextSeed bool`（json:"-"）**：seed 请求置 true，
   `buildBody` 检测到它时把 query 替换为「折叠历史 + 摘要指令」、去掉尾部
   最新消息渲染（摘要指令充当 tail）。这样 seed 复用 splitMessages 全部
   折叠逻辑（含 09-10/09-15/09-17 三条渲染契约），不另造渲染代码。
3. **接入点 `streamInternal`（stream.go:85 buildBody 之前）**：
   ```
   if 触发条件（冷启动 && 非tool-tail && len(history)>6 && 开关 && !req.WafProbe）:
       seedRes := seedConversation(...)
       if seedRes.Success: // 映射已存，buildBody 将命中增量路径
           // 继续正常流程
       elif seedRes.AuthFailed || seedRes.QuotaExhausted || seedRes.RateLimited:
           *res = *seedRes   // 账号级失败上抛，router 换号重试
           return err
       // 其余失败：静默回落折叠路径
   ```
4. **开关 `GATEWAY_CONTEXT_SEED`**：默认开；设 "0" 关闭（回到纯折叠路径）。
   沿用 GATEWAY_* env 惯例。

## 明确不做（YAGNI）

- tool-tail 重放路径补种：有 2026-09-15 自愈机制，工具循环里加一轮延迟代价高
- seed 摘要内容缓存/复用：seed 仅冷启动发生一次
- seed 失败计数/熔断：失败即回落，下次冷启动再试（频度极低）
- 跨账号 seed 复用：会话粘性已保证同账号重试

## 测试计划

1. **TestContextSeedColdStart**: 长会话冷启动（mock 上游，两轮请求捕获），
   断言：第一轮 query 含折叠历史+摘要指令、conversationId=null；第二轮
   query 只含最新消息、conversationId=seed 返回值。
2. **TestContextSeedFallbackOnFailure**: mock seed 轮 403 → 单发折叠路径，
   出站 query 与无 seed 逻辑时逐字节一致。
3. **TestContextSeedAccountFailurePropagates**: seed 轮 quota 耗尽 → 主 res
   带 QuotaExhausted（router 可换号）。
4. **TestContextSeedNotTriggered**: 首轮新对话 / tool-tail / 历史≤6 /
   开关关闭，均不产生 seed 请求。
5. 现有回归全量跑（09-10/09-15/09-17 三契约 + conv_test 全部）。

## 风险

- 每次冷启动多一次上游调用（摘要生成 + 一次 query 配额）。长会话截断造成的
  答非所问代价远高于此；开关可关。
- 摘要质量决定第二轮上下文质量：摘要由上游模型自己生成，服务端会话里同时有
  折叠原文（第一轮 query）+ 模型摘要（第一轮回复），第二轮模型可参照两者。
- trace 日志量增加（seed 轮也走 Trace，account_id 相同可区分）。
