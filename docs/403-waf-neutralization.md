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

面板 → 「WAF 检测」页（离线分析：签名扫描 + 对照 diff + 体积画像，自动执行本节第一、二步）；
细节排查仍可用 SQL 查询控制台。

**第一步：找到失败行，看三个判别量**

```sql
SELECT id, datetime(substr(created_at,1,19)) t, status, request_bytes,
  length(upstream_body) body_len,
  -- 原始特征在场（中和泄漏）：script 家族（含转义形）+ bin/cat（第 9 节第二类签名）
  instr(lower(upstream_body), '<script') > 0
    OR instr(lower(upstream_body), '\u003cscript') > 0
    OR instr(lower(upstream_body), 'bin/cat') > 0 AS raw_sig,
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

## 8. 后续排查记录（2026-09-10）：概率型 403 与网关内一次重试

ZWSP 版上线后再次出现 403。按第 6 节手册排查，结论与第 6 节预设的分支都不同：

**判别结果**：全部失败行 `raw_sig=0`、`zwsp=0`——今天的 50-62K 出站体里**根本没有任何 HTML/JS 标签**（连 `<div` 都没有），内容型成因彻底排除，`zwsp=0` 属正常（无可中和内容）。注意排查时 `raw_other` 类检查要同时查转义形 `<`（出站体经 json.Marshal，裸 `<` 不存在）。

**真正的形态**：概率型边缘拦截。铁证是 request_logs 里 id 2521（失败）与 2522（成功）同一秒、同账号、同出口、同 body（52404 字节）；2538/2539 成功与 2540/2541 失败同账号同出口仅差 1-2 秒。内容、账号、出口全同仍有成败分岔 → 非确定性。单发拦截率 ~8%（16 败 / 204 总）。速率成因排除（08:57、09:26 等单发低流量分钟也失败，无聚簇）；账号/出口维度排除（失败散布 13 个账号、direct 与三个代理口都有）。

**根因**：route_loop 的 GatewayBlocked 分支一律「不重试、不换号、立即 529」——该契约在内容型 403 时代是对的（重试必复现，纯浪费），但概率型 403 下把重试责任全部推给客户端 SDK，连续两次被拦（~0.6%）就暴露成用户可见错误。

**修复**（route_loop.go GatewayBlocked 分支二分）：

- `WafSignatureHitCount > 0`（内容型，确定性复现）→ 维持原契约：不重试、不冷却、立即终止；
- `== 0`（概率型）→ 冷却账号 + **允许一次网关内重试**（`gwRetried` 标志控制，`maxAttempts++` 扩一档预算，不挤占普通重试额度）：续聊经会话粘性回原号（egressSeq 递增换出口 IP），新对话回号池会跳过刚冷却的号（等效换号重试）。重试仍 403 才终止，剩余尾巴由客户端 529 退避兜底。

验证：`go vet` + `go test ./...` 全绿；7 个既有 GatewayBlocked 测试按新契约更新（共两次上游调用），新增 `TestStreamZeroSignature403RetryRecovers` 覆盖「首次 403、重试成功、客户端无感」的核心路径。

## 9. 第二类内容签名：bin/cat（2026-09-11，七轮探针二分定位）

第 8 节的「概率型」结论需要修正一部分：**存在第二类内容签名，探测表当时看不见它**。

**案发**：14:15:45 起同一会话连续 403（双出口、客户端重试均复现），出站体 0 签名、0 个 `<`、ZWSP=0（无可中和内容），日志误判「非内容形状」。但 14 秒前（14:15:31）同工作区、同 IP、52427 字节的近似体成功。

**定位方法**：成功体与失败体做 970 个文本叶子逐叶 diff——唯一实质差异是新增的 `toolResponses[0].content.message`（93B→4136B，一段 CatPaw2API 的 README + `ls` 输出，纯后端内容、无任何 HTML 标签）。用 repro403 探针七轮二分（F→P→S→V→W→X，每轮 2 次重复，全部确定性复现）：

| 输入形态 | 上游结果 |
|---|---|
| `./bin/cat …` / `/bin/cat …` / `./bin/catpaw2api`（无参数）/ `./bin/Catpaw2api` | **403** |
| `bin/` + ZWSP + `catpaw2api` | 放行 |
| `bin/ls` `bin/sh` `bin/rm` `bin/python` `bin/curl` `bin/myapp` `bin/dogpaw2api` | 放行 |
| `./cat`（无 bin/）、裸 `catpaw2api` | 放行 |

**结论**：特征是字面量 **`bin/cat`**（大小写不敏感）——`cat` 前缀词跟在 `bin/` 后被当作 `cat` 命令执行路径匹配。`catpaw2api` 撞上纯属项目名倒霉；`export API_KEY=`、`sudo/systemctl`、`curl -H "Authorization: Bearer …"`、`X-Header: <…>` 全部实测放行（第 3 节「真正的特征只有 script 家族」修正为：**script 家族标签 + bin/cat**，其余仍是保险层）。

**修复**（与第 4 节同一套机制，零新代码路径）：

- `waf.go` 新增第 5 条规则 `(?i)(s?bin/)(cat)` → `bin/` 与 `cat` 之间插 ZWSP，自动覆盖 `input.query` 与 `toolResponses[].content` 两个既有出口；幂等（破坏后不再命中）。
- `errors.go` 签名探测表补 `bin/cat`，403 取证不再误报「未检出」（也修正第 8 节概率型判定的盲区：当时 raw_sig=0 的 403 里可能混有 bin/cat 内容型，需按新表重分类——同 body 成败分岔的样本仍是真概率型）。

**验证**：单测钉住 cat 形与误伤边界（`catpaw2api` 无 bin/ 前缀、`bin/ls` 必须原样通过）；`go vet` + `go test ./...` 全绿；端到端——修复前 2/2 被 403 的完整 README 原文（探针 I_full_readme），修复后 2/2 成功（出站体 +3 字节 = 一个 ZWSP）。

**已知边界**：只实测了 `cat` 一个命令名（ls/sh/rm/python/curl 均放行）；`bin/` 前必须紧跟 `cat`，`sbin/cat` 由 `(s?bin/)` 覆盖。若未来出现其他命令名触发，按本节方法加词即可。


## 10. 在线探针二分（面板化，2026-09-12）

第 9 节的七轮手工二分已固化为面板「WAF 检测」页的在线探针：

1. 分析面板勾选差异叶子（默认全选；removed 叶子不可选——没有发送到上游的内容）；
2. 「发起探针」→ 确认弹窗（变体数 = 1 对照 + N 叶子，每变体 2 次请求；可指定账号/模型，
   默认首个活跃号 + claude-opus-4-8）；
3. 流程：对照变体（等长纯文本，被拦即中止——账号/出口在风控窗口）→ 叶子轮（逐叶验证）
   → 行级二分（~log2(行数) 轮收敛到触发行，每片 padding 到与原叶子等长）；
4. 页面 2s 轮询展示变体证据表（结果/Ray/出站字节）与触发行结论。

实现要点（`internal/api/waf_probe.go`）：

- 探针请求带 `ChatRequest.WafProbe` 标志：出站 query 跳过 WAF 中和与 capUpstreamQuery
  截断——中和会掐灭已知特征（叶子轮假阴性），截断会破坏二分切片的等长 padding；
- 不 ResetConversation：探针消息带唯一 nonce，指纹必然未命中 → 天然冷启动 USER_QUERY；
  Reset 会清掉该账号全部业务会话映射，干扰线上续聊；
- 绕过 router：不占重试预算、不触发账号冷却、不写 request_logs；
- 同一时刻仅一个 job（重复发起 409），可中止（DELETE），不持久化（重启即丢）；
- 叶子全文受上游 10000 字符 query 上限约束（provider.MaxUpstreamQueryRunes），
  超长叶子（全文 + 前缀超限）会被 400 拒绝，无法逐字探测。

验收基准（bin/cat 案例）：对 2026-09-11 b577d7cb 型 403，叶子轮应命中 README 叶子，
行级二分应收敛到 `./bin/catpaw2api -config config.json` 一行。

## 11. 第三次内容回归：script 分离形（2026-09-17，本文档自身触发）

**案发**：16:59:55-57 同账号（291）、同出口、同 body（91416 字节）4 连败 403（id 3405-3408）。按第 6 节手册判别：`raw_sig=0`、`zwsp=1`。但三个竞争假设逐一被否——

- **体积否**：3 秒前同号 id 3402（body 93061，更大）成功；当天 90K 桶 27 过 4 败、100K 桶 2/2 过，无阈值形态；
- **风控否**：同号 3400-3404 连续 5 单成功，下一秒同 body 确定性 4 连败，非概率型；
- **已知签名否**：raw_sig 三查（script 家族 + bin/cat + 转义形）全零。

**定位**（第 9 节同款叶子 diff 方法）：3404（成功）与 3405（失败）出站体 983 个叶子逐叶 diff，唯一实质差异是 `toolResponses[0].content`（5228B→9108B）——**内容是本排查文档自身的全文**。文档第 3 节表格里手写的「插空格/反斜杠无效」示例（`< script`、`<scr ipt`、`<\script`，出站体中各 4 处 + 反斜杠形）成了真实出站内容。第 3 节探针早已实测这三种形态确定性 403（CF 归一化剥空白与反斜杠后还原为 `<script`），当时判定「真实代码里不存在此形态，不修」——**本文档自己就是那个「真实代码」**，与 bin/cat 案例的 `catpaw2api` 撞名同构：排查工具的载体反噬。

**规则漏因**：第 4 节规则 1 形如 `(?i)(<|<)([!/?]?(?:script|…)\b)`，`<` 与标签名之间不容忍任何字符，三种分离形全部穿透。

**修复**（[waf.go](../internal/provider/waf.go) 新增第 6 条规则，零新代码路径）：

- 对 **script 单词逐字母**容忍分离符：`(?i)(<|<)([\s\\]*[!/?]?[\s\\]*s[\s\\]*c[\s\\]*r[\s\\]*i[\s\\]*p[\s\\]*t)`，ZWSP 插在 `<` 与首字母之间——归一化后是 `<`+ZWSP+`script…`，ZWSP 存活即放行；
- 分离符集 `[\s\\]` 恰为 CF 归一化剥掉的字符集（空白+反斜杠），与第 3 节实测严格对齐；
- 只对 script 逐字母展开（唯一实测标签签名），其余标签分离形未见真实流量，需要时同法扩词；
- 幂等：ZWSP 不属于 `[\s\\]`，破坏后的形态不再命中；
- `errors.go` 探测表补 `< script`、`<scr ipt`、`<\script`，raw_sig 判别不再漏报此类 403。

**验证**：单测 8 个新用例钉住分离形（含转义分离形 `< s c r i p t` 逐字母、闭合分离形 `</ script>`、不双重插入）；`go vet` + `go test ./...` 全绿（tlsfp 的 TestProxyTunnelRealProxy 为真实代理网络失败，stash 对照确认与本次无关）。

**遗留**：线上端到端回归未做——部署后重放本文档全文（或面板探针对 3405 差异叶子）应翻绿。第 3 节表格的示例文字**保持原样**：它们是实测记录，本文档现在是分离形规则的回归用例（写进本文档的内容自带回归价值）。

### 11.1 二次回归：`< img` 分离形（2026-09-17 18:36，分离形修复部署后）

**案发**：18:36:11-13 同账号（296）、同出口、同 body（103880 字节）4 连败（id 3414-3417）。判别量 `raw_sig=0`、`zwsp=1`——zwsp=1 同时证明新二进制已在跑（script 分离形已被第 11 节规则中和，`< script` 等形态不再裸露），排除「部署未生效」。

**定位**（同款叶子 diff + CF 归一化穷举）：3413（成功，4 秒前同号）与 3414 逐叶 diff 出两个差异叶子：

1. `toolResponses[0].content`——本排查文档新版（已带 ZWSP）；
2. `toolResponses[1].content`——新增叶子，`internal/api/waf.go` 的 Go 源码（9253 字符；CF 归一化后只有 `<=`、`<len(r)` 类比较运算，无标签形态，排除）。

对 3414 原始出站体模拟 CF 归一化（剥空白+反斜杠）后**穷举所有 `<` 后 token**，与 3413 成功对照相比，唯一多出的危险形态是 **`<imgsrc=xonerror…>`**——文档第 3 节表格那行「`< img src=x onerror=alert(1)>`（裸 handler）→ 放行」的示例。`<` 与 `img` 之间的空格让规则 1 不命中（当时规则在 `<` 与标签名之间不容忍任何字符），CF 剥空格后完整还原 `<img…`。与 script 分离形同一漏因，仅标签不同。

**注意**：第 3 节实测「裸 handler 无 script 标签 → 放行」的结论本身没错——错在示例里的 `< img` 分离形：裸 `<img…` 归一化后仍是完整标签开头，与 handler 无关。

**修复**：规则 1 统一加分离符容忍（与第 11 节规则 2 同构，一次覆盖全标签组而非逐标签打补丁）：`(?i)(<|<)([\s\\]*[!/?]?[\s\\]*(?:script|iframe|…|doctype)\b)`。单测补 4 用例（`< img src=x onerror=…>`、转义形、tab 分离 iframe、多行分离 img）；并用真实 3414 失败体做离线回归——修复后规则跑一遍，CF 归一化后危险标签零残留。

**方法论沉淀**：叶子 diff 缩小到「文档 + 源码」两个候选后，靠的是**模拟 CF 归一化 + 穷举 `<` token + 与成功对照做差集**完成终判——比逐个探针快一个量级，第 6 节面板的离线分析值得补这一步（「归一化穷举 vs 成功对照差集」直接给出残留形态清单）。

## 12. 排查流程 API 化（2026-09-17，供外部排查代理自助取数）

第 6/11 节的排查全流程已可通过 API Key 自助调用（开放范围与鉴权见
`docs/superpowers/specs/2026-09-17-ops-api-key-design.md`，端点契约见 openapi.yaml 的 Ops tag）——排查时无需再人工贴 SQL 结果，把 key 与网关地址交给外部排查代理（Claude）即可：

```bash
K='-H "Authorization: Bearer <key>"'   # 或 x-api-key 头
BASE=https://<网关>

