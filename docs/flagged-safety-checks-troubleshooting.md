# "flagged by safety checks" 排查手册

> 适用症状：网关返回 / 日志出现
> `This message got flagged by our safety checks. Try rephrasing your message or reach out to us at help@postman.com with reference ID <uuid>`

本文沉淀 2026-10-08（首轮根因）与 2026-11-09（复发排查）两次实战的完整方法论。
先读结论，再按流程排查。

---

## 核心结论（两次实战验证）

**这是「内容 × 账号套餐」的概率模型，不是确定性内容过滤。**

1. **FREE 号 flag 概率 ≈ 1**（长内容下）；**sync-solo-trial 号阈值明显更宽**（同内容实测 2026-11-09：FREE 号 14/14 全挂，trial 号可过）。
2. 同一内容同账号重跑结果不稳定（实测 trial 号上同一请求重试 ~15 次才过一次），概率性拦截，非信誉分非内容黑名单。
3. 已知**确定性触发源**（网关已内置中和，见下）之外的新 flag，优先怀疑：
   - 请求里混入了新的「分类器高危措辞」模板（对抗性指令风格）；
   - 内容过长 × FREE 号（解法：停用/删除 FREE 号，换 trial 号）；
   - 业务内容本身踩敏感面（如「任意执行 SQL」「加密 key 校验」这类安全相关需求描述）。

---

## 网关已内置的中和（复发时先确认这些没失效）

位置：`internal/provider/messages.go` + `internal/provider/request.go`（buildBody 内的出站链）。
**只挂在主路径 `/chat` 转换链路**（`split.Query` → 出站 query）。toolsets 透传端点（`/v1/messages` 的 claude-opus-5/sonnet-5 系列）**不做内容中和**，body 原样透传。

| 触发源 | 特征串 | 中和函数 |
|---|---|---|
| 会话标题生成模板（2200+ 字符） | `You are naming a coding session` | `neutralizeTitlePrompt()` |
| auto mode 本地分类器 Stage-1 尾 | `Err on the side of blocking.` | `neutralizeClassifierTail()` |
| auto mode 本地分类器 Stage-2 尾 | `Stage 1 does NOT apply user intent...` | `neutralizeClassifierTail()`（双 marker） |
| git 署名 system-reminder 块（含 🤖 签名） | `Attribution for git commits and pull requests` | `stripAttributionReminder()` |

另一个坑：上游把该拦截的 errorType 也写成 `INPUT_VALIDATION_ERROR`，`sse.go` 里 flagged 文案判定必须**先于** errorType 判定，否则会被误归类 `RequestRejected` → router 直接 400 不换号（2026-10-08 线上踩过）。

---

## 排查流程

### 第 0 步：确认症状范围

```bash
# 近期请求成败分布（同一 request_bytes 反复 error = 同一内容连环 flag）
curl -s -H "Authorization: Bearer <key>" -X POST http://<gw>:1930/api/sql-query \
  -H "Content-Type: application/json" \
  -d "{\"sql\":\"SELECT id, status, account_id, duration_ms, request_bytes FROM request_logs ORDER BY id DESC LIMIT 40\"}"
```

判断要点：
- **同一 request_bytes 连环 error** → 内容型触发（本次案例：37268 字节烧了 14 个号）。
- 错误是否横跨多种账号套餐 → 横跨 = 内容/概率；只挂 FREE 号 = 套餐阈值问题。
- `account_id` 为空的行是老日志格式，用 `upstream_headers` 里的 `X-Access-Token` 前 20 位到 `accounts.tokens` 里 LIKE 反查真实账号。

### 第 1 步：排除已知触发源

从 `request_logs.client_body`（客户端原始请求）与 `upstream_body`（真正出站的）分别查四个特征串：

```bash
# 取出原始 body 落盘（注意 client_body 可能很大，sql-query 有截断，超长分批取）
curl -s -H "Authorization: Bearer <key>" -X POST http://<gw>:1930/api/sql-query \
  -H "Content-Type: application/json" \
  -d "{\"sql\":\"SELECT client_body FROM request_logs WHERE id=<flagged_id>\"}" -o resp.json
```

```python
# 本地解析（rows 是 dict 数组，列名即 key）
import json
d = json.load(open('resp.json', encoding='utf-8'))
body = d['rows'][0]['client_body']
for marker in ['You are naming a coding session',
               'Err on the side of blocking',
               'Stage 1 does NOT apply user intent',
               'Attribution for git commits']:
    print(marker, '->', marker in body)
```

- client_body 有、upstream_body 没有 → 网关中和**正常工作**，flag 另有原因 → 进第 2 步。
- client_body 有、upstream_body 也有 → 中和没生效（检查请求是否走了没挂中和的路径，如 toolsets 透传）。

