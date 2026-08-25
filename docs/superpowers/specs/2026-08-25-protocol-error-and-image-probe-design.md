# 错误响应对齐标准协议 + 图片输入探针

日期：2026-08-25

## 背景

两个现象：

1. 上游账号池耗尽或被 Cloudflare 拦死时，agent 客户端不停止，表现为"一直在转"。
2. 客户端发图片时模型答非所问。

排查后确认这是两个性质完全不同的问题。

### 问题 1：503 不停止的四层原因

| 层 | 位置 | 问题 |
|---|---|---|
| 状态码 | `anthropic.go:57`、`openai.go:39`、`responses.go:137`、`responses.go:409`、`openai.go:74` | 503 属 5xx，Anthropic/OpenAI SDK 默认自动重试 5xx（max_retries=2）。且 503 不在 Anthropic 状态码枚举内（400/401/403/404/413/429/500/529），过载语义应为 529 `overloaded_error` |
| 错误体 | `helpers.go:19` | 产出 `{"error":{...}}`，Anthropic 规范要求顶层带 `{"type":"error","error":{...}}` |
| 分类丢弃 | `router.go:33` | `RouteError.GatewayBlocked` 的注释写明"供 HTTP 层选择状态码"，但三个 handler 都未读取 |
| 流式终止 | `anthropic.go:290`、`openai.go:80` | Anthropic 侧 `stop_reason: "error"` 不是合法枚举值；OpenAI 侧 error 帧后又发 `data: [DONE]`，而 `[DONE]` 语义是"正常完成"，SDK 据此吞掉 error 帧，认为流成功但内容为空 —— agent 于是继续下一轮 |

其中「`[DONE]` 吞掉 error」是客户端不停止的最直接原因。

### 问题 2：图片不是未处理，而是被静默降级

`content.go:32-33` 已匹配图片块，替换为字面量 `[image attachment]`：

```go
case "image_url", "image", "input_image":
    out = append(out, "[image attachment]")
```

模型收到的是这句占位符，因此"看不见图"但也不报错。

更根本的约束在上游协议。`request.go:35-41` 构造的 Postman 出站体中 `query` 是**单个字符串**，`seedingMessages` 是 `[]map[string]string`；`messages.go:113-115` 还对 query 做 9500 字符**尾部截断**。因此：

- 当前抓包对齐的 Postman `/chat` 形态没有图片通道。
- 任何把 base64 内联进 `query` 的做法都会被截成尾部残片（100KB PNG → base64 约 133K 字符），探针会得到"格式不对"的**假阴性**。

若 Postman 确有图片通道，它只可能是独立字段或先上传文件拿 ID，不会在 `query` 字符串里。

## 决策

- **失败语义**：按标准协议语义，允许 SDK 重试后停止。Anthropic 侧 529 `overloaded_error`，OpenAI 侧 503 `service_unavailable`。不做按错误分类的差异化状态码。
- **图片**：先用一次性探针打真实上游，探明 `input.query` 换成 Anthropic content blocks 数组时上游返回什么，再决定实现。不猜字段直接改产线。
- **版本常量不动**。抓包显示两份各自内部三元组自洽，且用旧版本（12.24.0-260817-0232，即代码现值）的那份是更晚抓的（10:08 vs 09:40），说明服务端仍接受当前版本。升级收益未验证，改错会导致全线 403。

## 设计一：图片探针

新增 `internal/provider/imgprobe_test.go`，同包 test，环境变量 `IMG_PROBE=1` gate，默认跳过（与 `REPRO_403` 同模式）。

**为什么不能走 `p.Chat`**：`postman.go:57` → `buildBody` → `splitMessages` → `ExtractText` 这条链会把图片压成占位符。探针必须自己组 body。`ChatRequest.Raw`（`types.go:69`）声明了但从未被使用，不是可用的注入点。

**复用**：`repro403_test.go` 的 `resolveExistingDB` / `envInt` / `oneLine`；provider 的 `p.Client`（带 uTLS 指纹，普通 client 会被 Cloudflare 拦）、`p.buildHeaders`、`p.chatURL`、`p.GetTokens`。

