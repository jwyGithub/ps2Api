# 排查端点开放 API Key 设计（2026-09-17）

## 背景与目标

线上 403 排查（403-waf-neutralization.md 第 6/11 节流程）目前依赖面板会话 Cookie，外部排查代理（Claude）只能靠人工贴 SQL 结果。目标：让持 Key 的外部代理自助调用 WAF 检测与数据查询端点，拿数据自己分析。

## 范围（用户确认）

- **端点**：只读排查 + 在线探针（选项 B）
  - `GET /api/waf/analyze`、`GET /api/waf/baselines`、`GET /api/waf-signatures`
  - `POST /api/waf/probe`、`GET /api/waf/probe/{job_id}`、`DELETE /api/waf/probe/{job_id}`
  - `POST /api/sql-query`（store 层 RunReadOnlyQuery 已保证只读：仅 SELECT/WITH/EXPLAIN、写/PRAGMA 拒绝、200 行上限、单元格截断）
  - `GET /api/request-logs`、`GET /api/logs`、`GET /api/stats`
- **Key 粒度**：复用现有 API Key，不加 scope 字段（选项 A）——任何有效 Key 皆可调（同 `/api/accounts*` 先例）。
- **openapi.yaml**：最小补充，只写本次开放的端点（选项 A），新增 tag `Ops`。

## 实现

### 1. auth 分流（internal/api/api.go）

`auth()` 里在 `accountsAPI` 旁加 `opsAPI` 判断（精确路径 + /api/waf/probe/ 前缀），验证顺序与既有先例一致：**先 Key 后会话**。

```go
accountsAPI := strings.HasPrefix(r.URL.Path, "/api/accounts")
opsAPI := opsReadOnlyAPI(r.URL.Path)
if accountsAPI || opsAPI {
    if _, err := s.resolveKey(r); err == nil {
        return true
    }
}
```

`opsReadOnlyAPI(path)` 匹配清单：

- 精确：`/api/waf/analyze`、`/api/waf/baselines`、`/api/waf-signatures`、`/api/waf/probe`、`/api/sql-query`、`/api/request-logs`、`/api/logs`、`/api/stats`
- 前缀：`/api/waf/probe/`（job_id 子路径，GET/DELETE 共用）

引导态继承既有语义：api_keys 表为空时 `resolveKey` 返回 `(nil, nil)` 放行，与 accounts 系一致，零额外代码。

### 2. openapi.yaml

新增 tag `Ops`（运维/排查只读 + WAF 在线探针），info 描述补鉴权说明（排查端点同样支持 Bearer/x-api-key）。按 handler 实际行为写 8 个 path：

| Path | 方法 | 说明 |
|---|---|---|
| /api/waf/analyze | GET | log_id、baseline_id 查询参数；返回签名计数/对照 diff/体积分桶 |
| /api/waf/baselines | GET | log_id；三级回退对照候选 |
| /api/waf-signatures | GET | 签名子串表副本 |
| /api/waf/probe | POST | 发起探针（log_id/baseline_id/paths/account_id/model），409 单飞 |
| /api/waf/probe/{job_id} | GET / DELETE | 进度轮询 / 中止 |
| /api/sql-query | POST | {"sql": "..."}；只读、200 行上限 |
| /api/request-logs | GET | page/pageSize 分组分页 |
| /api/logs、/api/stats | GET | 最近日志、统计 |

错误契约沿用 components/responses 的 InvalidRequest/InternalError + 401。

### 不改的

Key 表结构、面板 UI、探针 job 逻辑、store 层、`/v1/*` 鉴权。

## 测试

`internal/api/apikeys_test.go` 同款模式：

1. 设 ADMIN_PASSWORD + 有效 Key：调 `/api/sql-query`、`/api/waf/analyze`、`GET /api/waf/probe/{id}` → 200/404（非 401 即放行）；
2. 设 ADMIN_PASSWORD + 无 Key：同端点 → 401；
3. 引导态（无 Key 无密码）：200；
4. 无密码 + 无 Key：200（开放引导态不变）；
5. `opsReadOnlyAPI` 单测：清单内/外路径（如 /api/accounts、/api/keys、/api/settings 必须不放行）。
