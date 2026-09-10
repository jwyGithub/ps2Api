# ps2Api

将上游 AI 账号聚合成一个本地网关，对外暴露 **OpenAI 兼容**（`/v1/chat/completions`、`/v1/responses`）与 **Anthropic 兼容**（`/v1/messages`）接口。多账号自动轮询、故障切换、会话粘性续接、真实额度同步，附带一个支持登录的实时数据面板。以 **Docker** 运行（无界面、纯静态二进制）。

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE) ![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg) ![Docker](https://img.shields.io/badge/Docker-ready-2496ED.svg)

---

## 目录

- [功能特性](#功能特性)
- [工作原理](#工作原理)
- [快速开始](#快速开始)
- [账号接入](#账号接入)
- [核心用法](#核心用法)
- [配置](#配置)
- [API 参考](#api-参考)
- [支持的模型](#支持的模型)
- [已知限制](#已知限制)
- [开发](#开发)
- [目录结构](#目录结构)
- [许可](#许可)

## 功能特性

### 协议与推理

- **三协议兼容** — OpenAI `chat/completions`（流式 / 非流式）、OpenAI Responses（`/v1/responses`，含 Codex CLI 的 custom tool 翻译）、Anthropic `messages`，现有 SDK 无需改造即可接入。
- **工具调用闭环** — `tools` / `tool_use` / `tool_result` / `tool_choice` 全链路转换；Postman 原生工具（shell 执行、读文件等）与客户端第三方工具双向桥接（`toolsim.go`），Codex 的 `exec` custom tool 自动翻译（`codex_exec.go`）。
- **会话粘性续接** — 按消息历史算稳定指纹（剥离 `<system-reminder>`/`<total_tokens>` 等随轮变化的包装与客户端续写提示块），首次对话绑定账号与上游 `conversationId`，后续轮次只发新增内容（增量模式），与 Postman 网页版行为一致；指纹未命中时自动折叠全历史重放一轮并自愈回增量模式。多实例部署可配 Redis 共享会话映射。
- **图片识别桥接** — 上游只收纯文本；开启后入站图片块先经外部视觉模型（OpenAI 兼容，默认 xAI Grok）识别成文字再转发，识别结果按图片指纹缓存；失败一律 400，绝不静默丢图。
- **响应缓存（默认关）** — 内置影子探针先量真实命中率，达标后一键开启；命中回放零上游调用（Postman 按次扣额度 = 省一整次），usage 报告 `cached_tokens`。

### 账号池与容错

- **多账号号池** — 轮询 + 最少在途调度；账号额度耗尽 / 认证失败 / 瞬时错误时自动切换到下一个可用账号。
- **403 网关拦截策略** — 识别 Cloudflare 风控拦截（区分「出站体带 WAF 特征」与「零特征疑似风控」）：续聊钉住原号退避重试（换号会丢服务端会话），新对话按剩余额度比例挑号 failover；被拦账号进入可配置冷却。详见 [docs/403-gateway-block-failover.md](docs/403-gateway-block-failover.md)。
- **WAF 特征中和** — 出站文本里的 script 家族标签、行内事件等 Cloudflare WAF 特征以零宽空格破坏形态（对模型阅读无损），出站前自动处理，可 `GATEWAY_DISABLE_WAF_NEUTRALIZE=1` 关闭。详见 [docs/403-waf-neutralization.md](docs/403-waf-neutralization.md)。
- **TLS 指纹对齐** — 出站用 uTLS 模拟 Chromium 指纹 + Brotli/Zstandard 压缩，降低被上游风控识别为非浏览器客户端的概率。
- **出口代理池** — 所有上游流量可经代理池出站（http/https/socks5），403 重试自动轮换出口 IP；账号默认粘同一出口；面板可逐个探测代理延迟。

### 运维与面板

- **真实额度同步** — 每次聊天写入上游返回的真实 `limit / usage / overage`，面板「余量 / 总量」为真实数据；支持额度周期重置倒计时、限流快照与月度用量预测。
- **面板登录** — 设 `ADMIN_PASSWORD` 即启用：HMAC 签名会话 Cookie（7 天），未登录访问跳转 `/login`；未设置则维持无密码模式兼容既有部署。
- **实时数据面板** — 概览、统计分析、请求日志（含完整出站请求/响应排查字段）、号池管理、额度管理、路由策略、代理出口、图片识别、SQL 查询、告警中心、系统设置，全部由真实请求日志聚合，无任何 Mock。
- **SQL 排查** — 面板内置只读 SQL 查询（`/api/sql-query`，带常用预设），可直接对本地 SQLite 做聚合分析。
- **账号导入 / 导出** — 通过 `account.json` 一次性批量导入多个账号（导入后异步刷新额度），也支持导出备份；单账号支持连通性测试。
- **链路追踪** — 每条请求贯穿全链路的 `trace_id`，控制台按行输出事件；可开深追踪把完整请求/上游 SSE/响应体落盘 jsonl（敏感头自动脱敏）。
- **容器化部署** — 纯静态二进制（`CGO_ENABLED=0`，SQLite 用纯 Go 实现，无需 cgo），镜像小、无系统依赖。

## 工作原理

```text
┌──────────┐   OpenAI/Anthropic 协议    ┌──────────────────────────┐   上游服务
│ 客户端     │ ─────────────────────────► │  ps2Api 网关（容器）        │ ──────────────────────►
│ (SDK/Curl)│ ◄───────────────────────── │  账号池 轮询+最少在途        │ ◄──────────────────────
└──────────┘   流式 / 非流式             │  会话粘性 · 失败自动切换     │
                                         │  SQLite 日志/统计/设置/告警   │
                                         └──────────────────────────┘
```

客户端以标准 OpenAI / Anthropic 协议请求本网关；网关从账号池挑选账号，按会话指纹粘住原账号并以上游 `conversationId` 增量续接，把请求转换为上游协议转发，再将上游 SSE 流回写为客户端协议。全过程的请求量、延迟、成本、错误与额度写入 SQLite，面板实时读取。

## 快速开始

要求：**Docker**。

### docker compose（推荐）

```bash
docker compose up -d          # 构建并启动
docker compose logs -f        # 查看日志
docker compose down           # 停止
```

compose 默认启用面板登录（`ADMIN_USERNAME=admin` / `ADMIN_PASSWORD=change-me`，部署后请改掉）并接入 Redis 共享会话映射；不需要的可在 `docker-compose.yml` 里注释掉对应环境变量。

### docker 命令

```bash
docker build -t ps2api .
docker run -d --name ps2api \
  -p 1930:1930 \
  -v "$(pwd)/data:/data" \
  ps2api
```

启动后：

- 数据面板：<http://127.0.0.1:1930/>
- OpenAI 接口：`http://127.0.0.1:1930/v1/chat/completions`
- Anthropic 接口：`http://127.0.0.1:1930/v1/messages`

> `-v ./data:/data` 将账号库与日志持久化到宿主机；不挂载则容器重建后数据丢失。

## 账号接入

网关本身不注册账号，需要导入已登录的上游账号凭据。两种方式：

1. **批量导入（推荐）** — 面板「号池管理 → 导入」上传 `account.json`，或 `POST /api/accounts/import`。
2. **手动添加单个** — 面板「添加账号」，或 `POST /api/accounts`。

`account.json` 格式（`version` 必须为 `1`，可由面板「导出」得到）：

```json
{
    "version": 1,
    "accounts": [
        {
            "email": "your@email.example",
            "source": "manual",
            "enabled": true,
            "tokens": {
                "access_token": "…",
                "user_id": "…",
                "workspace_id": "…",
                "workspace_subdomain": "…"
            }
        }
    ]
}
```

- **令牌型账号**用 `access_token`（需同时提供 `workspace_subdomain`）。
- **会话型账号**用 `sid` 代替 `access_token`（此时 `workspace_subdomain` 可省略）。
- `user_id` 与 `workspace_id` 均为必填。

## 核心用法

### OpenAI 兼容（流式）

```bash
curl http://127.0.0.1:1930/v1/chat/completions \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

用官方 OpenAI SDK 时，把 `base_url` 指向本网关即可：

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:1930/v1", api_key="your-secret-key")
resp = client.chat.completions.create(
    model="claude-opus-4-8",
    messages=[{"role": "user", "content": "写一句 Go 的 Hello World"}],
)
print(resp.choices[0].message.content)
```

### OpenAI Responses（Codex CLI 等）

```bash
curl http://127.0.0.1:1930/v1/responses \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.6-sol","input":[{"role":"user","content":"你好"}],"stream":true}'
```

Codex CLI 把 `base_url` 指向本网关即可使用：客户端声明的 `exec` custom 工具会被自动桥接——Postman 原生工具（`executeShellCommand` / `readFile`）翻译成 exec 调用、`custom_tool_call_output` 回译给上游，工具调用可闭环。

### Anthropic 兼容

```bash
curl http://127.0.0.1:1930/v1/messages \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

网关同时支持 `tools` / `tool_use` / `tool_result`，可直接用于 function calling / agent 场景；`model` 传入官方 Claude 名（如 `claude-opus-4-20250514`）会自动归一到内部模型。

### 数据面板

浏览器打开 <http://127.0.0.1:1930/>：概览、统计分析、请求日志、号池管理、额度管理、路由策略、代理出口、图片识别、数据查询（SQL）、告警中心、系统设置。「系统设置 → 网关配置」可在线读写全部运行参数（重试次数、失败自动切换、403 冷却、账号避让、缓存与探针开关、图片识别、代理出口等），保存即生效、无需重启。

## 配置

环境变量在启动时读取（改后需重启）：

| 环境变量                | 默认                             | 说明                                                              |
| ----------------------- | -------------------------------- | ----------------------------------------------------------------- |
| `GATEWAY_PORT`          | `1930`                           | 监听端口                                                          |
| `DATABASE_PATH`         | `/data/gateway.db`（镜像内）     | SQLite 路径                                                       |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | （空）               | 面板登录凭据。设置 `ADMIN_PASSWORD` 即启用登录（未登录访问跳 `/login`），留空维持无密码模式 |
| `GATEWAY_SESSION_SECRET` | 进程随机生成                    | 登录会话 Cookie 的 HMAC 签名密钥。多副本 / 重启后会话仍有效需显式设置 |
| `REDIS_URL` / `REDIS_ADDR` | （空）                        | Redis 连接串（`redis://user:pass@host:port/db`；`REDIS_ADDR` 兼容裸 `host:port`）。配置后**会话映射**（指纹→conversationId / 归属账号）跨实例共享，解析或连接失败自动回退进程内存储 |
| `REDIS_KEY_PREFIX`      | `ps2api`                         | Redis 键前缀（多套网关共用一个 Redis 时区分）                      |
| `REDIS_CONV_TTL`        | `72h`                            | 会话映射的 TTL（Go duration 串；`0`/`off` 永不过期）                |
| `GATEWAY_LOG_LEVEL`     | `debug`                          | 控制台链路日志级别：`debug`/`info`/`warn`/`error`/`off`。默认 `debug`（全量）；每条请求恒有 `trace_id`，关键事件按行输出，可 `grep <trace_id>` 捞出完整链路 |
| `GATEWAY_TRACE_LOG`     | `0`                              | 设为 `1` 额外把完整请求 / 路由 / 上游 SSE 与响应体落盘为 jsonl 深追踪文件（与控制台链路日志相互独立） |
| `GATEWAY_TRACE_DIR`     | `./data/traces`                  | 深追踪文件根目录；按日期分目录，每个请求单独生成 `<trace_id>.jsonl` |
| `GATEWAY_DISABLE_WAF_NEUTRALIZE` | `0`                     | 设为 `1` 关闭出站 WAF 特征中和（默认开启）                          |
| `GATEWAY_DISABLE_THIRD_PARTY` | `0`                         | 设为 `1` 停止向上游播发客户端第三方工具                              |

> **API Key** 不再通过环境变量配置：首次启动后在面板「系统设置 → 安全与认证」填入并保存，写入 SQLite 后立即生效，面板会自动缓存。留空则关闭鉴权。

> **运行参数**（重试、failover、403 冷却、账号避让、缓存 / 探针 / 图片识别 / 代理出口等）全部在面板「系统设置」在线配置、存 SQLite、即时生效，无需环境变量或重启。

> 控制台链路日志默认开启（`GATEWAY_LOG_LEVEL=debug`）：每个请求都会生成贯穿全链路的 `trace_id`，入口访问日志行与各链路事件行都以 `[<短trace_id>]` 前缀对齐，`body`/`headers`/`content`/`messages` 只打字节数，其它值截断，不含对话正文。需要降噪时可调高级别（如 `warn`）或设 `off` 关闭。
>
> 深追踪文件（`GATEWAY_TRACE_LOG=1`）默认关闭，含完整请求 / 响应正文与工具结果；开启后 `Authorization`、`Cookie`、密码、API Key、access token、会话 token 会自动脱敏，排查后应关闭并妥善处理。响应头 `X-PS2API-Trace-ID` 对应该次请求的深追踪文件名。

### 链路排错

一次「命中 Cloudflare 拦截、原号重试后成功」的请求，控制台会输出同一 `trace_id` 前缀的连续事件：

```
[a1b2c3d4] INFO  client.request              method=POST path=/v1/messages body=48213b
[a1b2c3d4] INFO  router.attempt              account_id=17 attempt=1 email=foo@x.com model=claude-…
[a1b2c3d4] WARN  router.gateway_blocked      account_id=17 email=foo@x.com error=…Cf-Ray: …网关拦截…
[a1b2c3d4] WARN  router.gateway_sticky_retry account_id=17
[a1b2c3d4] INFO  router.attempt              account_id=17 attempt=2 email=foo@x.com …
[a1b2c3d4] INFO  router.success              account_id=17 attempt=2 email=foo@x.com
[a1b2c3d4] POST /v1/messages -> 200 (1230ms)
```

`grep a1b2c3d4` 即可捞出这条请求从入口 → 路由 → 上游、每次 failover（哪个号被拦、切到哪个号）到最终结果的完整链路；需要请求 / 响应原文时再开 `GATEWAY_TRACE_LOG=1` 查对应 `data/traces/<日期>/a1b2c3d4.jsonl`。

## API 参考

Base URL：`http://127.0.0.1:1930`。除面板只读接口外，均需 `Authorization: Bearer <API_KEY>`。

| 方法           | 路径                                                   | 说明                                                    |
| -------------- | ------------------------------------------------------ | ------------------------------------------------------- |
| POST           | `/v1/chat/completions`                                 | OpenAI 兼容（流式 / 非流式）                            |
| POST           | `/v1/responses`                                        | OpenAI Responses（流式 / 非流式，Codex CLI 可直连）     |
| POST           | `/v1/messages`                                         | Anthropic 兼容                                          |
| GET            | `/v1/models`                                           | 模型列表                                                |
| POST           | `/api/login` · `/api/logout`                           | 面板登录 / 登出（设 `ADMIN_PASSWORD` 后启用）           |
| GET / POST     | `/api/accounts`                                        | 账号列表 / 手动添加                                     |
| GET            | `/api/accounts/export`                                 | 导出 `account.json`                                     |
| POST           | `/api/accounts/import`                                 | 导入 `account.json`（导入后异步刷新额度）               |
| PATCH / DELETE | `/api/accounts/{id}`                                   | 启用 / 停用、删除                                       |
| POST           | `/api/accounts/{id}/refresh-quota`                     | 单账号刷新额度快照                                      |
| POST           | `/api/accounts/{id}/test`                              | 单账号连通性测试                                        |
| POST           | `/api/refresh-quota`                                   | 对所有启用账号发起轻量探测，更新额度周期与限流快照        |
| GET            | `/api/stats`                                           | 累计请求、成功率、平均延迟、P95、成本、错误率、今日请求 |
| GET            | `/api/analytics?days=N`                                | 日 / 时序列、模型分布、渠道对比、账号排行、热力图       |
| GET            | `/api/logs`                                            | 最近请求日志（条数可配）                                |
| GET            | `/api/request-logs`                                    | 请求日志明细（含出站体 / 响应等排查字段）               |
| GET / PUT      | `/api/settings`                                        | 系统设置读写                                            |
| POST           | `/api/sql-query`                                       | 对本地 SQLite 执行只读查询（SELECT/WITH/EXPLAIN）       |
| GET / DELETE   | `/api/cache-probe`                                     | 缓存探针统计 / 重置度量窗口                             |
| POST           | `/api/proxy-check` · `/api/proxy-test`                 | 出口代理连通性探测                                      |
| GET            | `/api/alerts`                                          | 告警记录                                                |
| POST           | `/api/alerts/{id}/resolve` · `/api/alerts/resolve-all` | 处理单条 / 全部告警                                     |
| GET            | `/health`                                              | 健康检查（无需鉴权）                                    |

## 支持的模型

`GET /v1/models` 返回完整列表。当前包含：

| 模型                                      | 上下文 | 最大输出 | 思考 |
| ----------------------------------------- | ------ | -------- | ---- |
| `claude-opus-4-8` / `4-7` / `4-6` / `4-5` | 200K   | 64K      | ✓    |
| `claude-sonnet-4-6` / `4-5`               | 200K   | 64K      | ✓    |
| `claude-haiku-4-5`                        | 200K   | 64K      | —    |
| `gpt-5.6-sol` / `terra` / `luna`          | 128K   | 32K      | ✓    |
| `codex-mini-latest`                       | 128K   | 32K      | ✓    |
| `gpt-5.5` / `5.4` / `5.2`                 | 128K   | 32K      | —    |
| `auto`                                    | 200K   | 64K      | —    |

## 已知限制

### Codex CLI：经 `/v1/responses` + exec 翻译桥接可用，边界仍存在

本网关的上游只有一条链路——Postman **Agent Mode**（`gateway.postman.com/chat` / `{sub}.postman.co/_gw/chat`，`x-pstmn-req-service: agent-mode-service`）。Postman 没有暴露任何"纯 completion / 非 Agent Mode"端点，服务端 harness（系统提示词 + 原生工具目录 + exec 运行时）是强制的，无法关闭。

「客户端本地执行工具」最初在这里撞墙：Agent Mode 模型会以**服务端 exec 编排格式**回调客户端保留工具（`functions__*`、`collaboration__*`）——参数是一段调用 `tools.exec_command` / `apply_patch` 的 JavaScript 程序而非结构化命令，客户端无法执行，逐条回 `unsupported call` 形成死循环。

当前的处理（两层）：

- **止血**：出站构造第三方工具时过滤客户端保留命名空间（`functions__` / `collaboration__`，见 [internal/provider/toolsim.go](internal/provider/toolsim.go)），不再向上游播发必然无法执行的工具。
- **桥接**：对声明了 `exec` custom 工具的客户端（运行时探测 `additional_tools`，见 [internal/api/codex_exec.go](internal/api/codex_exec.go)），把可映射的 Postman 原生工具（`executeShellCommand` / `readFile`）翻译成一次 `exec` custom tool 调用（input 为 JS 文本，`const r = await tools.exec_command(...); text(...)`），客户端执行后 `custom_tool_call_output` 回译给上游——工具调用可闭环，Codex CLI 把 `base_url` 指向本网关即可工作。

**仍然做不到**：

- 只翻译了能等价为一条 shell 命令的原生工具；其余原生工具（无 exec 等价实现）不经此桥。
- 注入提示词改不了 exec 的编排格式（很可能已 fine-tune 进模型）。
- 多客户端并发共用时，Codex 的工作目录语义由 `projectPath` 参数近似传递，非完全一致的沙箱语义。

**正确定位**：本网关是 **Postman Agent 的 OpenAI / Anthropic 兼容外壳**——适合当对话后端、Postman 工作区后端使用；「让本地 Agent 客户端借 Postman 额度在本机干活」目前对 Codex CLI 走通了主链路，其他客户端视其 custom tool 能力而定。

### 缓存 / 省额度：先用「影子探针」测命中率，再决定要不要建

Postman 按「每次调用扣额度」计费（不按 token），所以一次响应缓存命中 = 省掉一整次上游调用。但缓存能不能省到额度，**完全取决于命中率**，而 agent/Codex 类流量的请求体里塞满了随人/随天变化的内容（绝对路径 `/Users/<name>`、`cwd`、注入的当天日期），不同开发者发同一需求算出的 key 也对不上——**跨人命中率结构性接近 0**。因此本网关**不预先内置响应缓存**，而是提供一个零风险的「影子探针」先量出你团队的真实数字：

- 打开（默认关，避免探针表无界增长）：`PUT /api/settings` 设 `cache_probe_enabled=true`，或在设置页开启。
- 探针**只度量、绝不改变任何返回值**：对每个「单发、无状态」请求（排除工具结果回传轮次）记录指纹，剥掉 volatile 的 `<total_tokens>` 尾巴。
- 读结果：`GET /api/cache-probe` →
  - `potentialHitRate` — 若开启响应缓存的潜在命中率；
  - `singleflightSaved` — 同一指纹并发在途的撞车次数（= single-flight 并发去重本可省的调用数，运行时计数、多实例各计各的会低估）。
- 重置度量窗口：`DELETE /api/cache-probe`。

**决策路径**：跑几天看数字——`potentialHitRate` 明显 >0 才值得建响应缓存；`singleflightSaved` >0 才值得建并发去重。两者都需要真实数字支撑，别在数字出来前建（尤其流式回放、跨主机共享缓存/Redis 这些）——当前流量倾向于「都省不到」。

**响应缓存（按探针数字建成，默认关）**：探针测得 `potentialHitRate` 明显 >0 后，可在设置页开 `cache_enabled`。命中即回放（零上游调用，省一整次额度扣减）：

- 只缓存「单发无状态请求」的「成功且无 tool_calls」响应——带 tool_calls 的回放会让后续 tool-tail 续接丢会话映射（conversationId），刻意排除；
- 内存 LRU（512 条）+ 24h TTL，单实例；键复用探针指纹（`provider.CacheKey`）；
- 命中时 usage 报告缓存命中：OpenAI `prompt_tokens_details.cached_tokens` / Anthropic `cache_read_input_tokens`；
- 命中计数在缓存探针面板展示（`cacheHits` / `cacheMisses` / `cacheEntries`）。

## 开发

```bash
CGO_ENABLED=0 go build ./...                          # 编译（纯静态，无需 cgo）
go vet ./...                                          # 静态检查
go test ./...                                         # 单元测试
node --check internal/dashboard/static/dashboard.js   # 前端语法检查
docker build -t ps2api .                              # 构建镜像
```

CI（`.github/workflows/ci.yml`）：Linux 全量测试 + Docker 构建与容器冒烟。贡献前请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 目录结构

```text
main.go                      # 入口：裸 HTTP 服务
Dockerfile / docker-compose.yml / .dockerignore   # 容器构建与编排
internal/
  api/                       # HTTP 路由、三协议转换、Codex exec 桥接、面板 API、登录、图片识别入口
  provider/                  # 上游协议（令牌/会话）、SSE 解析、会话指纹/粘性、WAF 中和、uTLS、
                             #   图片识别桥接、token 估算
  pool/                      # 账号池调度与状态
  router/                    # 请求路由、重试/failover、403 策略、响应缓存、探针、用量持久化、链路日志
  store/                     # SQLite：账号/日志/设置/告警 + 聚合统计
  dashboard/static/          # 面板前端（独立，无构建步骤）
docs/                        # 协议笔记、403 拦截与 WAF 中和、事故分析
```

## 许可

代码以 **MIT License** 开源，见 [LICENSE](LICENSE)。
