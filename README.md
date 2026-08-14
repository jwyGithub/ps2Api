# Postman2API

将 **Postman Agent Chat** 账号聚合成一个本地网关，对外暴露 **OpenAI 兼容**（`/v1/chat/completions`）与 **Anthropic 兼容**（`/v1/messages`）接口。多账号自动轮询、故障切换、真实额度同步，附带一个实时数据面板。以 **Docker** 运行（无界面、纯静态二进制）。

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)
![Docker](https://img.shields.io/badge/Docker-ready-2496ED.svg)

> ⚠️ **免责声明**
> 本项目通过逆向 Postman Agent Chat 的私有协议实现，**与 Postman 无任何关联，也未获得其授权或认可**。仅供个人学习与技术研究，请勿用于违反 Postman 服务条款的场景（如绕过计费）。私有协议随时可能变更导致功能失效，后果由使用者自行承担。仓库内置的 Postman 火箭图标为其**注册商标**，公开分发时请替换为自有图标。

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
- [开发](#开发)
- [目录结构](#目录结构)
- [许可](#许可)

## 功能特性

- **双协议兼容** — 同时提供 OpenAI `chat/completions`（流式 / 非流式）与 Anthropic `messages` 接口，现有 SDK 无需改造即可接入。
- **多账号号池** — 轮询 + 最少在途请求调度；账号额度耗尽 / 认证失败 / 瞬时错误时自动切换到下一个可用账号。
- **真实额度同步** — 每次聊天写入 Postman 返回的真实 `limit / usage / overage`，面板「余量 / 总量」为真实数据。
- **账号导入 / 导出** — 通过 `account.json` 一次性批量导入多个账号，也支持导出备份。
- **实时数据面板** — 请求量、延迟 P95、成本估算、错误率、模型分布、账号排行、热力图等，全部由真实请求日志聚合，无任何 Mock。
- **容器化部署** — 纯静态二进制（`CGO_ENABLED=0`，SQLite 用纯 Go 实现，无需 cgo），镜像小、无系统依赖。

## 工作原理

```text
┌──────────┐   OpenAI/Anthropic 协议    ┌──────────────────────────┐   gateway.postman.com/chat
│ 客户端     │ ─────────────────────────► │  Postman2API 网关（容器）   │ ──────────────────────►
│ (SDK/Curl)│ ◄───────────────────────── │  账号池 轮询+最少在途        │ ◄──────────────────────
└──────────┘   流式 / 非流式             │  失败自动切换  ·  真实额度    │    x-access-token / sid
                                         │  SQLite 日志/统计/设置/告警   │
                                         └──────────────────────────┘
```

客户端以标准 OpenAI / Anthropic 协议请求本网关；网关从账号池挑选账号，把请求转换为 Postman 上游协议转发到 `gateway.postman.com`，再将上游 SSE 流回写为客户端协议。全过程的请求量、延迟、成本、错误与额度写入 SQLite，面板实时读取。

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
docker build -t postman2api-go .
docker run -d --name postman2api \
  -p 1930:1930 \
  -v "$(pwd)/data:/data" \
  -e API_KEY=postman2api-secret-key \
  postman2api-go
```

启动后：

- 数据面板：<http://127.0.0.1:1930/>
- OpenAI 接口：`http://127.0.0.1:1930/v1/chat/completions`
- Anthropic 接口：`http://127.0.0.1:1930/v1/messages`

> `-v ./data:/data` 将账号库与日志持久化到宿主机；不挂载则容器重建后数据丢失。

## 账号接入

网关本身不注册账号，需要导入已登录的 Postman Agent Chat 账号凭据。两种方式：

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

- **桌面端账号**用 `access_token`（需同时提供 `workspace_subdomain`）。
- **Web 端账号**用 `postman_sid` 代替 `access_token`（此时 `workspace_subdomain` 可省略）。
- `user_id` 与 `workspace_id` 均为必填。

## 核心用法

### OpenAI 兼容（流式）

```bash
curl http://127.0.0.1:1930/v1/chat/completions \
  -H "Authorization: Bearer postman2api-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

用官方 OpenAI SDK 时，把 `base_url` 指向本网关即可：

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:1930/v1", api_key="postman2api-secret-key")
resp = client.chat.completions.create(
    model="claude-opus-4-8",
    messages=[{"role": "user", "content": "写一句 Go 的 Hello World"}],
)
print(resp.choices[0].message.content)
```

### Anthropic 兼容

```bash
curl http://127.0.0.1:1930/v1/messages \
  -H "Authorization: Bearer postman2api-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

网关同时支持 `tools` / `tool_use` / `tool_result`，可直接用于 function calling / agent 场景；`model` 传入官方 Claude 名（如 `claude-opus-4-20250514`）会自动归一到内部模型。

### 数据面板

浏览器打开 <http://127.0.0.1:1930/>：概览、统计分析、实时日志、号池管理、额度管理、路由策略、告警中心、系统设置。「系统设置 → 网关配置」可在线读写重试次数、失败自动切换、错误率 / 额度告警阈值、日志保留条数。

## 配置

全部通过环境变量传入：

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `API_KEY` | `postman2api-secret-key` | 客户端 Bearer Key；设为空字符串则关闭鉴权 |
| `POSTMAN2API_PORT` | `1930` | 监听端口 |
| `DATABASE_PATH` | `/data/postman2api.db`（镜像内） | SQLite 路径 |
| `POSTMAN2API_TRACE_LOG` | `0` | 设为 `1` 记录客户端请求、路由、上游请求 / SSE 与响应，供故障排查 |
| `POSTMAN2API_TRACE_DIR` | `./data/traces` | 追踪日志根目录；按日期分目录，每个请求单独生成 `<trace_id>.jsonl` |

> 追踪日志默认关闭。开启后 `Authorization`、`Cookie`、密码、API Key、access token、`postman.sid` 会自动脱敏，但日志仍含对话正文与工具结果，排查后应关闭并妥善处理。响应头 `X-Postman2API-Trace-ID` 对应该次请求的日志文件名。

## API 参考

Base URL：`http://127.0.0.1:1930`。除面板只读接口外，均需 `Authorization: Bearer <API_KEY>`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/v1/chat/completions` | OpenAI 兼容（流式 / 非流式） |
| POST | `/v1/messages` | Anthropic 兼容 |
| GET | `/v1/models` | 模型列表 |
| GET / POST | `/api/accounts` | 账号列表 / 手动添加 |
| GET | `/api/accounts/export` | 导出 `account.json` |
| POST | `/api/accounts/import` | 导入 `account.json` |
| PATCH / DELETE | `/api/accounts/{id}` | 启用 / 停用、删除 |
| POST | `/api/refresh-quota` | 对未采集额度的账号发起轻量探测并写库 |
| GET | `/api/stats` | 累计请求、成功率、平均延迟、P95、成本、错误率、今日请求 |
| GET | `/api/analytics?days=N` | 日 / 时序列、模型分布、渠道对比、账号排行、热力图 |
| GET | `/api/logs` | 最近请求日志（条数可配） |
| GET / PUT | `/api/settings` | 系统设置读写 |
| GET | `/api/alerts` | 告警记录 |
| POST | `/api/alerts/{id}/resolve` · `/api/alerts/resolve-all` | 处理单条 / 全部告警 |
| GET | `/health` | 健康检查（无需鉴权） |

## 支持的模型

`GET /v1/models` 返回完整列表。当前包含：

| 模型 | 上下文 | 最大输出 | 思考 |
| --- | --- | --- | --- |
| `claude-opus-4-8` / `4-7` / `4-6` / `4-5` | 200K | 64K | ✓ |
| `claude-sonnet-4-6` / `4-5` | 200K | 64K | ✓ |
| `claude-haiku-4-5` | 200K | 64K | — |
| `gpt-5.6-sol` / `terra` / `luna` | 128K | 32K | ✓ |
| `gpt-5.5` / `5.4` / `5.2` | 128K | 32K | — |
| `auto` | 200K | 64K | — |

## 开发

```bash
CGO_ENABLED=0 go build ./...                          # 编译（纯静态，无需 cgo）
go vet ./...                                          # 静态检查
go test ./...                                         # 单元测试
node --check internal/dashboard/static/dashboard.js   # 前端语法检查
docker build -t postman2api-go .                      # 构建镜像
```

CI（`.github/workflows/ci.yml`）：Linux 全量测试 + Docker 构建与容器冒烟。贡献前请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 目录结构

```text
main.go                      # 入口：裸 HTTP 服务
Dockerfile / docker-compose.yml / .dockerignore   # 容器构建与编排
internal/
  api/                       # HTTP 路由、账号导入、面板 API
  provider/                  # Postman 上游协议（桌面/Web）、SSE 解析、token 估算
  pool/                      # 账号池调度与状态
  router/                    # 请求路由、重试、用量持久化、全量日志
  store/                     # SQLite：账号/日志/设置/告警 + 聚合统计
  dashboard/static/          # 面板前端（独立，无构建步骤）
docs/                        # 逆向协议笔记
```

## 许可

- 代码以 **MIT License** 开源（见 [LICENSE](LICENSE)），**不包含** Postman 任何源代码。
- 「Postman」名称与火箭图标为 Postman, Inc. 的注册商标，仓库内置图标仅作本地使用；**公开分发时请替换为自有图标**。
- 上游协议的逆向描述见 `docs/`，仅用于技术说明。
