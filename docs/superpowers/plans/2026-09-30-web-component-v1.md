# Web Component 前端 v1 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `internal/web/` 建立基于 Lit 的新前端：组件库 + 布局骨架 + overview 真实数据样板页，通过 `/v2` 入口访问，旧前端零改动。

**Architecture:** Go 侧新增 `internal/web` embed 包 + 两个 handler（`/v2` 页面、`/v2/` 静态资源）。前端为无构建 ESM：Lit 3 走 jsDelivr CDN（锁版本），Light DOM 基类让 Tailwind Play CDN 样式生效，页面组件按需 `import()`。

**Tech Stack:** Go 1.26（embed + net/http）、Lit 3、Tailwind Play CDN、Chart.js CDN、原生 ES Modules

## Global Constraints

- 不改 `internal/dashboard/` 下任何旧文件（唯一例外：`ops.go`/`api.go` 属于 `internal/api`，允许追加路由与 handler）
- Lit CDN 锁版本：`https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm`（子模块 import 同源同版本）
- 组件一律 Light DOM：`createRenderRoot() { return this; }`
- 样式变量沿用旧版 `dashboard.css` 的 `:root` CSS 变量（复制到新目录，作为唯一样式来源）
- 组件对外只经 property 传数据、CustomEvent 传事件，禁止全局函数字符串拼接（旧模式）
- 无测试框架：前端验证走浏览器实际访问，Go 侧验证走 `go build ./...` + 手动 HTTP 请求
- 提交信息以 `feat:`/`fix:`/`docs:` 开头

---

### Task 1: Go 侧新模块与路由

**Files:**
- Create: `internal/web/web.go`
- Create: `internal/web/dist/index.html`（占位，Task 3 完善）
- Modify: `internal/api/api.go:127-129`（追加两行路由）
- Modify: `internal/api/ops.go:187-204`（追加 handler；顺手给 dashboardStatic 补 .js MIME）

**Interfaces:**
- Produces: `web.Files`（embed.FS，根为 `dist/`）；HTTP 路由 `GET /v2`（HTML）、`GET /v2/{name...}`（静态资源）

- [ ] **Step 1: 创建 internal/web/web.go**

```go
// web.go —— 新版 Web Component 面板（/v2 入口），与 internal/dashboard 旧版并行。
package web

import "embed"

// Files contains the Lit-based dashboard frontend shipped with the binary.
// Source lives under dist/, served at /v2.
//
//go:embed all:dist
var Files embed.FS
```

- [ ] **Step 2: 创建占位 index.html**

```html
<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Postman2API Dashboard v2</title>
</head>
<body>
  <div id="app">v2 placeholder</div>
</body>
</html>
```

- [ ] **Step 3: 追加 Go handler（internal/api/ops.go 末尾）**

```go
// webPage 提供新版面板入口（/v2）。登录策略与旧版 dashboard() 一致。
func (s *Server) webPage(w http.ResponseWriter, r *http.Request) {
	if loginEnabled() && !validSession(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	data, err := web.Files.ReadFile("dist/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// webStatic 提供新版面板静态资源（/v2/...）。ES module 必须带正确 JS MIME，
// 否则浏览器拒绝以 module 方式加载。
func (s *Server) webStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v2/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := web.Files.ReadFile("dist/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".mjs"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Write(data)
}
```

ops.go 顶部 import 区加 `"ps2api/internal/web"`。

- [ ] **Step 4: 修复旧 dashboardStatic 的 JS MIME（同文件，上游 bug 顺手修）**

在 `dashboardStatic` 的 Content-Type 分支中补：

```go
	} else if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
```

- [ ] **Step 5: 注册路由（internal/api/api.go:128 后）**

```go
	mux.HandleFunc("GET /v2", s.webPage)
	mux.HandleFunc("GET /v2/", s.webStatic)
```

- [ ] **Step 6: 构建验证**

Run: `go build ./...`
Expected: 无输出（编译通过）

- [ ] **Step 7: Commit**

```bash
git add internal/web internal/api
git commit -m "feat: add /v2 entry serving new web component frontend"
```

---

### Task 2: 全局样式与基础工具

**Files:**
- Create: `internal/web/dist/css/dashboard.css`
- Create: `internal/web/dist/js/api.js`
- Create: `internal/web/dist/js/base.js`

**Interfaces:**
- Produces:
  - `css/dashboard.css`：`:root` 变量 + 全部组件类（card/btn/input/tag/dot/progress/sidebar-item/topnav/tab/icon-btn/data-table/timeline-item/kpi-value/hero-title/toast/page 等，从旧版重整）
  - `js/api.js`：`export api(path, options)`、`export fmt/fmtMs/pct/ago/fmtDate/esc/countdown`（纯函数重实现，语义参考旧版）
  - `js/base.js`：`export class BaseElement extends LitElement`（Light DOM + `emit(name, detail)`）

