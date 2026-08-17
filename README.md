# ps2Api

将上游 AI 账号聚合成一个本地网关，对外暴露 **OpenAI 兼容**（`/v1/chat/completions`）与 **Anthropic 兼容**（`/v1/messages`）接口。多账号自动轮询、故障切换、真实额度同步，附带一个实时数据面板。以 **Docker** 运行（无界面、纯静态二进制）。

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

- **双协议兼容** — 同时提供 OpenAI `chat/completions`（流式 / 非流式）与 Anthropic `messages` 接口，现有 SDK 无需改造即可接入。
- **多账号号池** — 轮询 + 最少在途请求调度；账号额度耗尽 / 认证失败 / 瞬时错误时自动切换到下一个可用账号。
- **真实额度同步** — 每次聊天写入上游返回的真实 `limit / usage / overage`，面板「余量 / 总量」为真实数据。
- **账号导入 / 导出** — 通过 `account.json` 一次性批量导入多个账号，也支持导出备份。
- **实时数据面板** — 请求量、延迟 P95、成本估算、错误率、模型分布、账号排行、热力图等，全部由真实请求日志聚合，无任何 Mock。
- **容器化部署** — 纯静态二进制（`CGO_ENABLED=0`，SQLite 用纯 Go 实现，无需 cgo），镜像小、无系统依赖。

## 工作原理

```text
┌──────────┐   OpenAI/Anthropic 协议    ┌──────────────────────────┐   上游服务
│ 客户端     │ ─────────────────────────► │  ps2Api 网关（容器）        │ ──────────────────────►
│ (SDK/Curl)│ ◄───────────────────────── │  账号池 轮询+最少在途        │ ◄──────────────────────
└──────────┘   流式 / 非流式             │  失败自动切换  ·  真实额度    │
                                         │  SQLite 日志/统计/设置/告警   │
                                         └──────────────────────────┘
```

客户端以标准 OpenAI / Anthropic 协议请求本网关；网关从账号池挑选账号，把请求转换为上游协议转发，再将上游 SSE 流回写为客户端协议。全过程的请求量、延迟、成本、错误与额度写入 SQLite，面板实时读取。

## 快速开始

要求：**Docker**。

### docker compose（推荐）

```bash
docker compose up -d          # 构建并启动
docker compose logs -f        # 查看日志
docker compose down           # 停止
```

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

### Anthropic 兼容

```bash
curl http://127.0.0.1:1930/v1/messages \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

网关同时支持 `tools` / `tool_use` / `tool_result`，可直接用于 function calling / agent 场景；`model` 传入官方 Claude 名（如 `claude-opus-4-20250514`）会自动归一到内部模型。

### 数据面板

浏览器打开 <http://127.0.0.1:1930/>：概览、统计分析、实时日志、号池管理、额度管理、路由策略、告警中心、系统设置。「系统设置 → 网关配置」可在线读写重试次数、失败自动切换、错误率 / 额度告警阈值、日志保留条数。

## 配置

全部通过环境变量传入：

| 环境变量                | 默认                             | 说明                                                              |
| ----------------------- | -------------------------------- | ----------------------------------------------------------------- |
| `GATEWAY_PORT`          | `1930`                           | 监听端口                                                          |
| `DATABASE_PATH`         | `/data/gateway.db`（镜像内）     | SQLite 路径                                                       |
| `GATEWAY_TRACE_LOG`     | `0`                              | 设为 `1` 记录客户端请求、路由、上游请求 / SSE 与响应，供故障排查  |
| `GATEWAY_TRACE_DIR`     | `./data/traces`                  | 追踪日志根目录；按日期分目录，每个请求单独生成 `<trace_id>.jsonl` |

> **API Key** 不再通过环境变量配置：首次启动后在面板「系统设置 → 安全与认证」填入并保存，写入 SQLite 后立即生效，面板会自动缓存。留空则关闭鉴权。

> 追踪日志默认关闭。开启后 `Authorization`、`Cookie`、密码、API Key、access token、会话 token 会自动脱敏，但日志仍含对话正文与工具结果，排查后应关闭并妥善处理。响应头 `X-Postman2API-Trace-ID` 对应该次请求的日志文件名。

## API 参考

Base URL：`http://127.0.0.1:1930`。除面板只读接口外，均需 `Authorization: Bearer <API_KEY>`。