# 第一步（第 6 节）：找失败行 + 三个判别量（SQL 免写，直接查）
curl $K "$BASE/api/logs"                              # 最近日志
curl $K "$BASE/api/request-logs?page=1"               # 按会话分组的完整日志

# 第二步：判别（签名计数 / 对照 diff / 体积分桶，一次调用全出）
curl $K "$BASE/api/waf/analyze?log_id=<403行id>"                # baseline_id 可选，缺省三级回退
curl $K "$BASE/api/waf/baselines?log_id=<403行id>"              # 手动挑对照
curl $K "$BASE/api/waf-signatures"                              # 当前签名表

# 自由 SQL（只读：仅 SELECT/WITH/EXPLAIN，200 行上限）——第 6 节任何判别 SQL 都能跑
curl $K -X POST "$BASE/api/sql-query" -d '{"sql":"SELECT ..."}'

# 第三步（第 10 节）：在线探针二分（analyze 的 diff[].path 即探针 paths 入参）
curl $K -X POST "$BASE/api/waf/probe" -d '{"log_id":…,"baseline_id":…,"paths":[…]}'
curl $K "$BASE/api/waf/probe/<job_id>"                 # 2s 轮询
curl $K -X DELETE "$BASE/api/waf/probe/<job_id>"       # 中止
```

排查判读方法论不变（第 6 节三分走 + 第 11 节归一化穷举差集），只是数据获取从「人工贴结果」变成「代理直连」。新增内容签名时，签名探测表（`wafSignatureProbes`）与面板 SQL 预设同源，`/api/waf-signatures` 返回的即最新清单。