- [ ] **Step 1: 创建 css/dashboard.css**

以旧版 [dashboard.css](../../../internal/dashboard/static/dashboard.css) 为基础重整：保留 `:root` 全部变量、card/btn/input/tag/dot/progress/data-table/timeline-item/hero-title/kpi-value/tab/icon-btn/toast/sidebar-item/topnav/bg-grain/reqlog-pre 类；删除旧版特有且 v2 不用的（glow-orb、grid-bg、mini-bars、ring-stat、gradient-text、drawer*、page 切换类——v2 用组件路由）。字体族声明保留（index.html 继续引 Google Fonts 同一 URL）。

- [ ] **Step 2: 创建 js/api.js（重新实现，非复制）**

```javascript
// api.js —— 面板 HTTP 与格式化工具。401 时跳登录页。
export async function api(path, options = {}) {
  const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
  const res = await fetch(path, { ...options, headers });
  const text = await res.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
  if (!res.ok) {
    if (res.status === 401) { window.location.href = '/login'; throw new Error('登录已过期'); }
    throw new Error(data.error?.message || data.message || `HTTP ${res.status}`);
  }
  return data;
}

export const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;' }[c]));
export const fmt = (v) => Number(v || 0).toLocaleString('zh-CN');
export const fmtMs = (ms) => {
  const v = Number(ms || 0);
  if (v <= 0) return '-';
  return v >= 1000 ? (v / 1000).toFixed(2) + 's' : Math.round(v) + 'ms';
};
export const pct = (r) => (Number(r || 0) * 100).toFixed(2) + '%';
export function ago(value) {
  if (!value) return '-';
  const ms = Date.now() - new Date(value).getTime();
  if (!isFinite(ms) || ms < 0) return '刚刚';
  if (ms < 60000) return Math.floor(ms / 1000) + ' 秒前';
  if (ms < 3600000) return Math.floor(ms / 60000) + ' 分钟前';
  if (ms < 86400000) return Math.floor(ms / 3600000) + ' 小时前';
  return Math.floor(ms / 86400000) + ' 天前';
}
export function fmtDate(value) {
  if (!value) return '-';
  const d = new Date(value);
  return isFinite(d.getTime())
    ? d.toLocaleString('zh-CN', { month:'2-digit', day:'2-digit', hour:'2-digit', minute:'2-digit', hour12:false })
    : '-';
}
export function countdown(value) {
  if (!value) return '-';
  const ms = new Date(value).getTime() - Date.now();
  if (!isFinite(ms)) return '-';
  if (ms <= 0) return '等待更新';
  const d = Math.floor(ms / 86400000), h = Math.floor(ms % 86400000 / 3600000), m = Math.floor(ms % 3600000 / 60000);
  if (d > 0) return `${d}天 ${h}小时`;
  if (h > 0) return `${h}小时 ${m}分`;
  return Math.max(1, m) + '分钟';
}
```

- [ ] **Step 3: 创建 js/base.js**

```javascript
// base.js —— 全组件基类：Light DOM（Tailwind CDN 样式可达）+ 统一事件发射。
import { LitElement } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class BaseElement extends LitElement {
  // ponytail: 关闭 Shadow DOM 换取 Tailwind CDN 直接生效，内部面板无需样式隔离
  createRenderRoot() { return this; }
  emit(name, detail) {
    this.dispatchEvent(new CustomEvent(name, { detail, bubbles: true, composed: true }));
  }
}
```

- [ ] **Step 4: 浏览器验证**

启动服务后访问 `/v2/css/dashboard.css` 与 `/v2/js/api.js` 能返回 200 且 Content-Type 正确。

- [ ] **Step 5: Commit**

```bash
git add internal/web/dist/css internal/web/dist/js
git commit -m "feat: add v2 global styles, api utils and Light DOM base class"
```

---

### Task 3: 入口 HTML 与布局组件

**Files:**
- Modify: `internal/web/dist/index.html`
- Create: `internal/web/dist/js/components/pt-icon.js`
- Create: `internal/web/dist/js/components/pt-toast.js`
- Create: `internal/web/dist/js/layout/pt-topnav.js`
- Create: `internal/web/dist/js/layout/pt-sidebar.js`
- Create: `internal/web/dist/js/app.js`

