# Cloudflare WAF 中和（wafNeutralize）：实证、演进与排查手册

> 承接 `403-gateway-block-failover.md`（确认了 403 诱因是出站体的类 HTML/JS 内容形状）。本文档记录其后「中和方案」的三轮演进、对 Cloudflare 匹配行为的实测结论、最终实现，以及遗留问题的排查运行手册。
> 涉及服务：Postman → API 的 HTTP 代理/网关（Go 实现）。

---

## 1. 问题定义

出站请求体的文本出口（`input.query` 与 `toolResponses[].content`）里，前端项目源码天然携带 `<script>`、`<template>`、`onerror=` 等标记，确定性触发 Cloudflare WAF 托管内容规则 → 上游 403 → 客户端看到 529。

约束（铁律）：

- **指纹不动原文**：会话粘性 / 会话查找对 `req.Messages` 原文做 sha256 指纹，中和只允许作用于**出站序列化副本**。
- **模型可读**：中和后的文本必须仍能被模型正确理解（编辑工具调用依赖逐字匹配回读内容）。
- **幂等**：同一字符串重复中和不产生二次破坏。

## 2. 三轮演进（每轮都被实测推翻或证实）

### 2.1 第一轮：逐特征插空格

17 条 per-signature 规则（`<script` → `< script` 之类）。部署后 403 减少，但仍有泄漏。

**泄漏根因（转义形）**：tool result 的 wrap 路径用 `json.Marshal` 把非 JSON 内容包成 `{"status":...,"message":...}`，marshal 会把 `<` 转成 `\u003c` 六字符字面量转义序列——裸文本正则对此失明，中和形同虚设。修复：中和移到 wrap **之前** + 补转义形规则。

（另一处实现教训：`json.Marshal` 之后二次中和救不回来，必须在序列化前处理原始文本。）

### 2.2 第二轮：按特征类别合并规则

17 条逐特征规则收敛为 4 条类别规则（危险标签开头 / `on*=` 事件处理器 / `javascript:` URI / Vue 指令），用捕获组边界 + replace 模板插破坏字符。扩特征只需往交替组加词。

### 2.3 第三轮：破坏字符从空格换成零宽空格（当前方案）

**空格方案有天花板**：通过真实网关直连上游做对照实验，发现 Cloudflare 匹配前做归一化——**剥离所有空白与反斜杠**。因此：

- `< script>` / `<scr ipt>` / `<\script` 单独在场仍然 403（插空格全部无效）；
- **零宽空格（U+200B）与全角字符在归一化后存活**：`<` + ZWSP + `script>` 正常放行。

选零宽空格而非全角：对 tokenizer 基本不可见（阅读无损）、四类规则统一可用、幂等（破坏后的形态不会再命中规则）。

**另一个实测修正（探测门）**：`wafNeutralize` 原以 `WafSignatureHitCount == 0` 短路放行，但探测表只有 `<script` 形态，认不出 `</script>`-only 的载荷（如只剩闭合标签的截断 tool result）——实测 CF 对 `</script>` 单独在场即 403。已删掉探测门，4 条规则全跑（对干净文本近零开销）。

## 3. Cloudflare 匹配行为实测结论（2026-09-09，经网关直连上游 13 组探针）

| 输入形态 | 上游结果 |
|---|---|
| 干净文本 / `alert(1)` / 裸 `script` | 放行 |
| `< div>` `< span>`（无 script 家族标签） | 放行 |
| `< img src=x onerror=alert(1)>`（裸 handler，无 script 标签） | 放行 |
| `＜script src=x>`（全角） | 放行 |
| `<` + ZWSP + `script src=x>` | 放行 |
| ZWSP 版 .vue 片段 / ZWSP 版 handler | 放行 |
| `< script>` / `<scr ipt>`（空白分离形） | **403** |
| `</script>`（仅闭合标签） | **403** |
| `<\script`（反斜杠形） | **403** |

结论：

1. **归一化剥空白 + 剥反斜杠后匹配**，所以空格/反斜杠不是有效破坏字符；
2. **真正的特征只有 script 家族标签（开/闭都算）**；`onerror=`、`javascript:`、裸 `script` 单独在场均放行。其余三类规则是保险层；
3. 空白分离形（`<scr ipt>`）真实代码里不存在，规则不覆盖，属可接受的已知边界。

## 4. 最终实现（`internal/provider/waf.go`）