**两个变体，都必须跑**：

| 变体 | `input.query` | 作用 |
|---|---|---|
| A 对照组 | 纯字符串（照抓包） | 证明探针本身能通。A 失败则凭据/出口有问题，B 的结果不可信 |
| B 图片 | Anthropic blocks 数组 `[{type:text},{type:image,source:{type:base64,media_type,data}}]` | 探测位置 |

**图片素材**：`image/png` 现场生成 32×32、左半红右半蓝的 PNG（约 200 字节），提问"左半和右半分别是什么颜色"。模型同时猜对两侧颜色的概率极低 —— 答对即证明上游真的看到了图，而非敷衍。

**body 用产线的版本三元组**（`WebAppVersion` 等常量），不用抓包里的新版本 —— 探针要测的变量是图片格式，其余变量与产线一致才能归因。

**输出**：状态码、响应头、SSE 全文，两变体并排。

**预期**：B 很可能被服务端类型校验拒（`INPUT_VALIDATION_ERROR`）。该错误信息本身即情报，能揭示 `query` 的期望类型，为下一轮探针（独立 attachments 字段）指方向。

## 设计二：错误响应对齐标准协议

### 1. helpers.go 新增两个协议专属写出函数

`jsonError` 保留原样 —— 它被 accounts/metrics/ops 等面板端点大量使用，那些不是 LLM 协议端点，不应改动。

```go
// Anthropic：顶层必须带 type:"error"
{"type":"error","error":{"type":"overloaded_error","message":"..."}}
// OpenAI：error 对象带 param/code
{"error":{"message":"...","type":"service_unavailable","param":null,"code":null}}
```

新增 `anthropicError` / `openAIError`，以及 `protoError` —— 后者按 `r.URL.Path` 分流，供 `auth` 与 `traceChat` 这两处被两种协议共享的代码使用（`traceChat` 已有同样的路径判断逻辑）。

### 2. 状态码与 type 值对齐枚举

| 位置 | 现状 | 改为 |
|---|---|---|
| `anthropic.go` 上游失败 | 503 `api_error` | **529** `overloaded_error` |
| `anthropic.go` 工具循环拦截 | 400 `tool_execution_error` | 400 `invalid_request_error`（前者不在 Anthropic 枚举内） |
| `anthropic.go` stream unsupported | 500 `internal_error` | 500 `api_error` |
| `openai.go` / `responses.go` 上游失败 | 503 `provider_error` | 503 `service_unavailable` |
| `openai.go` / `responses.go` 400 类 | `invalid_request` | `invalid_request_error` |
| `api.go` auth 401 | `invalid_api_key` | 按协议分流：Anthropic `authentication_error` / OpenAI `invalid_request_error` + `code: invalid_api_key` |
| `middleware.go` 413 | `invalid_request` | Anthropic `request_too_large` / OpenAI `invalid_request_error` |

### 3. 流式终止序列改成标准形态

- **Anthropic**（`anthropic.go:286-292`）：删掉 `message_delta` 那行（`stop_reason: "error"` 非法枚举值，会让类型化 SDK 解析异常）。保留 `event: error` + `message_stop` —— 认 error 的 SDK 抛异常停止，不认的靠 message_stop 干净收尾，两头都不挂起。
- **OpenAI**（`openai.go:77-81`）：error 帧后**不再发 `data: [DONE]`**，直接 return 关闭连接。连接 EOF 即流结束，SDK 不会挂起。
- **Responses**：已发 `response.failed`，符合标准，不改。

### 4. 附带修复

- 删除 `ChatRequest.Raw` 死字段（`types.go:69`，声明后从未被读写）。
- `streamAnthropic` 的 `message_delta` 回填真实 `output_tokens`：现为硬编码 0，改为接收 `Router.Stream` 返回的 `Result.CompletionTokens`（当前该返回值被丢弃）。

### 5. 测试

