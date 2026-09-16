# 号池页离线统计 + 重置刷新 设计

日期：2026-09-16

## 需求

号池 & 额度页面：

1. 统计卡片增加「离线」数据（status=offline 的账号数与占比）。
2. 工具栏新增「重置刷新」按钮：点击后对所有 `status == "exhausted"`（额度耗尽）的账号发起额度探测，查证额度周期重置后余量是否恢复；恢复的账号经 `applyUsageState` 自动转回 active 并重新入池。

范围确认：重置刷新只探测 exhausted 账号，不触碰其它状态（active/error/offline/disabled）。

## 现状

- `renderPoolsReal`（internal/dashboard/static/dashboard.js:309-317）已计算 `counts.offline`，但渲染循环只遍历 `['active','exhausted','error','disabled']`，HTML 也无对应卡片。
- `Router.ProbeQuotas`（internal/router/quota_probe.go:32）为增量刷新，明确跳过 exhausted 账号；`RefreshDueQuotas`（internal/router/quota_reset.go:17）只探测 `QuotaCycleEnd` 已过的账号，每日 23:00 定时跑。
- 恢复路径已存在：`probeAccountQuota` → `applyUsageState`，探测到 AVAILABLE 且余量 > 0 时自动 `SetAccountStatus(active)` + `SetAccountEnabled(true)`。

## 改动

### 后端

1. `internal/router/quota_probe.go` 新增 `ProbeExhaustedQuotas(ctx context.Context) []ProbeResult`：
   - `ListAccounts()` 后筛 `Status == "exhausted"`（不看 enabled——exhausted 账号 enabled 保持 true，即便被手动停用，用户点重置刷新的意图就是想恢复它）。
   - 复用 `probeAccountQuota(ctx, acc, false)`，并发度沿用 `probeConcurrency`。
   - 写库、状态翻转全部复用现有逻辑，无新代码路径。

2. `internal/api/accounts.go` 新增 `refreshExhaustedQuota` handler，`internal/api/api.go` 注册 `POST /api/refresh-quota-exhausted`：
   - 返回格式与 `refreshQuota` 一致：`{ok, failed, results}`。

### 前端

1. `internal/dashboard/static/fragments/page-pools.html`：
   - 「异常」卡片后新增「离线」卡片：`dot-offline` + `data-pool-count="offline"` + `data-pool-pct="offline"`。
   - 工具栏（导入/导出旁）新增「重置刷新」按钮，`onclick="resetRefreshQuota()"`。
2. `internal/dashboard/static/dashboard.js`：
   - `renderPoolsReal` 渲染循环数组加入 `'offline'`。
   - 新增 `window.resetRefreshQuota()`：toast 提示 → `POST /api/refresh-quota-exhausted` → toast 结果（N 个已恢复 / M 个仍耗尽）→ `loadAll()`。

## 错误处理

- 探测失败的账号计入 `failed` 并体现在 toast；单账号失败不影响其它账号（并发探测互不阻塞）。
- 无 exhausted 账号时直接返回 `{ok:0, failed:0, results:[]}`，前端 toast「没有额度耗尽的账号」。

## 测试

- `go build ./...` + `go vet ./...` 通过。
- 手动验证：面板号池页显示离线卡片；点重置刷新后 toast 反馈、账号列表刷新、恢复账号状态变在线。
