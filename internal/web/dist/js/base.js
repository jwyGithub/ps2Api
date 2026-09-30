// base.js —— 全组件基类：Light DOM（Tailwind CDN 样式可达）+ 统一事件发射。
import { LitElement } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class BaseElement extends LitElement {
  // ponytail: 关闭 Shadow DOM 换取 Tailwind CDN 直接生效，内部面板无需样式隔离
  createRenderRoot() { return this; }
  emit(name, detail) {
    this.dispatchEvent(new CustomEvent(name, { detail, bubbles: true, composed: true }));
  }
}