| 方法           | 路径                                                   | 说明                                                    |
| -------------- | ------------------------------------------------------ | ------------------------------------------------------- |
| POST           | `/v1/chat/completions`                                 | OpenAI 兼容（流式 / 非流式）                            |
| POST           | `/v1/messages`                                         | Anthropic 兼容                                          |
| GET            | `/v1/models`                                           | 模型列表                                                |
| GET / POST     | `/api/accounts`                                        | 账号列表 / 手动添加                                     |
| GET            | `/api/accounts/export`                                 | 导出 `account.json`                                     |
| POST           | `/api/accounts/import`                                 | 导入 `account.json`                                     |
| PATCH / DELETE | `/api/accounts/{id}`                                   | 启用 / 停用、删除                                       |
| POST           | `/api/refresh-quota`                                   | 对所有启用账号发起轻量探测，更新额度周期与限流快照        |
| GET            | `/api/stats`                                           | 累计请求、成功率、平均延迟、P95、成本、错误率、今日请求 |
| GET            | `/api/analytics?days=N`                                | 日 / 时序列、模型分布、渠道对比、账号排行、热力图       |
| GET            | `/api/logs`                                            | 最近请求日志（条数可配）                                |
| GET / PUT      | `/api/settings`                                        | 系统设置读写                                            |
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
| `gpt-5.5` / `5.4` / `5.2`                 | 128K   | 32K      | —    |
| `auto`                                    | 200K   | 64K      | —    |

## 已知限制

### 不支持"客户端本地执行工具"型的 Agent 客户端（如 Codex CLI）

本网关的上游只有一条链路——Postman **Agent Mode**（`gateway.postman.com/chat` / `{sub}.postman.co/_gw/chat`，`x-pstmn-req-service: agent-mode-service`）。Postman 没有暴露任何"纯 completion / 非 Agent Mode"端点，所以服务端的 harness（系统提示词 + 原生工具目录 + exec 运行时）是强制的，无法关闭。

这带来一个根本性的不兼容：**把网关接给"自己在本地执行工具"的 Agent 客户端（如 Codex CLI）时，工具调用无法闭环。**

现象与成因：

- 客户端会注册自己的保留工具（`functions__*`、`collaboration__*`）。Agent Mode 模型（`gpt-5.6-sol` 等）收到后，会以**服务端 exec 编排格式**回调它们——`functions__exec` 的参数是一段调用 `tools.exec_command` / `apply_patch` 的 **JavaScript 程序**（含 `Promise.all`、变量绑定、对结果的后续计算），而不是一次结构化的命令调用。
- 客户端无法执行这种调用，回 `unsupported custom tool call` → 网关据此熔断（`400 tool_execution_error`，见 [internal/api/api.go](internal/api/api.go)）→ 客户端重发同一调用 → 死循环。
- 该调用的 `toolCallGroupId` 恒为 `null`，无法经 Postman 原生工具回传协议闭环。

已做的处理与边界：

- **止血（已实现）**：出站构造第三方工具时过滤掉客户端保留命名空间（`functions__` / `collaboration__`，见 [internal/provider/toolsim.go](internal/provider/toolsim.go) 的 `isClientReservedTool`）。这样不再向上游播发这些必然无法经代理执行的工具，**死循环被彻底消除**。
- **无法做到**："让 Codex 在本机执行命令"。这不是难度问题——Postman 只有 Agent Mode 一条路由，服务端 harness 拿不掉。
- **注入提示词也改不了**：注入的系统提示词能影响回复语言 / persona，但**改不了 exec 的编排格式**（很可能已 fine-tune 进模型），实测仍为 `functions__exec`。

**正确定位**：本网关是 **Postman Agent 的 OpenAI / Anthropic 兼容外壳**——适合当对话后端、Postman 工作区后端使用；不适合作为"让本地 Agent 客户端借 Postman 额度在本机干活"的桥接。

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
  api/                       # HTTP 路由、账号导入、面板 API
  provider/                  # 上游协议（令牌 / 会话）、SSE 解析、token 估算
  pool/                      # 账号池调度与状态
  router/                    # 请求路由、重试、用量持久化、全量日志
  store/                     # SQLite：账号/日志/设置/告警 + 聚合统计
  dashboard/static/          # 面板前端（独立，无构建步骤）
docs/                        # 协议笔记
```

## 许可

代码以 **MIT License** 开源，见 [LICENSE](LICENSE)。
