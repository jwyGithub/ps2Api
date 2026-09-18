# 折叠重放质量优化设计

日期：2026-09-18
状态：已批准（brainstorming 三节逐节确认）

## 背景

2026-09-18 线上事故（请求 3666-3674，aistar-jflow database-v2.html 改造会话）：

1. 会话 7c9207c0 在 3666 遭遇空回复（`Upstream returned an empty completion`）→
   `SessionCorrupt` → `InvalidateConversation` 定点失效会话映射。
2. 下一轮 3667 是 tool-tail 请求：`shouldSeed` 排除 tool-tail，无补种；直接裸折叠
   128 条消息（79K prompt tokens）进 10000 rune 的 USER_QUERY。
3. `capUpstreamQuery` 固定 30% 一刀切，中段省略恰好吞掉 msgs 3~88——含已批准的
   完整方案（msg 86，2818 字符）。
4. 新会话 3f67335c 里模型只看到「方案已获批准」的残影而看不到方案本身，后续
   3673 声称「方案被压缩」并重新调研、重新向用户提问——上下文丢失的完整链路。

三个独立缺陷叠加：

- **缺陷 A**：`shouldSeed` 排除 tool-tail，补种恰好对最需要它的场景（agent 长会话
  中途断链）失效。
- **缺陷 B**：`[User (task)]` 块渲染时 `ExtractText` 带上 `<system-reminder>` 注入块
  （CLAUDE.md/gitStatus，数千字符），2000 rune 预算被挤占，真实任务句贴着截断边缘。
- **缺陷 C**：`capUpstreamQuery` 中段省略对内容权重一无所知，固定 30% 切点。

## 改动一：tool-tail 允许补种

### shouldSeed（internal/provider/seed.go）

移除 `toolTail(req.Messages)` 排除条件。保留的排除项不变：ContextSeed 自身、
开关关闭、WafProbe、无 reusable 历史、历史不足 7 条、会话已命中。

### seedConversation 前缀指纹（internal/provider/seed.go）

queryIdx 计算按请求形态分：

- 非 tool-tail：现状不变——最后一条非 tool_result 的 user 消息。
- tool-tail：取 `toolTailIndex(messages)` 位置，前缀指纹 = `messages[:toolIdx]`
  （含 toolIdx 前的全部消息，即折叠重放时的历史范围）。

客户端重试同一批 messages 时，`LookupConversation` 前缀循环（从 len-1 往回）必然
命中该键，恢复增量模式。

### 补种轮形态

不变：折叠历史 + `seedSummaryInstruction` 摘要指令，conversationId=null。摘要
成功后服务端新会话持有任务上下文，重试轮 query 只带本轮 tool results（hasConv
分支）。

### 已知限制（接受）

补种轮（USER_QUERY）不产生 toolCallId→groupID 映射，重试轮走折叠 hasConv 分支
而非 TOOL_RESPONSE——与 3667 现状同形态，但会话里多了任务摘要。摘要上下文 >
中段省略的残缺上下文，净收益为正。

### 失败回落

补种失败（上游拒绝/空回复）回落现状裸折叠路径，不比今天更差。
`streamInternal` 现有的账号级失败上抛逻辑（AuthFailed/QuotaExhausted/RateLimited）
不变。

## 改动二：任务块剥离 system-reminder

### stripSystemReminders（internal/provider/messages.go 新增）

正则剥掉所有 `<system-reminder>...</system-reminder>` 块（`(?s)` 跨行、非贪婪），
返回剩余文本。

### 应用点

仅折叠路径的 `[User (task)]` 块（messages.go taskIdx 渲染处）：

```go
taskBlock = "[User (task)]\n" + truncateMiddleRunes(stripSystemReminders(task), FoldedTextMsgBudgetRunes)
```

### 明确不做

- 首轮直发路径（tail = `"[User]\n" + query` 全文）不剥离：首轮 system-reminder
  是用户真实指令，且任务句在尾部保留区，无截断风险（3628 实证工作正常）。
- 折叠路径其余 `[User]`/`[System]` 段不剥离：它们是历史上下文，保留原貌；改动
  超出本次范围。

### 不变量

剥离只改出站渲染文本，不碰 `req.Messages`，不影响会话指纹与账号粘性（与
capUpstreamQuery 同一契约）。

## 改动三：cap 中段省略感知段落权重

### querySection（internal/provider/messages.go 新增）

```go
type querySection struct {
    text   string
    weight int // 3=高 2=中 1=低
}
```

### 段落权重

| 档位 | 段 | 理由 |
|---|---|---|
| 高（永不丢） | skills 块、`[User (task)]` 块、tail（最新 query / 待处理 tool-tail） | 09-10/09-15/09-17 三次线上事故契约：任务与最新输入必须存活 |
| 中 | `[System]` 段 | 环境与指令上下文 |
| 低 | 历史 `[User]`/`[Assistant]`/`[Tool Result]` 段 | 时间越早价值越低，从最旧开始丢 |

### splitMessagesSeed 折叠分支改造

产出 `[]querySection` 替代纯字符串 sections。组装顺序不变：skills 最前、
task 前置（普通续聊）/中置（tool-tail）、context 居中、tail 最后。每段带权重。

### capUpstreamQuerySections（新增重载）

1. 全量拼接 ≤ 9900 rune（`MaxUpstreamQueryRunes - 100`）：原样输出，行为零变化
   （多数会话走这条）。
2. 超限：先尝试只保留高+中段；仍超，只保留高段。丢弃以整段为单位，段内不再
   截断（各段已由段预算约束）。
3. 低档丢弃从最旧开始，丢到装得下为止；低档丢满仍不够才降档丢中。
4. 丢弃发生时省略标记写明内容：`...[omitted: N older history sections]...`。
5. 防御兜底：高段单独仍超限（理论上 4000+2000+2800=8800，不可能），回退现有
   `capUpstreamQuery` 字符串硬切。

### 路径隔离

- 非折叠路径（hasConv 增量、首轮直发）仍走原 `capUpstreamQuery` 字符串版。
- 探针（WafProbe）原样旁路，不参与。
- 出站仍是单条 query 字符串，上游协议零变化。

### 效果推演（以 3667 复盘）

低档段从最旧开始丢：msgs 3~70 的早期探索记录被丢弃，msgs 71~88（含 86 号方案
的 ~2000 rune `[Assistant]` 段）作为低档最新段存活，进入新会话。

## 测试

现有测试基线（messages/seed/conv 相关 `_test.go`）必须全绿。新增：

1. `stripSystemReminders`：常规剥离、多块剥离、无块直通、跨行块。
2. `capUpstreamQuerySections`：
   - 不超限直通（与旧版输出一致）；
   - 超限丢低档（从最旧开始）、丢后仍超降档丢中；
   - 高段必存（task/skills/tail 不丢）；
   - 省略标记含丢弃计数。
3. `shouldSeed`：tool-tail + 无会话命中 + 历史足够 → true（原为 false）。
4. `seedConversation` 前缀指纹：tool-tail 请求的前缀 = messages[:toolIdx]，
   重试轮 LookupConversation 命中。
5. 折叠路径集成：含 system-reminder 的 msg[0] 作为 task 时，任务句完整出现在
   出站 query。
