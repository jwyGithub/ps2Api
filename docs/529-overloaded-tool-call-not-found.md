# 生产环境 529（Repeated Overloaded）：空流被当成成功，毒化 TOOL_RESPONSE 会话

> 本文档记录一次生产事故的根因分析与修复方向。
> 涉及服务：Postman → API 的 HTTP 代理/网关（Go 实现）。
> 证据：`data/traces/anthropic/2026-08-27/`（132 条 trace，2026-08-27 10:04–10:43）。

---

## 1. 现象（Symptom）

两个 agent 客户端同时走网关时，Claude Code / Cursor 报：

```
API Error: Repeated 529 Overloaded errors. The API is at capacity —
this is usually temporary. Try again in a moment.
If it persists, check your inference gateway (localhost:1930).
```

网关日志对应的 HTTP 行是：

```
POST /v1/messages -> 529
{"error":{"type":"overloaded_error","message":"All accounts failed. Last error: Looks like I lost my way. Try sending a new message. | No tool call found for the tool response that was sent"}}
```

表面像 Postman / Bedrock 容量满。实际上上游既不是 529，也不是限流：账号 `RateLimit remaining` 仍有余量，`usageState=AVAILABLE`。客户端反复重试的，是网关把另一类错误伪装成了 `overloaded_error`。

---

## 2. 当天数字

| 现象 | 次数 |
|---|---|
| 上游请求 | 205 |
| `router.failure` | 113 |
| 其中 `TOOL_CALL_NOT_FOUND` | **108**（36 条客户端请求 × 网关内部重试 3 次） |
| 额度耗尽换号 | 5 |
| 视觉模型超时 | 7 |
| **空流却被标 `Success=true`** | **3**（每个卡死会话各 1 次，1:1） |
| 真正成功（有 `[DONE]` 且有正文或工具调用） | 89 |

三个卡死会话，各自独立、互不串台：

| 客户端 session | 账号 | Postman conversationId | 引爆空流 |
|---|---|---|---|
| `6c1c97e4` | 58 | `a48c37eb-…` | `55d4a2ed` 10:32:26 |
| `2dffa09e` | 57 | `42c01329-…` | `ba2241e1` 10:39:57 |
| `49badd26` | 59 | `fe8f07f3-…` | `cc55c0e4` 10:42:48 |

---

## 3. 根因分析（Root Cause）

问题是三层叠加，不是单一故障。

### 3.1 上游 SSE 空结束，网关当成成功

三次引爆点完全同构：网关发出 `chatType=TOOL_RESPONSE` 之后，Postman 回 HTTP 200，只吐了 `usage` / `streamingFormat` / `conversation`，**没有** `textChunk`、`toolCallChunk`、`failure`，也**没有** `[DONE]`，流就干净结束。

| 时间 | trace | 耗时 | 上游实际吐出 |
|---|---|---|---|
| 10:32:26 | `55d4a2ed` | 2.9s | usage + conversation 后结束 |
| 10:39:57 | `ba2241e1` | 2.7s | 同上 |
| 10:42:48 | `cc55c0e4` | 0.47s | 只有 usage |

对照：当天 89 次真正成功的流**都带 `[DONE]` 且有正文或工具调用**。这 3 次是仅有的「`Success` 但没有 `[DONE]`」。

`streamInternal` 的判定是：scanner 遇到 EOF 且 `reader.Err == ""` 就 `res.Success = true`：

```215:248:internal/provider/stream.go
	if reader.QuotaExceeded { ... }
	if reader.Err != "" { ... return err }
	// 没有任何失败标记 → 视为成功
	res.Success = true
	return nil
```

空流走这条路径之后，`streamAnthropic` 给客户端发：

- `message_start`
- `message_delta`（`stop_reason=end_turn`，`output_tokens=0`）
- `message_stop`

Claude Code 看到的是「模型空回复结束了」，不是过载。

空流本身是 Postman 已收下工具结果、但 Bedrock 生成被掐断（对端发了 `END_STREAM`；若是 RST，网关会记 `stream read error`，不会标成功）。`55d4a2ed` 的 conversation 事件可印证：`interactionCount` 已从 3 变成 4，`state=WAITING_FOR_AGENT`——工具结果已经被服务端消费。

### 3.2 客户端立刻重发同一组 tool result，会话已毒化

`55d4a2ed` 成功后 **29ms**，`fc6ae65a` 带着同样的 296 条消息再打进来。网关指纹命中同一 `conversationId`，再次发：

```text
chatType=TOOL_RESPONSE
conversationId=a48c37eb-...
toolCallGroupId=609aed67-...
toolCallId=toolu_bdrk_01VRiahk9MknyyWEoXucZvRB
           toolu_bdrk_01LsvGGb9dtF7KrwqU8i4TCu
```

Postman 第一次空流时已经把这组 pending tool call 消费掉。第二次再交同一组 id，上游返回：

```json
{
  "errorType": "TOOL_CALL_NOT_FOUND",
  "message": "No tool call found for the tool response that was sent",
  "userMessage": "Looks like I lost my way. Try sending a new message."
}
```

