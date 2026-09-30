// pt-btn.js —— 按钮薄封装：<button class="btn btn-{variant}">，点击走原生 click 冒泡
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class PtBtn extends BaseElement {
  static properties = { variant: { type: String } };
  constructor() { super(); this.variant = 'ghost'; }
  updated() { this.captureChildren(this.renderRoot.querySelector(':scope > .btn')); }
  render() {
    return html`<button class="btn btn-${this.variant}" part="button"></button>`;
  }
}
customElements.define('pt-btn', PtBtn);
