# Web Component 前端迁移设计（并行双前端方案）

日期：2026-09-30
状态：已确认（对话中逐节确认，本文件为落档），阶段一已交付（commit e8da583）

## 目标

将 dashboard 前端从「静态 HTML 片段 + 字符串拼接 JS」迁移为基于 Lit 的 Web Component
组件化前端。采用**新旧并行**策略：旧前端完全不动，新前端放独立目录，通过 `/v2` 入口区分，
逐页迁移，全部迁完后一次性切换 `GET /`。

## 约束

- 无构建步骤：Lit 走 CDN ESM（锁版本 `lit@3`），Tailwind 沿用 Play CDN，Google Fonts 不变
- Go 二进制 `go:embed` 内嵌，无外部文件依赖
- 并行期两套前端共用同一批 `/api/*` 接口与会话 Cookie，后端不新增接口
- 组件用 **Light DOM**（`createRenderRoot() { return this; }`），否则 Tailwind CDN
  的 document 级样式进不了 Shadow DOM，组件内所有 class 失效

## 目录结构

```
internal/web/                     ← 新前端模块（独立 embed）
  ├── web.go                      ← //go:embed all:dist
  └── dist/
      ├── index.html              ← /v2 入口
      ├── css/dashboard.css       ← 复制旧版全局样式（CSS 变量 + 组件类）
      ├── js/
      │   ├── base.js             ← BaseElement（Light DOM）
      │   ├── api.js              ← api() + esc/fmt/fmtMs/pct/ago 等工具（复用旧实现）
      │   ├── toast.js            ← pt-toast（替代 window.showToast）
      │   └── icons.js            ← pt-icon：name → svg path 表
      ├── components/
      │   ├── pt-btn.js           ← 按钮（variant: primary/ghost/gold）
      │   ├── pt-input.js         ← 输入框（v1 未实现，首个表单页迁移时补）
      │   ├── pt-table.js         ← 表格（columns + rows 属性，slot 自定义单元格）
      │   ├── pt-table-paged.js   ← 表格（内置分页，复用 pt-pager）
      │   ├── pt-pager.js         ← 分页条（窗口化页码，迁移旧 pagerHTML 逻辑）
      │   ├── pt-tag.js           ← 状态标签（green/amber/red/blue/gray）
      │   └── pt-card.js          ← 卡片容器
      └── pages/
          ├── page-overview.js    ← 第一阶段交付页
          └── ...（后续逐页补充）
```

## Go 侧改动（最小）

`internal/api/api.go` 路由注册追加两条；`internal/api/ops.go` 追加两个 handler：

- `GET /v2` → `webPage`：复用 `dashboard()` 的登录校验（`loginEnabled()/validSession`），
  读 `web.Files` 的 `dist/index.html`
- `GET /v2/` → `webStatic`：复用 `dashboardStatic` 逻辑，前缀换 `dist/`，
  **必须补 `.js/.mjs → text/javascript` 的 Content-Type 分支**（ES module 无正确 MIME
  会被浏览器拒载；旧 `dashboardStatic` 同样缺此分支，顺手修复）

## 组件契约

- 属性/特性传入数据，`CustomEvent(bubbles: true)` 向外通信（如 pt-pager 发 `page-change`），
  页面组件监听——替代旧版全局 `onclick="gotoPage(n)"` 字符串拼接
- 基类统一提供：Light DOM、`emit(event, detail)` 便捷方法
- 图标统一走 `pt-icon name="..."`（内联 lucide 风格 path 表），不再各页复制 svg

## 数据流

页面组件（如 page-overview）在 `connectedCallback` 调 `api.js` 的
`api('/api/stats')`、`api('/api/analytics?days=14')` 等，结果存组件 `@state`，
渲染用 Lit 模板。图表沿用 Chart.js CDN，在 `updated()` 后挂到 canvas。

## 迁移原则：重实现，非移植

旧前端（dashboard.js 1775 行字符串拼接 + DOM 查询）实现质量差，新前端**不直接复制旧代码**。
每迁移一页的流程：

1. **功能分析先行**：梳理该页现有功能点清单（展示什么数据、哪些交互、调用哪些 API），
   与用户确认——迁移中会**去掉部分功能**、也可能**新增功能**，以确认后的清单为准
2. 按清单用 Lit 组件**重新实现**；旧代码只作行为参考（如字段含义、API 参数、边界情况），
   不搬运其拼接式实现
3. 工具函数（api/esc/fmt 等）同样重新实现，旧版仅参考语义

阶段一交付的 overview 页同样先给出功能点清单确认后再实现。

## 阶段一范围（v1：先跑起来）

v1 只做**组件封装 + 样式统一 + 布局完善**，跑通 `/v2` 全链路，不逐页迁功能：

- 基础组件库：base/toast/icon/btn/input/tag/card/pager/table/table-paged
- 布局组件：topnav + sidebar + 页面容器（页面路由骨架，未迁移的菜单显示占位）
- overview 页以真实数据渲染作为链路验证样板
- 各页功能清单在每个菜单迁移时再逐个分析确认，v1 不锁定

## 迁移顺序

1. **阶段一（本次交付，v1）**：`internal/web/` 骨架 + Go 路由 + 基础组件库 +
   布局（topnav/sidebar/路由骨架）+ overview 真实数据样板页，先跑起来
2. 阶段二起：按流量低→高逐页迁移（vision → modelmap → routing → proxies →
   sql → waf → apikeys → settings → pools → reqlogs → stats → overview 收尾），
   每页先功能分析、按需增删功能
3. 每页迁移 = 新目录补一个 page module + 侧边栏加入口；旧 fragments 对应页不删
4. 全部完成后单独一次切换：`GET /` 改读 `web.Files`，删 `static/`（可回退的独立 commit）

## 明确不做

- 不建主题系统 / 设计 token 体系、不做 SSR、不给组件写单测框架
- 不加 CSP / 缓存头（与旧版保持一致）
- 不做 Shadow DOM 样式隔离（内部面板无此需求）

## 验证方式

阶段一完成后：`go build` 通过；浏览器访问 `/v2`，登录后 overview 页 KPI、
流量趋势图、号池分布图、最近活动均显示真实数据；`/` 旧版不受影响。
