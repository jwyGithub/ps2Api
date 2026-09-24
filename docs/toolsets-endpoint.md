# Toolsets 端点（claude-opus-5 / claude-sonnet-5）接入文档

> Postman「工具集(Toolsets)」功能暴露的 Anthropic Messages **原生代理端点**。此端点的模型白名单与 Agent Mode（`/chat`）不同——**claude-opus-5 / claude-sonnet-5 仅在此端点可用**（2026-09-24 实测）。
> ps2Api 以**原生透传**方式接入：`/v1/messages` 的 Anthropic 请求不做任何内部转换，直接转发上游（thinking/signature/图片 blocks/工具调用零损耗）。

---

## 1. 端点规格

```
POST https://<subdomain>.postman.co/_gw/toolsets/v1/messages
```

| 项 | 值 | 说明 |
|---|---|---|
| **`x-pstmn-req-service`** | `ai-toolsets` | **必须。缺失/错误一律 404**，与认证无关（排障第一检查点） |
| 认证 | `x-access-token` + `x-multi-login-token`（双 token）或 web cookie | 产线注册号的双 token 直连可用 |
| `x-entity-team-id` | `<teamId>` | 建议带上（抓包对齐） |
| `anthropic-version` | `2023-06-01` | 标准 Anthropic 头 |
| `x-app-version` | `12.29.2-260923-0231` | 对齐桌面端 |
| 域名 | 账号自己的 `<subdomain>.postman.co` 即可，`go.postman.co` 亦可 | 无需特定团队域 |

**请求体** = 标准 Anthropic Messages API，唯一差异是 `model` 用三段式路由名：

```json
{
  "model": "claude-opus-5/anthropic/anthropic-messages",
  "max_tokens": 1024,
  "system": [{"type": "text", "text": "..."}],
  "messages": [{"role": "user", "content": [{"type": "text", "text": "..."}]}],
  "tools": [...],
  "tool_choice": {"type": "auto"},
  "thinking": {"type": "enabled", "budget_tokens": 1024},
  "stream": true
}
```

- 三段式 = `<模型名>/<provider>/<api>`；当前仅 `anthropic/anthropic-messages` 路由有模型（openai 系后缀全 404）
- **响应** = 标准 Anthropic SSE（流式）或 Anthropic JSON（非流式），`usage` 只有 token 计数
- 端点**无 conversationId 会话粘性**——每次全量 messages，上下文由调用方维护
- 端点**无 credits usage 事件**——不产生 AI credits 计量数据

---

## 2. 实测模型枚举（2026-09-24）

| model 三段式前缀 | 可用 | 备注 |
|---|---|---|
| `claude-opus-5` | ✅ | thinking + signature + 真实回复；PAID/FREE 账号均通 |
| `claude-sonnet-5` | ✅ | |
| `claude-opus-4-8` | ✅ | 与 /chat 白名单重叠 |
| `claude-opus-5-1` | ❌ not_found_error | |
| `claude-haiku-5` | ❌ not_found_error | |
| `claude-opus-5-5` | ❌ not_found_error | 含裸名、多 provider 后缀变体 |
| `gpt-6-astra` | ❌ not_found_error | 含裸名、openai/responses、chat-completions 后缀 |
| `gpt-5.6` / `gemini-3-pro` | ❌ not_found_error | 走 anthropic-messages 路由 |
| `claude-opus-5/openai/chat-completions` | ❌ not_found_error | 非 anthropic-messages api 全 404 |

> `/chat`（Agent Mode）对照：`claude-opus-5` 等不在其白名单，返回流内 `INPUT_VALIDATION_ERROR: Forbidden`。
> **判定方法论**：/chat 会先发 usage 事件再发 failure——测模型必须看到 `textContent`，只看 HTTP 200 会误判。

---

## 3. 限频与退避

- 连续 ~6 请求后开始交替返回：
  - `429` + `{"error":{"code":"rate_limited","message":"Too many requests. Please try again shortly."}}`
  - `200` + `{"error":{"code":"upstream_unavailable","message":"The model provider is unavailable. Please try again."}}`