**Interfaces:**
- Consumes: Task 2 的 `BaseElement`
- Produces:
  - `<pt-icon name>`：icons 表内查 path，渲染 `<svg>`（lucide 风格，24 viewBox，stroke=currentColor）
  - `<pt-toast>`：监听 `document` 上的 `toast` CustomEvent（`detail: string`）显示提示
  - `<pt-topnav>`：固定顶栏，含面包屑 `label` property
  - `<pt-sidebar>`：`pages` property（`[{id,label,icon,group}]`），激活项发 `page-change` 事件
  - `app.js`：`<pt-app>` 根组件，持有 `route` 状态，`import()` 对应 page module，未迁移页面渲染占位

- [ ] **Step 1: pt-icon.js**

```javascript
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

// lucide 风格 path 表（仅收录布局与 overview 所需，后续页面按需追加）
const ICONS = {
  'layout-dashboard': 'M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z',
  'chart-line': 'M3 3v18h18M7 14l4-4 4 4 5-5',
  'file-text': 'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6M16 13H8M16 17H8',
  'database': 'M3 5a9 3 0 1 0 18 0 9 3 0 1 0-18 0M3 5v14a9 3 0 0 0 18 0V5M3 12a9 3 0 0 0 18 0',
  'shield': 'M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10zM9 12l2 2 4-4',
  'boxes': 'M3 7V5a2 2 0 0 1 2-2h2M17 3h2a2 2 0 0 1 2 2v2M21 17v2a2 2 0 0 1-2 2h-2M7 21H5a2 2 0 0 1-2-2v-2',
  'git-branch': 'M6 3a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM6 9v6M18 6a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM15 9v3a2 2 0 0 1-2 2h-1',
  'flask': 'M10 2v7l-5 9a2 2 0 0 0 2 4h10a2 2 0 0 0 2-4l-5-9V2M8.5 2h7',
  'globe': 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z',
  'image': 'M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2zM9 11a2 2 0 1 0 0-4 2 2 0 0 0 0 4zM21 15l-3.1-3.1a2 2 0 0 0-2.8 0L6 21',
  'key': 'M21 2l-2 2m-7.6 7.6a5.5 5.5 0 1 1-7.8 7.8 5.5 5.5 0 0 1 7.8-7.8zm0 0L15.5 7.5m0 0 3 3L22 7l-3-3',
  'settings': 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
  'log-out': 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9',
  'plus': 'M12 5v14M5 12h14',
  'download': 'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3',
  'activity': 'M22 12h-4l-3 9L9 3l-3 9H2',
  'users': 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM22 21v-2a4 4 0 0 0-3-3.9M16 3.1a4 4 0 0 1 0 7.8',
  'clock': 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM12 6v6l4 2',
  'check-circle': 'M22 11.1V12a10 10 0 1 1-5.9-9.1M22 4L12 14l-3-3',
};

export class PtIcon extends BaseElement {
  static properties = { name: { type: String }, size: { type: Number } };
  constructor() { super(); this.size = 18; }
  render() {
    const d = ICONS[this.name];
    if (!d) return html``;
    return html`<svg width=${this.size} height=${this.size} viewBox="0 0 24 24" fill="none"
      stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"
      style="flex-shrink:0"><path d=${d} /></svg>`;
  }
}
customElements.define('pt-icon', PtIcon);
```

- [ ] **Step 2: pt-toast.js**

```javascript
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class PtToast extends BaseElement {
  static properties = { message: { state: true } };
  constructor() {
    super();
    this.message = '';
    this._timer = null;
    this._onToast = (e) => this.show(String(e.detail ?? ''));
  }
  connectedCallback() { super.connectedCallback(); document.addEventListener('toast', this._onToast); }
  disconnectedCallback() { super.disconnectedCallback(); document.removeEventListener('toast', this._onToast); clearTimeout(this._timer); }
  show(message) {
    this.message = message;
    clearTimeout(this._timer);
    this._timer = setTimeout(() => { this.message = ''; }, 3000);
  }
  render() {
    return html`<div class="toast ${this.message ? 'show' : ''}">${this.message}</div>`;
  }
}
customElements.define('pt-toast', PtToast);
// 全局便捷入口：任何地方 dispatchEvent(new CustomEvent('toast', {detail:'...', bubbles:true, composed:true}))
export function toast(message) { document.dispatchEvent(new CustomEvent('toast', { detail: message })); }
```

- [ ] **Step 3: pt-topnav.js**

