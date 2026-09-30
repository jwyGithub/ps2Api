// pt-sidebar.js —— 左侧导航：分组菜单，激活项发 page-change 事件
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import '../components/pt-icon.js';

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