- 退避策略（ps2Api 实现）：
  - Provider 层 `doWithRetry`：429/503 重试 2 次（8s / 16s）
  - API 层换号：重试耗尽后从号池换下一个账号再试（最多 3 号）
- 客户端侧建议：SDK 自带指数退避即可，529 overloaded_error 是 Anthropic 标准「上游暂时不可用」表达

---

## 4. ps2Api 行为

### 4.1 模型路由

```
客户端 /v1/messages (model=claude-opus-5)
  → anthropic.go 入口: IsToolsetsModel?
    → 是: handleToolsetsMessages（原生透传）
         选号（号池轮询，无会话粘性）
         → 改写 model 为三段式 → POST toolsets 端点（双 token）
         → 上游 SSE/JSON 原样回写（仅 model 字段回写客户端原名）
         → 限频: Provider 层退避 2 次 → 换号重试（≤3 号）
    → 否: 原有 /chat 路径（PostmanProvider）不变
```

- **透传保真**：客户端与上游同为 Anthropic 协议，body 除 `model` 外零改动——thinking/signature/图片 blocks/tool_use 原样往返。非流式响应的 `model` 字段回写客户端原名；流式逐事件透传（上游 `message_start` 本就返回裸名，无需回写）
- `/v1/models` 列表已追加 `claude-opus-5`、`claude-sonnet-5`
- OpenAI 端点（`/v1/chat/completions`、`/v1/responses`）请求这两个模型 → 返回 "Invalid model"（未做协议转换，明确报错）
- `normalizeModel` 已加特例：`claude-opus-5` / `claude-sonnet-5` 直通（否则会被通用 claude- 规则错改成 `claude-sonnet-4-6`）

### 4.2 计费与日志

- 端点无 credits usage 事件 → `Credits = 0`，`chargeKey` no-op（API Key 计费不受影响，但不产生计量数据）
- 面板请求日志可见：`endpoint = anthropic-toolsets`，`upstreamUrl = _gw/toolsets/v1/messages`
- 额度管控：号池的额度探测（ProbeQuota）仍走 /chat，与 toolsets 使用互不影响

### 4.3 关键文件

| 文件 | 职责 |
|---|---|
| `internal/provider/toolsets.go` | ToolsetsProvider：Chat/StreamChat 透传、model 改写/回写、限频退避、错误分类 |
| `internal/api/toolsets.go` | /v1/messages 分流 handler：选号 → 透传 → 换号重试 → 响应回写 |
| `internal/api/anthropic.go` | `IsToolsetsModel` 分流入口 + `normalizeModel` 特例 |
| `internal/provider/models.go` | /v1/models 列表追加 |
| `internal/router/router.go` | `Router.Toolsets` 接线（复用 Provider 代理池/cookie jar） |
| `internal/router/account.go` | 导出 `SelectAccount` 供 api 层选号 |

---

## 5. 验证

```bash
# 非流式
curl http://127.0.0.1:1930/v1/messages \
  -H "x-api-key: <key>" -H "content-type: application/json" \
  -d '{"model":"claude-opus-5","max_tokens":1024,"messages":[{"role":"user","content":"Reply: OPUS5 VIA PS2API"}]}'
# 预期: "model":"claude-opus-5"，内容匹配，usage 为 Anthropic 原生 token 计数

# 流式（观察完整 SSE 事件序列: message_start → thinking/text/tool_use → message_stop）
# 同请求加 "stream": true

# 工具调用往返
# 带 tools 请求 → tool_use 块；补 tool_result 续聊 → 正常

# 非法模型（干净透传 404）
# model=claude-opus-5-1 → not_found_error

# /v1/models 应含 claude-opus-5 与 claude-sonnet-5
```

**背景与完整实测记录**（含 token 产线、PKCE handover、试用机制、已证伪路径）：见 [desktop-token-and-toolsets-research.md](desktop-token-and-toolsets-research.md)。