之后这个会话**每一次**续聊都是同一组 id、同一个 group、同一个 conversation，全部失败。另外两个会话同一剧本。

空成功仍会 `RememberConversation`，指纹继续指向已毒化的 Postman 会话，后续永远走 native `TOOL_RESPONSE`，不会降级成把历史折进 `USER_QUERY`。

### 3.3 网关把这个错误包装成 529，SDK 再叠一层重试

三处放大：

**错误类型映射错了。** Anthropic 端点把几乎所有上游失败都写成 529 `overloaded_error`：

```68:70:internal/api/anthropic.go
		// 529 overloaded_error 是 Anthropic 协议表达「上游暂时不可用」的标准方式；
		anthropicError(w, 529, err.Error(), "overloaded_error")
```

流式路径同样发 `overloaded_error`。这是 2026-08-25 为了对齐 Anthropic 状态码枚举、让 SDK 能停下来而做的选择（见 `docs/superpowers/specs/2026-08-25-protocol-error-and-image-probe-design.md`）。对「暂时不可用」它是对的；对「会话状态坏了、同一 payload 再发一定失败」它是错的——SDK 对 529 的标准动作就是退避重试，于是用户看到 `Repeated 529 Overloaded errors`。

**`TOOL_CALL_NOT_FOUND` 没被当成「请求内容拒绝」。** `requestRejectionMarkers` 里没有它，router 会钉住原账号重试 3 次。换号也救不了——pending tool call 已经没了，同一 conversation 再发一定失败。1 次上游失败变成网关 3 次，再被客户端乘上 N 次。

**空成功不会让会话映射失效。** 见 §3.2。

### 3.4 和「两个 agent 同时用」的关系

**不是**两个客户端共用一个 Postman 会话。账号粘性把它们钉在 57 / 58 / 59 上，conversation 也是分开的。每个账号走自己的 `*.postman.co` host，h2 连接按 authority 池化，两条流不会挤在同一条 HTTP/2 连接上互相截断。

并发只是催化剂：

1. 两路同时打，Postman/Bedrock 更容易给出「收下 TOOL_RESPONSE 但不生成」的空流。
2. 空流一旦被当成成功，**单个**会话就会自己卡死；另一个 agent 在空流之前其实还在正常跑（`2dffa09e` 在 `6c1c97e4` 卡死期间仍在成功续聊）。
3. 空流发生时刻的瞬时并发度是 1，但前后几十秒内两路请求交错。

体感是「一开两个就 529」，根因是：**并发提高空流概率 → 空流被标成功 → 工具回执死循环 → 529 触发 SDK 重试。**

---

## 4. 修复方向（Fix）

尚未落地。**关键前提：4.1 / 4.2 / 4.3 必须作为一个整体一起落地。** 任何一个单独上都不够甚至有害：

- 只做 4.1（把空流标可重试）→ 映射还在，`runAttempts` 会用同一 pinnedAcc + 同一 conversationId 再发一次 native `TOOL_RESPONSE`，把「空流→再发→`TOOL_CALL_NOT_FOUND`」的死循环搬进网关内部。
- 只做 4.2（停 529）→ 会话仍死，客户端从吃 `Repeated 529` 变成持续吃 400。
- 4.3 若用账号级 `Reset` → 误伤同账号上并发的健康会话（事故里 `2dffa09e` 卡死期间仍在成功续聊）。

按优先级：

### 4.1 空 SSE 不能算成功

同时满足以下条件时，视为上游不完整结束，**不得** `Success=true`，也**不得**向客户端发 `end_turn`：

- 没有 `[DONE]`（需先在 `sse.go` 记 `sawDone`，当前完全没记）
- 没有 `textChunk` / `toolCallChunk`
- 没有 `failure`

按请求类型区分重试策略：

- **`USER_QUERY` 空流**：可直接按瞬时错误重试。
- **`TOOL_RESPONSE` 空流**：必须假定 Postman 已消费了 pending tool call。按 `UpstreamFailure` 语义处理（`route_loop.go` 钉住原账号重试、不换号），**但重试前先执行 4.3 的映射失效**——这样 `LookupConversation` 落空，重试时自动降级为 `conversationId=null` 的 `USER_QUERY` 重建，而不是再交一次已消费的 `toolCallId`。

> 顺序陷阱：4.1 的 `TOOL_RESPONSE` 重试与 4.3 的映射失效是强耦合的，必须「先失效映射、再重试」，否则复现内部死循环。

### 4.2 `TOOL_CALL_NOT_FOUND` 按请求拒绝处理，不要映射 529

- 识别方式优先用 `errorType`（更稳，不随上游文案变化）：在 `sse.go` 的 `handleFailure` 里对 `errorType == "TOOL_CALL_NOT_FOUND"` 直接置 `RequestRejected=true`（现在只有 `INPUT_VALIDATION_ERROR` 会置）。若走 marker 路线，`Err` 里已拼接 `| No tool call found ...`，把 `"no tool call found"` 加入 `requestRejectionMarkers` 也能命中 `isRequestRejectionMessage`。
- 走 `RequestRejected`：不内部重试、不换号、不标记账号（`route_loop.go` 已有短路）。
- Anthropic 侧不要用 529 `overloaded_error`，改 400 `invalid_request_error`。**但这需要先补 `RouteError` 的分类能力（见 4.4），否则 API 层无从区分。**

