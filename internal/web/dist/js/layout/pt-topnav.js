// pt-topnav.js —— 固定顶栏：品牌 + 面包屑 + 状态 + 退出
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
