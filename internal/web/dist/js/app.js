// app.js —— <pt-app> 根组件：持有 page 状态，已迁移页面动态加载 page module，其余渲染占位
import { BaseElement } from './base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import './layout/pt-topnav.js';
import './layout/pt-sidebar.js';
import './components/pt-toast.js';

const LABELS = {
  overview: '概览', stats: '统计分析', reqlogs: '请求日志', sql: '数据查询', waf: 'WAF 检测',
  pools: '号池 & 额度', routing: '路由策略', modelmap: '模型映射', proxies: '代理出口',
  vision: '图片识别', apikeys: 'API KEY 管理', settings: '系统设置',
};
// 已迁移到 v2 的页面，Task 4 起逐个加入
const MIGRATED = new Set();

class PtApp extends BaseElement {
  static properties = { page: { state: true } };
  constructor() { super(); this.page = 'overview'; }
  updated(changed) {
    if (changed.has('page') && MIGRATED.has(this.page)) {
      import(`./pages/page-${this.page}.js`).catch(() => {});
    }
  }
  render() {
    return html`
      <pt-topnav label=${LABELS[this.page] ?? this.page}></pt-topnav>
      <div class="pt-16 flex">
        <pt-sidebar active=${this.page} @page-change=${(e) => { this.page = e.detail.page; }}></pt-sidebar>
        <main class="ml-60 flex-1 min-h-[calc(100vh-4rem)]">
          ${MIGRATED.has(this.page)
            ? html`<pt-page .page=${this.page}></pt-page>`
            : html`<div class="p-8"><div class="card p-10 text-center" style="color:var(--muted)">该页面尚未迁移到 v2，请访问 <a class="font-mono underline" href="/">旧版面板</a></div></div>`}
        </main>
      </div>`;
  }
}
customElements.define('pt-app', PtApp);
