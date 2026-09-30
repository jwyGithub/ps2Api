// pt-toast.js —— 监听 document 上的 'toast' CustomEvent 显示提示，3s 自动消失
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
// 全局便捷入口：toast('已保存')
export function toast(message) { document.dispatchEvent(new CustomEvent('toast', { detail: message })); }
