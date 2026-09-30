// pt-pager.js —— 分页条：窗口化页码（当前±2 + 首末页 + 省略号），翻页发 page-change {page}
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
