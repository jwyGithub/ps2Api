// pt-icon.js —— lucide 风格图标：ICONS 表内查 path 渲染 svg（24 viewBox, stroke=currentColor）
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

// 仅收录布局与 overview 所需，后续页面按需追加
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
  // pt-pager（Task 4）分页用
  'chevron-left': 'M15 18l-6-6 6-6',
  'chevron-right': 'M9 18l6-6-6-6',
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