```javascript
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import './pt-icon.js';

export class PtTopnav extends BaseElement {
  static properties = { label: { type: String } };
  constructor() { super(); this.label = ''; }
  render() {
    return html`
    <nav class="topnav fixed top-0 left-0 right-0 z-40 h-16 flex items-center px-6 gap-6">
      <div class="flex items-center gap-2.5">
        <div class="w-9 h-9 rounded-xl flex items-center justify-center" style="background: linear-gradient(135deg, var(--accent), var(--accent-2));">
          <pt-icon name="activity" size="18" style="color:white"></pt-icon>
        </div>
        <div>
          <div class="font-display font-bold text-[17px] leading-none">Postman2API</div>
          <div class="text-[10px] tracking-[0.15em] uppercase mt-0.5" style="color: var(--muted);">Control Plane v2</div>
        </div>
      </div>
      <div class="h-6 w-px" style="background: var(--border);"></div>
      <div class="flex items-center gap-2 text-[13px]" style="color: var(--fg-2);">
        <span>工作区</span><span style="color: var(--muted);">/</span>
        <span class="font-semibold" style="color: var(--fg);">${this.label}</span>
      </div>
      <div class="flex-1"></div>
      <div class="flex items-center gap-2 text-[12px] font-medium" style="color: var(--fg-2);">
        <span class="dot dot-online dot-pulse"></span>所有系统运行正常
      </div>
      <div class="flex items-center gap-3 pl-3" style="border-left: 1px solid var(--border);">
        <div class="text-right">
          <div class="text-[12.5px] font-semibold leading-tight">本地管理</div>
          <div class="text-[11px] leading-tight" style="color: var(--muted);">本机访问</div>
        </div>
        <div class="w-9 h-9 rounded-full flex items-center justify-center cursor-pointer" style="background: linear-gradient(135deg, var(--gold), var(--gold-2));" title="退出登录" @click=${this.logout}>
          <pt-icon name="log-out" size="15" style="color:white"></pt-icon>
        </div>
      </div>
    </nav>`;
  }
  logout() { fetch('/api/logout', { method: 'POST' }).catch(() => {}).finally(() => { window.location.href = '/login'; }); }
}
customElements.define('pt-topnav', PtTopnav);
```

- [ ] **Step 4: pt-sidebar.js**

```javascript
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import './pt-icon.js';

const PAGES = [
  { group: '监控', items: [
    { id: 'overview', label: '概览', icon: 'layout-dashboard' },
    { id: 'stats', label: '统计分析', icon: 'chart-line' },
    { id: 'reqlogs', label: '请求日志', icon: 'file-text' },
    { id: 'sql', label: '数据查询', icon: 'database' },
    { id: 'waf', label: 'WAF 检测', icon: 'shield' },
  ]},
  { group: '资源', items: [
    { id: 'pools', label: '号池 & 额度', icon: 'boxes' },
    { id: 'routing', label: '路由策略', icon: 'git-branch' },
    { id: 'modelmap', label: '模型映射', icon: 'flask' },
    { id: 'proxies', label: '代理出口', icon: 'globe' },
  ]},
  { group: '系统', items: [
    { id: 'vision', label: '图片识别', icon: 'image' },
    { id: 'apikeys', label: 'API KEY 管理', icon: 'key' },
    { id: 'settings', label: '系统设置', icon: 'settings' },
  ]},
];

export class PtSidebar extends BaseElement {
  static properties = { active: { type: String } };
  constructor() { super(); this.active = 'overview'; }
  select(id) { this.active = id; this.emit('page-change', { page: id }); }
  render() {
    return html`
    <aside class="fixed left-0 top-16 bottom-0 w-60 p-4 overflow-y-auto z-30" style="border-right:1px solid var(--border); background:var(--bg);">
      ${PAGES.map((g) => html`
        <div class="text-[10px] font-semibold tracking-[0.15em] uppercase px-3 mb-2 mt-4" style="color:var(--muted)">${g.group}</div>
        <div class="space-y-1 mb-2">
          ${g.items.map((p) => html`
            <div class="sidebar-item ${this.active === p.id ? 'active' : ''}" @click=${() => this.select(p.id)}>
              <pt-icon name=${p.icon}></pt-icon>${p.label}
            </div>`)}
        </div>`)}
    </aside>`;
  }
}
customElements.define('pt-sidebar', PtSidebar);
```

- [ ] **Step 5: index.html 与 app.js**

index.html：

```html
<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Postman2API Dashboard v2</title>
  <script src="https://cdn.tailwindcss.com"></script>
  <script src="https://cdn.jsdelivr.net/npm/chart.js@4.5.0/dist/chart.umd.min.js"></script>
  <link href="https://fonts.googleapis.com/css2?family=Fraunces:ital,opsz,wght@0,9..144,300;0,9..144,500;0,9..144,700;1,9..144,500&family=Manrope:wght@300;400;500;600;700;800&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
  <link rel="stylesheet" href="/v2/css/dashboard.css">
</head>
<body class="bg-grain">
  <pt-app></pt-app>
  <pt-toast></pt-toast>
  <script type="module" src="/v2/js/app.js"></script>
</body>
</html>
```

