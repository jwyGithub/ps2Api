// base.js —— 全组件基类：Light DOM（Tailwind CDN 样式可达）+ 统一事件发射。
import { LitElement } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';

export class BaseElement extends LitElement {
  // ponytail: 关闭 Shadow DOM 换取 Tailwind CDN 直接生效，内部面板无需样式隔离
  createRenderRoot() { return this; }
  emit(name, detail) {
    this.dispatchEvent(new CustomEvent(name, { detail, bubbles: true, composed: true }));
  }
  // Light DOM 下 <slot> 不分发宿主子节点：渲染后把宿主自带的文案/元素搬进包装元素
  captureChildren(target) {
    for (const n of [...this.childNodes]) {
      if (n.nodeType === Node.COMMENT_NODE || n === target || target.contains(n)) continue;
      target.appendChild(n);
    }
  }
}