- `wafBreak = "\u200b"`（Go 转义序列写零宽空格 U+200B，源码保持纯 ASCII，杜绝不可见字符）。
- 4 条类别规则，模式形如 `(?i)(<|\\u003c)([!/?]?(?:script|iframe|svg|template|...)\b)` → `"${1}"+wafBreak+"${2}"`；转义形 `<` 字面量与原始 `<` 同规则覆盖。
- `wafNeutralizeEnabled()`：`GATEWAY_DISABLE_WAF_NEUTRALIZE=1` 关闭（kill-switch）。
- 调用点（全部文本出口）：
  - `request.go` buildBody：`input.query`（先中和再 cap，长度仍受 10000 rune 上限约束）；
  - `localmode.go` nativeToolResponse 的 add()：`toolResponses[].content`，**在 json.Marshal wrap 之前**。
- 验证：`go vet` + `go test ./...` 全绿；经本地网关端到端实测，`.vue` 片段、`<script src=x onerror=...>`、`</script>`-only 均从 529 翻绿为 200。

## 5. 已知边界与遗留项

| 项 | 状态 | 处置 |
|---|---|---|
| 空白分离形（`< script>` / `<scr ipt>`） | 不覆盖 | 真实代码不存在此形态，不修 |
| 86KB 多 tool 的 .vue 真实场景 | **未验证** | 需真实 agent 流量回归；见第 6 节判定 |
| 零宽字符被模型回显进编辑块 | 理论风险 | 编辑不匹配重试自愈，接受 |
| 单条 tool content 上限 | 已有 16KB/条（`MaxToolResponseContentLen`） | 若体积因素坐实，需加**总量**预算 |

## 6. 排查运行手册（86KB 场景失败时）

面板 → SQL 查询控制台。

**第一步：找到失败行，看三个判别量**

```sql
SELECT id, datetime(substr(created_at,1,19)) t, status, request_bytes,
  length(upstream_body) body_len,
  -- 原始特征在场（中和泄漏）
  instr(lower(upstream_body), '<script') > 0
    OR instr(lower(upstream_body), '\u003cscript') > 0 AS raw_sig,
  -- ZWSP 中和形态在场（说明中和跑了）
  instr(upstream_body, char(8203)) > 0 AS zwsp,
  error_message
FROM request_logs
WHERE error_message LIKE '%403%' OR error_message LIKE '%Cloudflare%'
ORDER BY id DESC LIMIT 10;
```

**第二步：按 raw_sig / zwsp / body_len 三分走**

| 判别结果 | 结论 | 动作 |
|---|---|---|
| `raw_sig=1` | 中和泄漏，规则没盖住 | 取 `upstream_body` 命中片段，按类别加词 |
| `raw_sig=0, zwsp=1`，body_len ≥ 80K 且同桶无成功行 | **体积因素**（内容已干净） | 加 toolResponses 总量预算，压回 70K 以下 |
| `raw_sig=0, zwsp=1`，body_len 与成功行同桶 | 风控型（IP/账号/速率） | 第三步 |
| `raw_sig=0, zwsp=0` | 内容本来就干净，风控型 | 第三步 |

体积分桶对照：

```sql
SELECT (request_bytes/10000)*10 || 'K' bucket,
  SUM(status='success') ok, SUM(status!='success') fail,
  MAX(request_bytes) FILTER (WHERE status='success') max_ok
FROM request_logs WHERE substr(created_at,1,10) = date('now')
GROUP BY bucket ORDER BY bucket;
```

**第三步（风控型）：排除账号冷却余波**

零特征 403 会冷却账号，失败后紧跟的失败可能是余波而非新问题：

```sql
SELECT a.id, a.email, a.status, a.status_note, a.updated_at
FROM accounts a ORDER BY a.updated_at DESC LIMIT 5;
```

失败行集中同一 `account_id` = 账号维度；跨号同 IP = 出口维度（查 `egress` 列与代理设置）。

**判别铁律**：一次只改一个变量。`GATEWAY_DISABLE_WAF_NEUTRALIZE=1` 只用于反向验证（关掉后同 payload 必 403，证明此前通过确实是中和的功劳），不要在排查时开着。

## 7. 历史体积数据（判定体积因素的基线）

方案切换前的分布（旧特征时代）：80K+ 全失败、70-80K 有成功（最大 77952）、60-70K 106 过 / 14 败、<60K 422 过 / 6 败。失败集中在 4 个 tool result（出站体 86K）而成功行多为 1-2 个 tool result。**若 ZWSP 版在零特征形态下仍于 80K+ 失败，则体积独立成因坐实。**