app.js：

```javascript
import { BaseElement } from './base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import './layout/pt-topnav.js';
import './layout/pt-sidebar.js';
import './components/pt-toast.js';

class PtApp extends BaseElement {
  static properties = { page: { state: true } };
  constructor() { super(); this.page = 'overview'; }
  async loadPage() {
    try {
      await import(`./pages/page-${this.page}.js`);
    } catch (e) {
      // 页面 module 不存在 = 尚未迁移，渲染占位
    }
    const el = document.querySelector('pt-page');
    if (el) el.setAttribute('active', this.page);
  }
  updated(changed) {
    if (changed.has('page')) this.loadPage();
  }
  render() {
    return html`
      <pt-topnav label=${this.pageLabel}></pt-topnav>
      <div class="pt-16 flex">
        <pt-sidebar active=${this.page} @page-change=${(e) => { this.page = e.detail.page; }}></pt-sidebar>
        <main class="ml-60 flex-1 min-h-[calc(100vh-4rem)]">
          ${['overview'].includes(this.page)
            ? html`<pt-page .page=${this.page}></pt-page>`
            : html`<div class="p-8"><div class="card p-10 text-center" style="color:var(--muted)">该页面尚未迁移到 v2，请访问 <a class="font-mono underline" href="/">旧版面板</a></div></div>`}
        </main>
      </div>`;
  }
}
customElements.define('pt-app', PtApp);
```

（实现时如发现 `pt-page` 的 page→module 映射写法别扭，允许改为直接 `html` 引入已迁移页组件再 switch——以更简单的为准。）

- [ ] **Step 6: 浏览器验证**

访问 `/v2`：顶栏、侧边栏渲染正常，点击菜单高亮切换，未迁移菜单显示占位。

- [ ] **Step 7: Commit**

```bash
git add internal/web/dist
git commit -m "feat: add v2 layout shell with topnav, sidebar and page routing"
```

---

### Task 4: 基础组件库

**Files:**
- Create: `internal/web/dist/js/components/pt-btn.js`
- Create: `internal/web/dist/js/components/pt-tag.js`
- Create: `internal/web/dist/js/components/pt-card.js`
- Create: `internal/web/dist/js/components/pt-pager.js`
- Create: `internal/web/dist/js/components/pt-table.js`

**Interfaces:**
- Consumes: Task 2 `BaseElement`、Task 3 `pt-icon`
- Produces:
  - `<pt-btn variant="primary|ghost|gold" size>`：slot 为文案，点击发 `click`（原生冒泡即可，不额外包装）；实现为 `<button class="btn btn-{variant}">` 的薄封装
  - `<pt-tag color="green|amber|red|blue|gray">`：slot 文案，渲染 `.tag.tag-{color}`
  - `<pt-card padded hover>`：slot 容器，渲染 `.card`
  - `<pt-pager page pages>`：发 `page-change`（detail `{page}`），窗口化页码（当前±2 + 首末页 + 省略号）
  - `<pt-table .columns .rows>`：`columns: [{key,label,width?,render?}]`，`render(value,row)` 返回 Lit 模板自定义单元格；可选 `paged` property 内置分页（`pageSize` property，默认 20），分页条用 pt-pager，发 `page-change`
- 合并定义在各自独立文件，`components/` 目录每组件一个文件

- [ ] **Step 1: pt-btn.js / pt-tag.js / pt-card.js（薄封装三个一起做）**

```javascript
// pt-btn.js
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class PtBtn extends BaseElement {
  static properties = { variant: { type: String } };
  constructor() { super(); this.variant = 'ghost'; }
  render() {
    return html`<button class="btn btn-${this.variant}" part="button"><slot></slot></button>`;
  }
}
customElements.define('pt-btn', PtBtn);
```

```javascript
// pt-tag.js
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class PtTag extends BaseElement {
  static properties = { color: { type: String } };
  constructor() { super(); this.color = 'gray'; }
  render() {
    return html`<span class="tag tag-${this.color}"><slot></slot></span>`;
  }
}
customElements.define('pt-tag', PtTag);
```

```javascript
// pt-card.js
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class PtCard extends BaseElement {
  static properties = { padded: { type: Boolean }, hover: { type: Boolean } };
  constructor() { super(); this.padded = true; this.hover = false; }
  render() {
    const pad = this.padded ? ' p-6' : '';
    const hov = this.hover ? ' card-hover' : '';
    return html`<div class="card${pad}${hov}"><slot></slot></div>`;
  }
}
customElements.define('pt-card', PtCard);
```

- [ ] **Step 2: pt-pager.js**