### 4.3 毒化会话必须**定点**失效

空成功或 `TOOL_CALL_NOT_FOUND` 之后：

- **按 `conversationId` / 指纹定点删除**该会话的映射，同时清 `toolCallGroupId`（`toolGroups`）。**不能用账号级 `Reset(accountID)`**——它会清掉该账号名下全部会话与归属映射，误伤并发的健康会话。
- 现状 `ConversationStore` 只暴露 `Reset(accountID)`（`convstore.go`），需在接口新增「按 conversationId / 指纹定点删除」方法，并**同时实现 memory 与 redis 两个后端**（`redisstore.go` 也要改）。注意现有 `Reset` 并不清 `toolGroups`，定点失效时务必一并清。
- 下一轮改为折叠历史开新会话（`conversationId=null` 的 `USER_QUERY`），而不是继续交已消费的 `toolCallId`。

否则指纹会一直命中死会话，4.1 / 4.2 单独修不够。

### 4.4 `RouteError` 需带错误分类（原文档遗漏）

`RouteError` 现在只有 `Message` 和 `GatewayBlocked`（`router.go`），`res.RequestRejected` 分支返回的是 `&RouteError{Message: res.Error}`，**不带任何分类标记**，`anthropic.go` 因此只能一律 529。要实现 4.2 的 400：

- 给 `RouteError` 加分类字段（如 `Rejected bool` 或直接带 HTTP status code）；
- `route_loop.go` 的 `RequestRejected` 分支返回时置上该字段；
- `anthropic.go` 非流式与流式（error 事件）两处按该字段选 400 `invalid_request_error` / 529 `overloaded_error`。

---

## 5. 不是这次 529 的主因

| 现象 | 说明 |
|---|---|
| 账号 46 / 52 / 55 额度耗尽 | 新对话换号到 57 后继续成功 |
| 视觉模型 `125.122.23.233:9080` 超时 | 3 次 `client.vision_failed`，走 400，不是 529 |
| Cloudflare 403 | 当天未出现 |
| 自定义 h2 客户端截断流 | RST 会变成 `stream read error`；空流是干净 EOF，对端主动 `END_STREAM` |

---

## 6. 涉及文件（Go 参考实现）

| 文件 | 现状 | 拟改 |
|------|------|------|
| `internal/provider/sse.go` | 不跟踪 `[DONE]`；仅 `INPUT_VALIDATION_ERROR` 置 `RequestRejected` | 记录 `sawDone`；`handleFailure` 对 `errorType==TOOL_CALL_NOT_FOUND` 置 `RequestRejected` |
| `internal/provider/stream.go` | EOF 且无 `reader.Err` → 成功 | 无 `[DONE]` 且无正文/工具/failure → 失败（`TOOL_RESPONSE` 空流按 `UpstreamFailure`，重试前先失效映射） |
| `internal/provider/toolsim.go` | `requestRejectionMarkers` 不含本错误 | （marker 路线备选）加入 `no tool call found` |
| `internal/provider/conversation.go` | 成功（含空成功）都 `RememberConversation`，无失效路径 | 空流 / `TOOL_CALL_NOT_FOUND` 后按 conversationId 定点清映射 |
| `internal/provider/convstore.go` + 接口 | 只有账号级 `Reset(accountID)`，且不清 `toolGroups` | **新增** `InvalidateConversation` 按 conversationId 定点删除，一并清 `toolGroups` |
| `internal/provider/redisstore.go` | 同上，redis 后端 | **同步实现** `InvalidateConversation`（SCAN 值匹配 + owner/ownset 收缩） |
| `internal/router/router.go` | `RouteError` 只有 `Message`/`GatewayBlocked` | **新增**分类字段（`Rejected` 或 status code） |
| `internal/router/route_loop.go` | 未分类错误钉原号重试；`RequestRejected` 已有短路 | `RequestRejected` 分支返回时置 `RouteError` 分类字段 |
| `internal/api/anthropic.go` | 所有 `Chat` 失败 → 529 `overloaded_error`（非流式+流式两处） | 按 `RouteError` 分类：会话损坏改 400 `invalid_request_error`，保留 529 给真正暂时不可用 |

---

## 7. 复现线索

从 `data/traces/anthropic/2026-08-27/` 抽一条完整链：

1. `94b1de09` 10:32:09 `USER_QUERY` → 成功，吐出两个 `Bash` tool call（`ucZvRB` / `8i4TCu`），`conv=a48c37eb`。
2. `55d4a2ed` 10:32:23 把这两个结果以 `TOOL_RESPONSE` 交回 → 空流，网关标成功，客户端收到空 `end_turn`。
3. `fc6ae65a` 10:32:26 重发同一组 tool result → `TOOL_CALL_NOT_FOUND`。
4. 之后该 session 所有请求（`0c08ff94`、`26c2f854`、`02579080` …）都带着同一组 id 失败，直到客户端放弃。
