// page-overview.js —— <pt-page> 概览页：KPI 四卡 + 流量趋势折线图 + 号池分布环图 + 最近活动时间线
import { BaseElement } from '../base.js';
import { html } from 'https://cdn.jsdelivr.net/npm/lit@3.3.3/+esm';
import { api, fmt, fmtMs, ago } from '../api.js';
import '../components/pt-card.js';
import '../components/pt-icon.js';

const CHART_COLORS = { ok: '#0B3D2E', bad: '#C2410C', axis: '#8A8F96' };

export class PtPageOverview extends BaseElement {
  static properties = { stats: { state: true }, analytics: { state: true }, accounts: { state: true }, logs: { state: true }, days: { state: true } };
  constructor() {
    super();
    this.stats = {}; this.analytics = {}; this.accounts = []; this.logs = []; this.days = 14;
    this._charts = {};
  }
  connectedCallback() { super.connectedCallback(); this.refresh(); }
  disconnectedCallback() { super.disconnectedCallback(); Object.values(this._charts).forEach((c) => c.destroy()); }
  async refresh() {
    const [stats, analytics, accountsRes, logsRes] = await Promise.all([
      api('/api/stats'),
      api('/api/analytics?days=' + this.days),
      api('/api/accounts'),
      api('/api/logs'),
    ]);
    this.stats = stats || {};
    this.analytics = analytics || {};
    this.accounts = accountsRes.data || accountsRes || [];
    this.logs = (logsRes.data || []).slice(0, 5);
    await this.updateComplete;
    this.drawCharts();
  }
  setDays(n) { this.days = n; this.refresh(); }
  drawCharts() {
    const daily = this.analytics.daily || [];
    const mk = (id) => document.getElementById(id);
    if (mk('v2chartTraffic')) {
      this._charts.traffic?.destroy();
      this._charts.traffic = new Chart(mk('v2chartTraffic'), {
        type: 'line',
        data: {
          labels: daily.map((p) => (p.label || '').slice(5)),
          datasets: [
            { label: '成功', data: daily.map((p) => p.success || 0), borderColor: CHART_COLORS.ok, backgroundColor: 'rgba(11,61,46,0.08)', fill: true, tension: 0.35, pointRadius: 2 },
            { label: '失败', data: daily.map((p) => p.error || 0), borderColor: CHART_COLORS.bad, backgroundColor: 'rgba(194,65,12,0.08)', fill: true, tension: 0.35, pointRadius: 2 },
          ],
        },
        options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { display: false } },
          scales: { x: { ticks: { maxTicksLimit: 8, color: CHART_COLORS.axis } }, y: { beginAtZero: true, ticks: { color: CHART_COLORS.axis } } } },
      });
    }
    const counts = this._poolCounts();
    if (mk('v2chartPool')) {
      this._charts.pool?.destroy();
      this._charts.pool = new Chart(mk('v2chartPool'), {
        type: 'doughnut',
        data: { labels: ['在线', '额度耗尽', '异常', '停用'],
          datasets: [{ data: [counts.active, counts.exhausted, counts.error, counts.disabled],
            backgroundColor: ['#15803D', '#B45309', '#B91C1C', '#8A8F96'] }] },
        options: { responsive: true, maintainAspectRatio: false, cutout: '62%', plugins: { legend: { display: false } } },
      });
    }
  }
  _poolCounts() {
    const c = { active: 0, exhausted: 0, error: 0, disabled: 0 };
    for (const a of this.accounts) {
      if (!a.enabled) c.disabled++;
      else if (a.status === 'active') c.active++;
      else if (a.status === 'exhausted') c.exhausted++;
      else if (a.status === 'error') c.error++;
    }
    return c;
  }
  render() {
    const s = this.stats;
    const successRate = s.totalRequests ? ((s.successRequests / s.totalRequests) * 100).toFixed(2) : '0.00';
    const counts = this._poolCounts();
    const total = (this.analytics.daily || []).reduce((n, p) => n + (p.total || 0), 0);
    return html`
    <div class="p-8">
      <div class="flex items-end justify-between mb-8 gap-6 flex-wrap">
        <div>
          <h1 class="hero-title">Postman2API 网关<br>累计处理 <em>${fmt(s.totalRequests)}</em> 次请求。</h1>
          <p class="mt-3 text-[14px]" style="color: var(--fg-2);">账号池直连 Postman 桌面端，轮询 + 最少在途路由，请求失败自动切换账号。</p>
        </div>
        <div class="flex p-1 rounded-lg" style="background: var(--bg-2);">
          ${[7, 14, 30].map((d) => html`<div class="tab ${this.days === d ? 'active' : ''}" @click=${() => this.setDays(d)}>${d}天</div>`)}
        </div>
      </div>

      <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
        ${[
          { label: '今日请求', value: fmt(s.todayRequests), icon: 'activity' },
          { label: '活跃账号', value: html`${fmt(s.activeAccounts)}<span class="text-[20px]" style="color:var(--muted)">/${fmt(s.totalAccounts)}</span>`, icon: 'users' },
          { label: '平均延迟', value: html`${fmtMs(s.avgLatencyMs)}<span class="text-[20px]" style="color:var(--muted)"> ms</span>`, icon: 'clock' },
          { label: '成功率', value: html`${successRate}<span class="text-[20px]" style="color:var(--muted)">%</span>`, icon: 'check-circle' },
        ].map((k) => html`
          <pt-card hover>
            <div class="flex items-center justify-between mb-4">
              <div class="text-[11px] font-semibold tracking-wider uppercase" style="color:var(--muted)">${k.label}</div>
              <pt-icon name=${k.icon} style="color:var(--accent)"></pt-icon>
            </div>
            <div class="kpi-value">${k.value}</div>
          </pt-card>`)}
      </div>

      <div class="grid grid-cols-1 lg:grid-cols-3 gap-4">
        <pt-card class="lg:col-span-2">
          <div class="flex items-center justify-between mb-4">
            <h3 class="font-display text-[22px] font-medium leading-tight">请求流量趋势</h3>
            <span class="text-[12px] font-mono" style="color:var(--muted)">总计 ${fmt(total)} 次</span>
          </div>
          <div class="h-72"><canvas id="v2chartTraffic"></canvas></div>
        </pt-card>
        <pt-card>
          <h3 class="font-display text-[22px] font-medium leading-tight mb-4">号池分布</h3>
          <div class="h-44 flex items-center justify-center"><canvas id="v2chartPool"></canvas></div>
          <div class="mt-4 space-y-2.5">
            ${[['在线', counts.active, 'var(--accent)'], ['额度耗尽', counts.exhausted, 'var(--warning)'], ['异常', counts.error, 'var(--danger)'], ['停用', counts.disabled, '#8A8F96']].map(([label, n, color]) => html`
              <div class="flex items-center justify-between text-[12.5px]">
                <div class="flex items-center gap-2"><span class="w-2 h-2 rounded-sm" style="background:${color}"></span>${label}</div>
                <span class="font-mono font-semibold">${fmt(n)}</span>
              </div>`)}
          </div>
        </pt-card>
      </div>

      <pt-card class="mt-6">
        <h3 class="font-display text-[18px] font-medium mb-4">最近活动</h3>
        ${this.logs.length ? this.logs.map((l) => html`
          <div class="timeline-item">
            <div class="flex items-start justify-between gap-3">
              <div>
                <div class="text-[13px] font-semibold">${l.status === 'success' ? '请求成功' : '请求失败'}
                  <span class="font-mono" style="color:var(--accent)">${l.model || l.errorMessage || '未知请求'}</span></div>
                <div class="text-[12px] mt-0.5" style="color:var(--fg-2)">账号 #${l.accountId ?? '-'} · ${l.durationMs || 0}ms</div>
              </div>
              <span class="text-[11px] font-mono whitespace-nowrap" style="color:var(--muted)">${ago(l.createdAt)}</span>
            </div>
          </div>`) : html`<div class="py-5" style="color:var(--muted)">暂无活动</div>`}
      </pt-card>
    </div>`;
  }
}
customElements.define('pt-page', PtPageOverview);