新增 `internal/api/protocol_error_test.go`，断言：

1. Anthropic 错误体含顶层 `type: "error"`。
2. Anthropic 上游失败返回 529 且 type 为 `overloaded_error`。
3. Anthropic 流式错误序列不含非法 `stop_reason`，且以 `message_stop` 收尾。
4. OpenAI 流式错误后不含 `[DONE]`。

## 探针结论（2026-08-25 已执行）

**上游没有任何图片/附件通道。** 三条独立证据：

| 变体 | 位置 | 结果 |
|---|---|---|
| A 对照组 | body 不改 | 200 + 正常回答 → 链路有效，其余结果可归因 |
| B | `input.query` 换成 Anthropic blocks 数组 | `{"errorType":"INPUT_VALIDATION_ERROR","message":"Forbidden"}` → 该字段被强校验为 string |
| C | `input.attachments` | 200，模型答"没收到任何图片" → 字段被静默忽略 |
| D | `input.images` | 同上 |
| E | `input.files` | 同上 |
| F | `body.selectedContext`（`{type:IMAGE,value:...}`） | 同上 |
| G | 不带图，直接问模型 | 自述 *"I can't reliably receive or view images in this Postman Agent Mode... I have no image-decoding capability and no tool for accepting an uploaded image"*，并明确拒绝编造机制 |

C/D/E/F 全部落在「静默忽略」而非「被拒」—— 服务端连这些字段名都不认识。继续猜字段名的期望收益已归零。

### 因此：入站明确拒绝，不再静默降级

静默降级比拒绝更糟：模型看不到附件却收到一句 `[image attachment]`，只能瞎猜；调用方还以为请求成功了。

实现（`provider/content.go` + 三个 handler）：

- `provider.unsupportedMediaKinds` 把六种入站块类型映射到两个类别：
  `image`/`image_url`/`input_image` → `image`，`document`/`file`/`input_file` → `document`。
  与 `ExtractText` 放同一文件，避免类型列表漂移。
- `provider.UnsupportedMediaContent(messages)` —— 给 Anthropic / OpenAI 端点用，与既有的
  `UnsupportedToolResult` 同一模式：provider 层判定，HTTP 层决定怎么回错。
- `provider.UnsupportedMediaInJSON(raw)` —— **递归**扫描。三个理由：图片可能直接在 content
  blocks 里、可能嵌在 `tool_result.content` 内、也可能在 Responses 的 input 项里。递归只按
  `{"type": ...}` 匹配，text 块字符串值里的同名字面量不会被解析成对象，不会误报。
- **Responses 端点必须扫原始 `rr.Input`**：`responsesToOpenAI` 走 `extractResponsesText`
  只取 `.text` 字段，`input_image` 在转换那一步就丢了，转成 ChatMessage 之后查不到。
  这是最容易漏的一处。
- 统一文案 `unsupportedMediaMessage(kind)`：400 + `invalid_request_error`，说明"上游只接受
  文本、没有附件通道"并给出可操作建议（移除附件或把内容贴成文本）。用 4xx 而非 5xx 是因为
  重试永远不会成功，SDK 见到 4xx 会立即停止。
- `ExtractText` 的 `[image attachment]` 分支保留为兜底，注释说明正常路径下 HTTP 层已拦截，
  它只防内部调用路径（会话重放等）漏检。

**document 一并纳入**：上游没有附件通道这件事对 PDF 与图片完全同因，同一个 map 里一行之差。

测试：`internal/api/media_reject_test.go` 8 个用例 —— 三协议各自的图片块、Anthropic
document、嵌在 tool_result 里的图片、纯文本不误伤、text 里提到 image 的字面量不误报、
provider 层类型映射。

## 不在本次范围

- 版本三元组升级 —— 证据不支持。
- `MaxQueryLen` 9500 尾部截断策略。
- 若将来 Postman 网页版 UI 出现传图入口，抓包拿到真实字段后可重新实现透传；
  `imgprobe_test.go` 的变体表可直接扩展复用。