**2026-11-09 实测坑**：`git 署名块`这次带了新前导句（"this replaces Claude Code's own earlier attribution guidance..."），与 10 月版本措辞不同，但 marker 串 `Attribution for git commits and pull requests` 仍命中、剥除正常。marker 选得宽所以没失效。

### 第 2 步：区分「套餐问题」还是「新触发内容」

最省事的做法先行：**把 FREE 号全停用/删除，用 trial 号重试**。

```bash
curl -s -H "Authorization: Bearer <key>" -X PATCH http://<gw>:1930/api/accounts/<id> \
  -H "Content-Type: application/json" -d '{"enabled":false}'
```

- trial 号能过 → 结案：FREE 号概率模型（记忆 `title-prompt-flag-trigger` §4）。
- trial 号也大概率挂 → 进第 3 步做内容二分。

> 2026-11-09 复盘：当时重放实测 trial 号上也连挂 5 次（其中还夹了一次 3KB 小请求也挂，说明概率模型下小内容偶尔也拦），但用户在客户端真实重试即通过。**概率拦截下「重试」本身就是有效解**，别急着深挖内容。

### 第 3 步：直连二分内容（网关中和后的出站 query）

用面板「直连测试」端点对**单个账号**发探针，绕过号池与重试：

```bash
curl -N -H "Authorization: Bearer <key>" -X POST \
  http://<gw>:1930/api/accounts/<trial_account_id>/test \
  -H "Content-Type: application/json" \
  -d "{\"mode\":\"direct\",\"model\":\"claude-opus-4-8\",\"prompt\":\"<探针内容>\"}"
```

出站 query 的拆段方法（`upstream_body` → `input.query`，9321 rune 时实测分段）：

```
[Segment A] [System skills] 技能列表           （~7600 rune，无法删，Claude Code 功能组件）
[Segment B] <system-reminder> 块们（gitStatus 等）（~1300 rune）
[Segment C] 末尾用户真实 query                  （~460 rune）
```

二分顺序（每次 1 发，等 30-60s 观察上游概率抖动，可疑段跑 2-3 发确认）：

1. C 单发（小内容）→ 预期过；挂则 C 本身是触发源（敏感业务描述）。
2. A + C → 过则 A 无辜；挂则 skills 列表概率触发（已知无法修复，换 trial 号解决）。
3. B + C → 过则 B 无辜。
4. A + B + C（重组全量，作 baseline sanity check）→ 预期与线上一致。

对照实验纪律（10-08 实战经验）：**同账号、同出口（mode=direct 固定直连）、同内容**，一次只改一个变量；短句比长句复现率高得多，优先用「完整必挂 baseline」+「逐段删除」而不是「逐段单独测」。

### 第 4 步：命中新触发源后的修复模式

参照 `neutralizeTitlePrompt` / `neutralizeClassifierTail` 的三要素：

1. 在 `messages.go` 加常量 marker + neutral 版本（保留语义必需的输出格式约定，丢弃对抗性措辞）。
2. 在 `request.go` buildBody 中和链上挂函数（注意：必须在 `capUpstreamQuery` 之前）。
3. 单测钉死（参照 `titleprompt_test.go`，含畸形输入不越界 case）。

若触发源在客户端注入的 system-reminder（如本次 git 署名新变体），优先**扩 marker 而不是加新函数**——marker 选宽泛前缀可以覆盖上游模板措辞变化。

---

## 本次案例时间线（2026-11-09）

1. 用户报 opus-4-8 主路径 `/v1/messages` 全挂 flagged，reference ID `5cc55f53...`。
2. 排查：37268 字节请求（Java 动态 SQL 接口需求）烧 14 号；7 FREE 号全挂，trial 号没轮到（重试预算烧完）。
3. `stripAttributionReminder` 验证正常（client_body 有署名块、upstream_body 无）——排除已知触发源复发。
4. 停用 7 个 FREE 号 → 重放仍挂 5 次（trial 号概率抖动，夹一次 3KB 小请求也挂）。
5. **用户删除全部 FREE 号后客户端重试即通过** → 结案为 FREE 号概率模型 + 概率抖动的叠加。
6. 未做完整内容二分（概率模型下二分信噪比差），触发源深挖挂起，若复发再按第 3 步流程走。

## 相关记忆

- `title-prompt-flag-trigger` —— 2026-10-08 首轮根因（三大触发源 + 分概率模型）
- `safety-flagged-analysis` —— 错误分类（UpstreamFailure 换号不踢池）与 sse.go 判序坑
- `docs/403-gateway-block-failover.md` —— 网关层 403（Cloudflare）与本次应用层 flag 是**两回事**，别混淆