```javascript
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import './pt-icon.js';

export class PtPager extends BaseElement {
  static properties = { page: { type: Number }, pages: { type: Number } };
  constructor() { super(); this.page = 1; this.pages = 1; }
  get _nums() {
    const cur = Math.min(Math.max(1, this.page), this.pages);
    const nums = [];
    for (let i = 1; i <= this.pages; i++) {
      if (i === 1 || i === this.pages || (i >= cur - 2 && i <= cur + 2)) nums.push(i);
      else if (nums[nums.length - 1] !== '…') nums.push('…');
    }
    return nums;
  }
  go(n) {
    n = Math.min(Math.max(1, n), this.pages);
    if (n === this.page) return;
    this.emit('page-change', { page: n });
  }
  render() {
    if (this.pages <= 1) return html``;
    const arrow = (dir, disabled, target) => html`
      <button class="icon-btn" ?disabled=${disabled} @click=${() => this.go(target)}>
        <pt-icon name=${dir < 0 ? 'chevron-left' : 'chevron-right'} size="14"></pt-icon>
      </button>`;
    return html`
      ${arrow(-1, this.page <= 1, this.page - 1)}
      ${this._nums.map((n) => n === '…'
        ? html`<button class="icon-btn" disabled style="opacity:0.4">…</button>`
        : html`<button class="icon-btn" style=${n === this.page ? 'background:var(--accent);color:white' : ''} @click=${() => this.go(n)}>${n}</button>`)}
      ${arrow(1, this.page >= this.pages, this.page + 1)}`;
  }
}
customElements.define('pt-pager', PtPager);
```

（`chevron-left`/`chevron-right` 需在 Task 3 的 ICONS 表补：`'M15 18l-6-6 6-6'` 与 `'M9 18l6-6-6-6'`。）

- [ ] **Step 3: pt-table.js**

```javascript
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import './pt-pager.js';

export class PtTable extends BaseElement {
  static properties = {
    columns: { type: Array },
    rows: { type: Array },
    paged: { type: Boolean },
    pageSize: { type: Number },
    page: { state: true },
  };
  constructor() {
    super();
    this.columns = []; this.rows = []; this.paged = false; this.pageSize = 20;
    this.page = 1;
  }
  get _view() {
    if (!this.paged) return this.rows;
    const pages = Math.max(1, Math.ceil(this.rows.length / this.pageSize));
    const page = Math.min(Math.max(1, this.page), pages);
    return { items: this.rows.slice((page - 1) * this.pageSize, page * this.pageSize), page, pages };
  }
  render() {
    const v = this._view;
    const items = Array.isArray(v) ? v : v.items;
    return html`
      <table class="data-table">
        <thead><tr>${this.columns.map((c) => html`<th style=${c.width ? `width:${c.width}` : ''}>${c.label}</th>`)}</tr></thead>
        <tbody>
          ${items.map((row) => html`<tr>${this.columns.map((c) =>
            html`<td>${c.render ? c.render(row[c.key], row) : row[c.key]}</td>`)}</tr>`)}
          ${items.length === 0 ? html`<tr><td colspan=${this.columns.length} style="text-align:center;padding:24px;color:var(--muted)">暂无数据</td></tr>` : ''}
        </tbody>
      </table>
      ${this.paged && v.pages > 1
        ? html`<div class="flex items-center justify-end gap-1 p-3" style="border-top:1px solid var(--border)">
            <pt-pager page=${v.page} pages=${v.pages} @page-change=${(e) => { this.page = e.detail.page; }}></pt-pager>
          </div>` : ''}`;
  }
}
customElements.define('pt-table', PtTable);
```

- [ ] **Step 4: 浏览器验证**

临时在 app.js 占位页中放一个 `<pt-table paged .columns .rows>`（10 行假数据、pageSize 3）与 `<pt-btn variant="primary">`，确认分页交互、按钮样式正常后移除假数据。

- [ ] **Step 5: Commit**

```bash
git add internal/web/dist/js/components
git commit -m "feat: add v2 base components (btn, tag, card, pager, table)"
```

---

### Task 5: overview 样板页（真实数据）

**Files:**
- Create: `internal/web/dist/js/pages/page-overview.js`
- Modify: `internal/web/dist/js/app.js`（确认 overview 引入路径正确）

**Interfaces:**
- Consumes: `api/fmt/fmtMs/pct/ago`（Task 2）、`pt-card/pt-tag/pt-table`（Task 4）、Chart.js 全局 `Chart`
- Produces: `<pt-page>` overview 组件：4 KPI 卡 + 流量趋势折线图 + 号池分布环图 + 最近活动时间线，数据全部来自 `/api/stats`、`/api/analytics?days=14`、`/api/accounts`、`/api/logs`

