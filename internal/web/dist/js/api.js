// api.js —— 面板 HTTP 与格式化工具。401 时跳登录页。
export async function api(path, options = {}) {
  const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
  const res = await fetch(path, { ...options, headers });
  const text = await res.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
  if (!res.ok) {
    if (res.status === 401) { window.location.href = '/login'; throw new Error('登录已过期'); }
    throw new Error(data.error?.message || data.message || `HTTP ${res.status}`);
  }
  return data;
}

export const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;' }[c]));
export const fmt = (v) => Number(v || 0).toLocaleString('zh-CN');
export const fmtMs = (ms) => {
  const v = Number(ms || 0);
  if (v <= 0) return '-';
  return v >= 1000 ? (v / 1000).toFixed(2) + 's' : Math.round(v) + 'ms';
};
export const pct = (r) => (Number(r || 0) * 100).toFixed(2) + '%';
export function ago(value) {
  if (!value) return '-';
  const ms = Date.now() - new Date(value).getTime();
  if (!isFinite(ms) || ms < 0) return '刚刚';
  if (ms < 60000) return Math.floor(ms / 1000) + ' 秒前';
  if (ms < 3600000) return Math.floor(ms / 60000) + ' 分钟前';
  if (ms < 86400000) return Math.floor(ms / 3600000) + ' 小时前';
  return Math.floor(ms / 86400000) + ' 天前';
}
export function fmtDate(value) {
  if (!value) return '-';
  const d = new Date(value);
  return isFinite(d.getTime())
    ? d.toLocaleString('zh-CN', { month:'2-digit', day:'2-digit', hour:'2-digit', minute:'2-digit', hour12:false })
    : '-';
}
export function countdown(value) {
  if (!value) return '-';
  const ms = new Date(value).getTime() - Date.now();
  if (!isFinite(ms)) return '-';
  if (ms <= 0) return '等待更新';
  const d = Math.floor(ms / 86400000), h = Math.floor(ms % 86400000 / 3600000), m = Math.floor(ms % 3600000 / 60000);
  if (d > 0) return `${d}天 ${h}小时`;
  if (h > 0) return `${h}小时 ${m}分`;
  return Math.max(1, m) + '分钟';
}
