// pt-card.js —— 卡片容器：渲染 .card，padded 控制内边距、hover 控制悬浮样式
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class PtCard extends BaseElement {
  static properties = { padded: { type: Boolean }, hover: { type: Boolean } };
  constructor() { super(); this.padded = true; this.hover = false; }
  updated() { this.captureChildren(this.renderRoot.querySelector(':scope > .card')); }
  render() {
    const pad = this.padded ? ' p-6' : '';
    const hov = this.hover ? ' card-hover' : '';
    return html`<div class="card${pad}${hov}"></div>`;
  }
}
customElements.define('pt-card', PtCard);