- [ ] **Step 1: 实现 page-overview.js**

```javascript
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import { api, fmt, fmtMs, pct, ago } from '../api.js';
import '../components/pt-card.js';
import '../components/pt-icon.js';

const CHART_COLORS = { ok: '#0B3D2E', bad: '#C2410C', axis: '#8A8F96' };

export class PtPageOverview extends BaseElement {
  static properties = { stats: { state: true }, analytics: { state: true }, accounts: { state: true }, logs: { state: true }, days: { state: true } };
  constructor() {
    super();
    this.stats = {}; this.analytics = {}; this.accounts = []; this.logs = []; this.days = 14;
    this._charts = {};
  }
  connectedCallback() { super.connectedCallback(); this.refresh(); }
  disconnectedCallback() { super.disconnectedCallback(); Object.values(this._charts).forEach((c) => c.destroy()); }
  async refresh() {
    const [stats, analytics, accountsRes, logsRes] = await Promise.all([
      api('/api/stats'),
      api('/api/analytics?days=' + this.days),
      api('/api/accounts'),
      api('/api/logs'),
    ]);
    this.stats = stats || {};
    this.analytics = analytics || {};
    this.accounts = accountsRes.data || accountsRes || [];
    this.logs = (logsRes.data || []).slice(0, 5);
    await this.updateComplete;
    this.drawCharts();
  }
  setDays(n) { this.days = n; this.refresh(); }
  drawCharts() {
    const daily = this.analytics.daily || [];
    const mk = (id) => document.getElementById(id);
    if (mk('v2chartTraffic')) {
      this._charts.traffic?.destroy();
      this._charts.traffic = new Chart(mk('v2chartTraffic'), {
        type: 'line',
        data: {
          labels: daily.map((p) => (p.label || '').slice(5)),
          datasets: [
            { label: '成功', data: daily.map((p) => p.success || 0), borderColor: CHART_COLORS.ok, backgroundColor: 'rgba(11,61,46,0.08)', fill: true, tension: 0.35, pointRadius: 2 },
            { label: '失败', data: daily.map((p) => p.error || 0), borderColor: CHART_COLORS.bad, backgroundColor: 'rgba(194,65,12,0.08)', fill: true, tension: 0.35, pointRadius: 2 },
          ],
        },
        options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { display: false } },
          scales: { x: { ticks: { maxTicksLimit: 8, color: CHART_COLORS.axis } }, y: { beginAtZero: true, ticks: { color: CHART_COLORS.axis } } } },
      });
    }
    const counts = this._poolCounts();
    if (mk('v2chartPool')) {
      this._charts.pool?.destroy();
      this._charts.pool = new Chart(mk('v2chartPool'), {
        type: 'doughnut',
        data: { labels: ['在线', '额度耗尽', '异常', '停用'],
          datasets: [{ data: [counts.active, counts.exhausted, counts.error, counts.disabled],
            backgroundColor: ['#15803D', '#B45309', '#B91C1C', '#8A8F96'] }] },
        options: { responsive: true, maintainAspectRatio: false, cutout: '62%', plugins: { legend: { display: false } } },
      });
    }
  }
  _poolCounts() {
    const c = { active: 0, exhausted: 0, error: 0, disabled: 0 };
    for (const a of this.accounts) {
      if (!a.enabled) c.disabled++;
      else if (a.status === 'active') c.active++;
      else if (a.status === 'exhausted') c.exhausted++;
      else if (a.status === 'error') c.error++;
    }
    return c;
  }
  render() {
    const s = this.stats;
    const successRate = s.totalRequests ? ((s.successRequests / s.totalRequests) * 100).toFixed(2) : '0.00';
    const counts = this._poolCounts();
    const total = (this.analytics.daily || []).reduce((n, p) => n + (p.total || 0), 0);
    return html`
    <div class="p-8">
      <div class="flex items-end justify-between mb-8 gap-6 flex-wrap">
        <div>
          <h1 class="hero-title">Postman2API 网关<br>累计处理 <em>${fmt(s.totalRequests)}</em> 次请求。</h1>
          <p class="mt-3 text-[14px]" style="color: var(--fg-2);">账号池直连 Postman 桌面端，轮询 + 最少在途路由，请求失败自动切换账号。</p>
        </div>
        <div class="flex p-1 rounded-lg" style="background: var(--bg-2);">
          ${[7, 14, 30].map((d) => html`<div class="tab ${this.days === d ? 'active' : ''}" @click=${() => this.setDays(d)}>${d}天</div>`)}
        </div>
      </div>

      <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
        ${[
          { label: '今日请求', value: fmt(s.todayRequests), icon: 'activity' },
          { label: '活跃账号', value: html`${fmt(s.activeAccounts)}<span class="text-[20px]" style="color:var(--muted)">/${fmt(s.totalAccounts)}</span>`, icon: 'users' },
          { label: '平均延迟', value: html`${fmtMs(s.avgLatencyMs)}<span class="text-[20px]" style="color:var(--muted)"> ms</span>`, icon: 'clock' },
          { label: '成功率', value: html`${successRate}<span class="text-[20px]" style="color:var(--muted)">%</span>`, icon: 'check-circle' },
        ].map((k) => html`
          <pt-card hover class="p-5">
            <div class="flex items-center justify-between mb-4">
              <div class="text-[11px] font-semibold tracking-wider uppercase" style="color:var(--muted)">${k.label}</div>
              <pt-icon name=${k.icon} style="color:var(--accent)"></pt-icon>
            </div>
            <div class="kpi-value">${k.value}</div>
          </pt-card>`)}
      </div>

      <div class="grid grid-cols-1 lg:grid-cols-3 gap-4">
        <pt-card class="lg:col-span-2">
          <div class="flex items-center justify-between mb-4">
            <h3 class="font-display text-[22px] font-medium leading-tight">请求流量趋势</h3>
            <span class="text-[12px] font-mono" style="color:var(--muted)">总计 ${fmt(total)} 次</span>
          </div>
          <div class="h-72"><canvas id="v2chartTraffic"></canvas></div>
        </pt-card>
        <pt-card>
          <h3 class="font-display text-[22px] font-medium leading-tight mb-4">号池分布</h3>
          <div class="h-44 flex items-center justify-center"><canvas id="v2chartPool"></canvas></div>
          <div class="mt-4 space-y-2.5">
            ${[['在线', counts.active, 'var(--accent)'], ['额度耗尽', counts.exhausted, 'var(--warning)'], ['异常', counts.error, 'var(--danger)'], ['停用', counts.disabled, '#8A8F96']].map(([label, n, color]) => html`
              <div class="flex items-center justify-between text-[12.5px]">
                <div class="flex items-center gap-2"><span class="w-2 h-2 rounded-sm" style="background:${color}"></span>${label}</div>
                <span class="font-mono font-semibold">${fmt(n)}</span>
              </div>`)}
          </div>
        </pt-card>
      </div>

      <pt-card class="mt-6">
        <h3 class="font-display text-[18px] font-medium mb-4">最近活动</h3>
        ${this.logs.length ? this.logs.map((l) => html`
          <div class="timeline-item">
            <div class="flex items-start justify-between gap-3">
              <div>
                <div class="text-[13px] font-semibold">${l.status === 'success' ? '请求成功' : '请求失败'}
                  <span class="font-mono" style="color:var(--accent)">${l.model || l.errorMessage || '未知请求'}</span></div>
                <div class="text-[12px] mt-0.5" style="color:var(--fg-2)">账号 #${l.accountId ?? '-'} · ${l.durationMs || 0}ms</div>
              </div>
              <span class="text-[11px] font-mono whitespace-nowrap" style="color:var(--muted)">${ago(l.createdAt)}</span>
            </div>
          </div>`) : html`<div class="py-5" style="color:var(--muted)">暂无活动</div>`}
      </pt-card>
    </div>`;
  }
}
customElements.define('pt-page', PtPageOverview);
```

