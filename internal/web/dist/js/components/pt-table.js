// pt-table.js —— 数据表：columns[{key,label,width?,render?}] × rows；paged+pageSize 内置分页（分页条用 pt-pager）
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
