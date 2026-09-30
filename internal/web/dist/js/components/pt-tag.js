// pt-tag.js —— 标签薄封装：渲染 .tag.tag-{color}
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class PtTag extends BaseElement {
  static properties = { color: { type: String } };
  constructor() { super(); this.color = 'gray'; }
  updated() { this.captureChildren(this.renderRoot.querySelector('span')); }
  render() {
    return html`<span class="tag tag-${this.color}"></span>`;
  }
}
customElements.define('pt-tag', PtTag);