（若 `/api/accounts`、`/api/logs` 响应包裹结构不同，以实际响应为准取 `.data`。）

- [ ] **Step 2: 浏览器验证**

启动 `go run .`（或现有启动方式）后访问 `/v2`：
- KPI 四卡显示真实数值
- 7/14/30 天切换触发趋势图重绘
- 号池分布与最近活动有真实数据（无数据时显示空态而非报错）
- `/` 旧版面板功能不受影响

- [ ] **Step 3: Commit**

```bash
git add internal/web/dist/js/pages internal/web/dist/js/app.js
git commit -m "feat: add v2 overview page with live stats and charts"
```

---

### Task 6: 收尾验证

**Files:** 无新文件

- [ ] **Step 1: go vet + build**

Run: `go vet ./... && go build ./...`
Expected: 均通过

- [ ] **Step 2: 并行验证两套前端**

浏览器同时访问 `/`（旧版）与 `/v2`（新版）：互不影响；`/v2` 登录跳转、会话失效 401 跳 `/login` 均正常。

- [ ] **Step 3: 更新设计文档状态**

`docs/superpowers/specs/2026-09-30-web-component-frontend-design.md` 顶部状态行追加「阶段一已交付（commit <hash>）」。

```bash
git add docs/superpowers/specs
git commit -m "docs: mark web component phase 1 delivered"
```
