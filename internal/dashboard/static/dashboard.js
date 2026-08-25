/* Postman2API dashboard adapter. All numbers and actions come from the Go
   service API — no static shells or mock values:
   - /api/stats       真实计数（累计请求/成功率/平均延迟/P95/成本/错误率）
   - /api/accounts    号池管理（真实账号 + source/plan/今日调用）
   - /api/logs        真实请求日志流
   - /api/analytics   日/时序列、模型分布、渠道对比、账号排行、活跃热力图
   - /api/settings    配置项真实读写（重试/故障切换/告警阈值）
   - /api/alerts      真实告警记录（未处理/解决/MTTR）
*/
(function () {
  'use strict';

  var state = {
    stats: {}, accounts: [], logs: [],
    analytics: {}, settings: {}, settingsDefs: [], apiKey: '',
    alerts: [], alertSummary: {}, cacheProbe: {},
    days: 14, page: 'overview', poolQuery: '', poolStatus: 'ALL', alertTab: 'open',
    poolPage: 1, quotaPage: 1
  };
  var PAGE_SIZE = 20;
  // 通用分页：切片当前页并生成页码控件 HTML（gotoFn 为全局翻页函数名）
  function paginate(list, page) {
    var pages = Math.max(1, Math.ceil(list.length / PAGE_SIZE));
    page = Math.min(Math.max(1, page), pages);
    return { items: list.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE), page: page, pages: pages, total: list.length };
  }
  function pagerHTML(p, gotoFn) {
    if (p.pages <= 1) return '';
    var arrow = function (dir, disabled, target) {
      return '<button class="icon-btn"' + (disabled ? ' disabled style="opacity:0.4;"' : ' onclick="' + gotoFn + '(' + target + ')"') + '><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="' + (dir < 0 ? 'm15 18-6-6 6-6' : 'm9 18 6-6-6-6') + '"/></svg></button>';
    };
    var btn = function (i) { return '<button class="icon-btn"' + (i === p.page ? ' style="background: var(--accent); color: white;"' : ' onclick="' + gotoFn + '(' + i + ')"') + '>' + i + '</button>'; };
    var ell = '<button class="icon-btn" disabled style="opacity:0.4;">…</button>';
    // 窗口化页码：首页 … 当前±2 … 末页（仿 element-ui / ant-design-vue）
    var nums = [];
    for (var i = 1; i <= p.pages; i++) {
      if (i === 1 || i === p.pages || (i >= p.page - 2 && i <= p.page + 2)) nums.push(i);
      else if (nums[nums.length - 1] !== '…') nums.push('…');
    }
    var html = arrow(-1, p.page <= 1, p.page - 1);
    for (var j = 0; j < nums.length; j++) html += nums[j] === '…' ? ell : btn(nums[j]);
    return html + arrow(1, p.page >= p.pages, p.page + 1);
  }
  var charts = {};
  var providerModels = ['gpt-5.6-luna','gpt-5.6-sol','gpt-5.6-terra','gpt-5.5','gpt-5.4','gpt-5.2','claude-opus-4-8','claude-sonnet-4-6','claude-haiku-4-5','auto'];

  function loadFragment(path) {
    return fetch('/dashboard/' + path).then(function (r) {
      if (!r.ok) throw new Error('无法加载前端片段：' + path);
      return r.text();
    });
  }

  function bootstrapDashboard() {
    var names = ['fragments/topnav.html', 'fragments/sidebar.html', 'fragments/page-overview.html', 'fragments/page-stats.html', 'fragments/page-pools.html', 'fragments/page-quota.html', 'fragments/page-routing.html', 'fragments/page-alerts.html', 'fragments/page-settings.html', 'fragments/page-proxies.html', 'fragments/drawer.html'];
    return Promise.all(names.map(loadFragment)).then(function (parts) {
      var app = document.getElementById('dashboard-app');
      if (!app) return;
      // parts[0]=topnav, [1]=sidebar, [2..N-2]=页面片段, 末位=drawer（放在 main 外）。
      var drawer = parts[parts.length - 1];
      var pages = parts.slice(2, parts.length - 1).join('\n');
      app.innerHTML = parts[0] + '<div class="pt-16 flex">' + parts[1] + '<main class="ml-60 flex-1 min-h-[calc(100vh-4rem)]">' + pages + '</main></div>' + drawer;
    });
  }

  function key() { return localStorage.getItem('ps2api_api_key') || ''; }
  function esc(value) { return String(value == null ? '' : value).replace(/[&<>"']/g, function (c) { return ({ '&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;' })[c]; }); }
  function fmt(value) { return Number(value || 0).toLocaleString('zh-CN'); }
  function fmtQuota(value) {
    var n = Number(value);
    return isFinite(n) ? n.toLocaleString('zh-CN', { maximumFractionDigits: 1 }) : '-';
  }
  function ago(value) {
    if (!value) return '-';
    var n = Date.now() - new Date(value).getTime();
    if (!isFinite(n) || n < 0) return '刚刚';
    if (n < 60000) return Math.floor(n / 1000) + ' 秒前';
    if (n < 3600000) return Math.floor(n / 60000) + ' 分钟前';
    if (n < 86400000) return Math.floor(n / 3600000) + ' 小时前';
    return Math.floor(n / 86400000) + ' 天前';
  }
  function fmtMs(ms) {
    var v = Number(ms || 0);
    if (v <= 0) return '-';
    if (v >= 1000) return (v / 1000).toFixed(2) + 's';
    return Math.round(v) + 'ms';
  }
  function fmtCost(c) {
    var v = Number(c || 0);
    return '$' + (v >= 1000 ? v.toLocaleString('en-US', { maximumFractionDigits: 2 }) : v.toFixed(4));
  }
  function pct(r) { return (Number(r || 0) * 100).toFixed(2) + '%'; }
  function fmtDate(value) {
    if (!value) return '-';
    var d = new Date(value); if (!isFinite(d.getTime())) return '-';
    return d.toLocaleString('zh-CN', { month:'2-digit', day:'2-digit', hour:'2-digit', minute:'2-digit', hour12:false });
  }
  function countdown(value) {
    if (!value) return '-';
    var ms = new Date(value).getTime() - Date.now(); if (!isFinite(ms)) return '-';
    if (ms <= 0) return '等待更新';
    var days = Math.floor(ms / 86400000), hours = Math.floor(ms % 86400000 / 3600000), minutes = Math.floor(ms % 3600000 / 60000);
    if (days > 0) return days + '天 ' + hours + '小时';
    if (hours > 0) return hours + '小时 ' + minutes + '分';
    return Math.max(1, minutes) + '分钟';
  }
  function api(path, options) {
    options = options || {};
    options.headers = Object.assign({ 'Authorization': 'Bearer ' + key(), 'Content-Type': 'application/json' }, options.headers || {});
    return fetch(path, options).then(function (r) {
      return r.text().then(function (text) {
        var data = {}; try { data = text ? JSON.parse(text) : {}; } catch (_) { data = { raw: text }; }
        if (!r.ok) throw new Error(data.error && data.error.message || data.message || 'HTTP ' + r.status);
        return data;
      });
    });
  }
  function toast(message) { if (typeof window.showToast === 'function') window.showToast(message); else window.alert(message); }
  function setText(selector, value) { var el = document.querySelector(selector); if (el) el.textContent = value; }
  function download(name, content) {
    var a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([content], { type: 'application/json' }));
    a.download = name;
    document.body.appendChild(a); a.click(); a.remove();
  }
  function sourceName(src) {
    return { 'manual': '手动添加', 'local': '本地导入', 'detect-web': '浏览器注册', 'browser': '浏览器登录' }[src] || (src ? src : 'Postman Desktop');
  }
  function todayCalls(accountId) { return (state.analytics.todayCalls || {})[accountId] || 0; }

  window.showToast = function (message) {
    var el = document.getElementById('toast');
    if (!el) { window.alert(message); return; }
    el.textContent = message;
    el.classList.add('show');
    clearTimeout(window.__postmanToastTimer);
    window.__postmanToastTimer = setTimeout(function () { el.classList.remove('show'); }, 2500);
  };
  window.closeDrawer = function () {
    var drawer = document.getElementById('drawer');
    var backdrop = document.getElementById('drawerBackdrop');
    if (drawer) drawer.classList.remove('show');
    if (backdrop) backdrop.classList.remove('show');
  };
  window.switchPage = function (page) {
    state.page = page;
    document.querySelectorAll('.page').forEach(function (el) { el.classList.toggle('active', el.id === 'page-' + page); });
    document.querySelectorAll('.sidebar-item[data-page]').forEach(function (el) { el.classList.toggle('active', el.dataset.page === page); });
    var names = { overview:'概览', stats:'统计分析', pools:'号池 & 额度', routing:'路由策略', proxies:'代理出口', alerts:'告警中心', settings:'系统设置' };
    setText('#crumb', names[page] || page);
    if (page === 'pools') { renderPoolsReal(); renderQuotaReal(); }
    if (page === 'alerts') renderAlertsReal();
    if (page === 'routing') renderRoutingReal();
    if (page === 'proxies') renderProxiesReal();
    if (page === 'settings') renderSettingsReal();
    refreshCurrentPage();
  };

  function statusInfo(status) {
    if (status === 'active') return { dot: 'dot-online', tag: 'tag-green', label: '在线' };
    if (status === 'exhausted') return { dot: 'dot-idle', tag: 'tag-amber', label: '额度耗尽' };
    if (status === 'error') return { dot: 'dot-error', tag: 'tag-red', label: '异常' };
    if (status === 'disabled') return { dot: 'dot-offline', tag: 'tag-gray', label: '已停用' };
    return { dot: 'dot-offline', tag: 'tag-gray', label: status || '停用' };
  }

  // 账号有效状态：异常 → 停用 → 额度耗尽 → 在线。
  // 过滤、汇总卡计数、表格行标签统一走这里，保证三处口径一致。
  function effectiveStatus(a) {
    if (a.status === 'error') return 'error';
    if (!a.enabled) return 'disabled';
    if (a.status === 'exhausted') return 'exhausted';
    return 'active';
  }

  var resourceLoaders = {
    stats: function () { return api('/api/stats').then(function (data) { state.stats = data || {}; }); },
    accounts: function () { return api('/api/accounts').then(function (data) { state.accounts = data.data || []; }); },
    logs: function () { return api('/api/logs').then(function (data) { state.logs = data.data || []; }); },
    analytics: function () { return api('/api/analytics?days=' + state.days).then(function (data) { state.analytics = data || {}; }); },
    settings: function () { return api('/api/settings').then(function (data) {
      state.settings = data.settings || {};
      state.settingsDefs = data.defs || [];
      state.apiKey = data.apiKey || '';
      // 初始化时把服务端存的 API Key 缓存到本地，之后每个请求都带上它。
      if (data.apiKey) localStorage.setItem('ps2api_api_key', data.apiKey);
    }); },
    alerts: function () { return api('/api/alerts').then(function (data) {
      state.alerts = data.data || [];
      state.alertSummary = data.summary || {};
    }); },
    cacheProbe: function () { return api('/api/cache-probe').then(function (data) { state.cacheProbe = data || {}; }); }
  };

  function loadResources(names) {
    return Promise.all(names.map(function (name) { return resourceLoaders[name](); })).then(function () {
      renderAll();
    }).catch(function (err) { toast('加载数据失败：' + err.message); });
  }

  function loadAll() {
    return loadResources(['stats', 'accounts', 'logs', 'analytics', 'settings', 'alerts', 'cacheProbe']);
  }

  function refreshCurrentPage() {
    var pages = {
      overview: ['stats', 'accounts', 'logs', 'analytics', 'alerts'],
      stats: ['stats', 'analytics', 'cacheProbe'],
      pools: ['accounts', 'analytics'],
      quota: ['accounts', 'analytics', 'settings', 'alerts'],
      routing: ['settings'],
      proxies: ['settings'],
      alerts: ['alerts'],
      settings: ['settings']
    };
    var resources = pages[state.page] || ['stats'];
    return resources.length ? loadResources(resources) : Promise.resolve();
  }

  function renderAll() {
    renderRealData(); renderStatsReal(); renderChartsReal(); renderTopAccounts();
    renderPoolsReal(); renderQuotaReal(); renderAlertsReal();
    renderRoutingReal(); renderSettingsReal(); renderProxiesReal(); renderOverviewActivity(); renderSidebarBadges();
    renderCacheProbeReal();
  }

  function renderSidebarBadges() {
    var pb = document.querySelector('.sidebar-item[data-page="pools"] .badge');
    if (pb) pb.textContent = state.accounts.length;
    var ab = document.querySelector('.sidebar-item[data-page="alerts"] .badge');
    if (ab) ab.textContent = state.alertSummary.open || 0;
    var nd = document.querySelector('.notif-dot');
    if (nd) nd.style.display = (state.alertSummary.open || 0) > 0 ? '' : 'none';
  }

  function renderRealData() {
    var s = state.stats;
    setText('body .hero-title em', fmt(s.totalRequests));
    var kpis = document.querySelectorAll('#page-overview .kpi-value');
    if (kpis[0]) kpis[0].textContent = fmt(s.todayRequests); // 今日请求（真实当日计数）
    if (kpis[1]) kpis[1].innerHTML = (s.activeAccounts || 0) + '<span class="text-[20px]" style="color:var(--muted)">/' + (s.totalAccounts || 0) + '</span>';
    if (kpis[2]) kpis[2].innerHTML = fmtMs(s.avgLatencyMs) + '<span class="text-[20px]" style="color:var(--muted)"> ' + (s.avgLatencyMs >= 1000 ? '' : 'ms') + '</span>';
    if (kpis[3]) kpis[3].innerHTML = (s.totalRequests ? ((s.successRequests / s.totalRequests) * 100).toFixed(2) : '0.00') + '<span class="text-[20px]" style="color:var(--muted)">%</span>';
    setText('#kpiErrors', (s.errorRequests || 0) + ' 次失败');
    var usage = document.getElementById('sidebarUsage');
    if (usage) usage.innerHTML = '<div class="flex items-center justify-between mb-3"><div class="text-[11px] font-semibold tracking-wider uppercase" style="color: var(--muted);">累计调用</div><span class="text-[11px] font-mono" style="color: var(--accent);">'+fmt(s.totalRequests)+'</span></div><div class="progress mb-3"><div class="progress-fill" style="width: 100%; background: linear-gradient(90deg, var(--accent), var(--accent-2));"></div></div><div class="flex items-baseline gap-1.5 mb-1"><span class="font-display text-[22px] font-medium leading-none">'+fmt(s.totalRequests)+'</span><span class="text-[11px]" style="color: var(--muted);">次请求</span></div><div class="text-[11px]" style="color: var(--muted);">成功 '+fmt(s.successRequests)+' · 失败 '+fmt(s.errorRequests)+' · Token '+fmt(s.totalTokens)+'</div>';
    setText('#sysHost', window.location.host);
    setText('#sysAccounts', fmt(s.totalAccounts));
    setText('#sysActive', fmt(s.activeAccounts));
    var tag = document.querySelector('#page-overview .hero-title + *');
    var ok = document.querySelector('#page-overview .tag-green');
    if (ok) ok.textContent = state.accounts.length ? '系统正常' : '等待账号';
  }

  // ─── 号池管理 ───────────────────────────────────────────────
  function renderPoolsReal() {
    var body = document.getElementById('poolsBody'); if (!body) return;
    var list = state.accounts.filter(function (a) {
      var st = effectiveStatus(a);
      if (state.poolStatus !== 'ALL' && st !== state.poolStatus) return false;
      if (state.poolQuery && (a.email + ' ' + (a.source || '')).toLowerCase().indexOf(state.poolQuery.toLowerCase()) < 0) return false;
      return true;
    });
    var pg = paginate(list, state.poolPage); state.poolPage = pg.page;
    body.innerHTML = list.length ? pg.items.map(function (a) {
      var s = statusInfo(effectiveStatus(a)), total = Number(a.quotaLimit || 0), remain = Number(a.quotaRemaining || 0);
      var pct = total > 0 ? Math.max(0, Math.min(100, (remain / total) * 100)) : 0;
      var color = pct < 20 ? 'var(--danger)' : pct < 50 ? 'var(--warning)' : 'var(--accent)';
      return '<tr><td><input type="checkbox"></td><td><div class="flex items-center gap-3"><span class="dot '+s.dot+'"></span><div><div class="font-mono font-semibold">'+esc(a.email)+'</div><div class="text-[11px]" style="color:var(--muted)">ID '+a.id+'</div></div></div></td><td><span class="tag tag-gray">'+esc(sourceName(a.source))+'</span></td><td><span class="tag '+s.tag+'">'+s.label+'</span></td><td>'+esc(a.plan || 'FREE_USER')+'</td><td><div class="w-32"><div class="flex items-center justify-between text-[11px] mb-1"><span class="font-mono">'+fmt(remain)+' / '+fmt(total)+'</span><span class="font-mono" style="color:'+color+'">'+(total ? pct.toFixed(1)+'%' : '-')+'</span></div><div class="progress" style="height:4px"><div class="progress-fill" style="width:'+pct+'%;background:'+color+'"></div></div></div></td><td class="font-mono">'+fmt(todayCalls(a.id))+'</td><td class="text-[12px]" style="color:var(--fg-2)">'+fmtDate(a.quotaCycleEnd)+'</td><td><div class="flex items-center gap-1"><button class="btn btn-ghost" style="height:28px;padding:4px 8px;font-size:11px" onclick="testAccount('+a.id+')">测试</button><button class="btn btn-ghost" style="height:28px;padding:4px 8px;font-size:11px" onclick="refreshAccountQuota('+a.id+')">刷新额度</button><button class="btn btn-ghost" style="height:28px;padding:4px 8px;font-size:11px" onclick="toggleAccount('+a.id+','+(!a.enabled)+')">'+(a.enabled?'停用':'启用')+'</button><button class="btn btn-ghost" style="height:28px;padding:4px 8px;font-size:11px;color:var(--danger)" onclick="deleteAccount('+a.id+')">删除</button></div></td></tr>';
    }).join('') : '<tr><td colspan="9" style="text-align:center;padding:40px;color:var(--muted)">'+(state.accounts.length ? '没有匹配的账号' : '暂无账号，请点击"添加账号"或通过"导入"上传 account.json')+'</td></tr>';
    var counts = { active:0, exhausted:0, error:0, disabled:0 };
    state.accounts.forEach(function (a) { var st = effectiveStatus(a); if (counts[st] !== undefined) counts[st]++; });
    var rings = document.querySelectorAll('#page-pools .ring-stat .value');
    if (rings[0]) rings[0].textContent = counts.active; if (rings[1]) rings[1].textContent = counts.exhausted; if (rings[2]) rings[2].textContent = counts.error; if (rings[3]) rings[3].textContent = counts.disabled;
    var poolTotal = state.accounts.length;
    ['active', 'exhausted', 'error', 'disabled'].forEach(function (k) {
      var el = document.querySelector('#page-pools [data-pool-big="' + k + '"]');
      if (el) el.textContent = poolTotal ? Math.round(counts[k] / poolTotal * 100) + '%' : '0%';
    });
    var cnt = document.getElementById('poolCount');
    if (cnt) cnt.textContent = '共 ' + pg.total + ' 条' + (pg.pages > 1 ? ' · 第 ' + pg.page + '/' + pg.pages + ' 页' : '');
    var pager = document.getElementById('poolPager');
    if (pager) pager.innerHTML = pagerHTML(pg, 'poolGoto');
  }
  window.poolGoto = function (n) { state.poolPage = n; renderPoolsReal(); };

  // ─── 额度管理（真实账号额度 + 配置规则读写）─────────────────
  function quotaObserved(account) {
    return !!(account.quotaState || account.quotaCycleStart || account.quotaCycleEnd || Number(account.rateLimit || 0));
  }
  function renderQuotaReal() {
    var observed = state.accounts.filter(quotaObserved), tracked = observed.filter(function (a) { return Number(a.quotaLimit || 0) > 0; });
    var total = tracked.reduce(function (n,a) { return n + Number(a.quotaLimit || 0); }, 0);
    var remain = tracked.reduce(function (n,a) { return n + Number(a.quotaRemaining || 0); }, 0);
    var used = tracked.reduce(function (n,a) { var v = Number(a.quotaUsed); return n + (isFinite(v) && v > 0 ? v : Math.max(0, Number(a.quotaLimit || 0) - Number(a.quotaRemaining || 0))); }, 0);
    var overage = tracked.reduce(function (n,a) { return n + Number(a.quotaOverage || 0); }, 0);
    var usedPct = total ? Math.min(100, used / total * 100) : 0;
    setText('#quotaTotal', fmt(total)); setText('#quotaUsed', fmt(used)); setText('#quotaRemaining', fmt(remain));
    setText('#quotaOverage', fmt(overage)); setText('#quotaTracked', observed.length + ' / ' + state.accounts.length);
    setText('#quotaProgressText', fmt(used)); setText('#quotaProgressTotal', fmt(total));
    var bar = document.getElementById('quotaProgressBar'); if (bar) bar.style.width = usedPct.toFixed(1) + '%';
    var near = document.getElementById('quotaNear');
    if (near) near.textContent = usedPct >= 90 ? '已用 ' + usedPct.toFixed(1) + '% · 注意' : usedPct >= 70 ? '已用 ' + usedPct.toFixed(1) + '%' : '已用 ' + usedPct.toFixed(1) + '% · 充足';
    var cycleEnds = tracked.map(function(a){return a.quotaCycleEnd;}).filter(Boolean).sort(function(a,b){return new Date(a)-new Date(b);});
    var reset = document.getElementById('quotaReset');
    if (reset) reset.textContent = observed.length < state.accounts.length ? (state.accounts.length - observed.length) + ' 个账号未采集，点击“刷新额度”' : cycleEnds.length ? '最近重置 ' + countdown(cycleEnds[0]) + '后' : '等待额度周期数据';
    var thresholds = [];
    tracked.forEach(function(a){ (a.quotaWarningThresholds || []).forEach(function(t){ var label = Number(t.value) + (String(t.unit).toLowerCase() === 'percentage' ? '%' : ' ' + t.unit); if (thresholds.indexOf(label) < 0) thresholds.push(label); }); });
    setText('#quotaOfficialThresholds', thresholds.length ? thresholds.join(' / ') : '-');
    setText('#quotaOveragePolicy', tracked.length ? tracked.filter(function(a){return a.quotaAllowOverage;}).length + ' 个允许 / ' + tracked.filter(function(a){return !a.quotaAllowOverage;}).length + ' 个禁止' : '-');
    setText('#quotaPoolMode', tracked.length ? tracked.filter(function(a){return a.quotaTeamPooled;}).length + ' 个共享 / ' + tracked.filter(function(a){return !a.quotaTeamPooled;}).length + ' 个独立' : '-');
    var rated = tracked.filter(function(a){return Number(a.rateLimit || 0) > 0;});
    setText('#quotaRateSummary', rated.length ? '最低 ' + Math.min.apply(null, rated.map(function(a){return Number(a.rateRemaining || 0);})) + ' / ' + rated[0].rateLimit + ' · ' + (rated[0].rateWindowSeconds || 60) + '秒' : '-');
    var latest = tracked.map(function(a){return a.updatedAt;}).filter(Boolean).sort().pop(); setText('#quotaSnapshotAt', latest ? '最近更新 ' + fmtDate(latest) : '-');
    var rules = document.getElementById('quotaRules');
    if (rules) {
      var th = state.settings['alert_quota'] || '0.2';
      var quotaAlerts = state.alerts.filter(function (a) { return a.alertType === 'low_quota' || a.alertType === 'quota_exhausted'; });
      var thPct = (Number(th) * 100).toFixed(0);
      var rows = [
        { name: '额度不足告警', cond: '剩余额度低于总配额 ' + thPct + '%', notify: '面板告警中心', status: '已启用', recent: quotaAlerts.filter(function(a){return a.alertType==='low_quota';})[0] },
        { name: '额度耗尽告警', cond: 'Postman 返回 QUOTA_EXCEEDED / usageState=EXCEEDED', notify: '面板告警中心', status: '已启用', recent: quotaAlerts.filter(function(a){return a.alertType==='quota_exhausted';})[0] }
      ];
      rules.innerHTML = rows.map(function (row) {
        return '<tr><td class="font-semibold">'+row.name+'</td><td>'+row.cond+'</td><td>'+row.notify+'</td><td><span class="tag tag-green">'+row.status+'</span></td><td class="font-mono text-[12px]">'+(row.recent ? ago(row.recent.createdAt) : '—')+'</td><td><span class="text-[12px]" style="color:var(--muted)">阈值可在系统设置修改</span></td></tr>';
      }).join('');
    }
  }

  // ─── 统计分析页 ─────────────────────────────────────────────
  function renderCacheProbeReal() {
    var c = state.cacheProbe || {};
    var panel = document.getElementById('cacheProbePanel'); if (!panel) return;
    var on = !!c.enabled;
    setText('#cpHitRate', c.potentialHitRate != null ? (c.potentialHitRate * 100).toFixed(2) + '%' : '-');
    setText('#cpHits', fmt(c.potentialHits));
    setText('#cpCacheable', fmt(c.cacheableRequests));
    setText('#cpDistinct', fmt(c.distinctRequests));
    setText('#cpSingleflight', fmt(c.singleflightSaved));
    var tag = document.getElementById('cpStatusTag');
    if (tag) { tag.textContent = on ? '采集中' : '未开启'; tag.className = 'tag ' + (on ? 'tag-green' : 'tag-gray'); }
    var btn = document.getElementById('cpToggleBtn');
    if (btn) btn.textContent = on ? '停止探针' : '开启探针';
    var total = Number(c.cacheableRequests || 0);
    setText('#cpDetail', !on
      ? '探针未开启。开启后跑几天真实流量，命中率明显 >0 才值得建响应缓存。'
      : total === 0
        ? '已开启，尚无可缓存请求样本——等团队发起单发无状态请求后开始累计。'
        : '基于 ' + fmt(total) + ' 条可缓存请求：命中率 ' + (Number(c.potentialHitRate || 0) * 100).toFixed(2) + '% 决定响应缓存价值，并发去重可省 ' + fmt(c.singleflightSaved) + ' 次。');
    setText('#cpMeta', on ? 'shadow · 只度量不改返回' : 'cache_probe_enabled=false');
  }

  function renderStatsReal() {
    var s = state.stats;
    var values = document.querySelectorAll('#page-stats .font-display');
    if (values[0]) values[0].textContent = fmt(s.totalRequests);
    if (values[1]) values[1].textContent = fmt(s.totalTokens);
    if (values[2]) values[2].textContent = fmtCost(s.estimatedCost);
    if (values[3]) values[3].textContent = fmtMs(s.p95LatencyMs);
    if (values[4]) values[4].textContent = pct(s.errorRate);
    setText('#statCostNote', '按模型单价估算');
    setText('#statP95Note', '来自真实请求日志');
    setText('#statErrNote', (s.totalRequests ? (s.successRequests / s.totalRequests * 100).toFixed(2) : '0') + '% 成功');
    setText('#statTodayQuota', fmt(s.todayRequests));
    renderQuotaForecast();
  }

  function renderQuotaForecast() {
    var f = state.analytics.quotaForecast || {};
    var status = document.getElementById('quotaForecastStatus');
    var panel = document.getElementById('quotaForecastPanel');
    if (!status || !panel) return;
    var statusMap = {
      sufficient: { label: '预计够用', tag: 'tag-green' },
      refill: { label: '需要补号', tag: 'tag-red' },
      insufficient_data: { label: '数据不足', tag: 'tag-gray' }
    };
    var meta = statusMap[f.status] || statusMap.insufficient_data;
    status.className = 'tag ' + meta.tag;
    status.textContent = meta.label;
    setText('#quotaForecastDaily', f.observedAccounts ? fmtQuota(f.dailyConsumption) : '-');
    setText('#quotaForecastRemaining', f.observedAccounts ? fmtQuota(f.currentRemaining) : '-');
    setText('#quotaForecastAtEnd', f.observedAccounts ? fmtQuota(f.forecastRemaining) : '-');
    setText('#quotaForecastShortfall', f.observedAccounts ? (f.shortfall > 0 ? fmtQuota(f.shortfall) : '0') : '-');
    setText('#quotaForecastAccounts', f.observedAccounts ? (f.suggestedAccounts || '0') + ' 个' : '-');
    var atEnd = document.getElementById('quotaForecastAtEnd');
    var shortfall = document.getElementById('quotaForecastShortfall');
    if (atEnd) atEnd.style.color = f.forecastRemaining < 0 ? 'var(--danger)' : 'var(--success)';
    if (shortfall) shortfall.style.color = f.shortfall > 0 ? 'var(--danger)' : 'var(--success)';
    var detail;
    if (!f.observedAccounts) {
      detail = '暂无可用额度快照，请先到“额度管理”点击“刷新额度”。';
    } else if (f.needsRefill) {
      detail = '按当前日均消耗，月底前预计还需 ' + fmtQuota(f.forecastAdditional) + '，当前余额不足。';
    } else {
      detail = '按当前日均消耗，月底预计仍余 ' + fmtQuota(Math.max(0, f.forecastRemaining)) + '。';
    }
    setText('#quotaForecastDetail', detail);
    setText('#quotaForecastMeta', f.observedAccounts ? f.month + ' · 已采集 ' + f.observedAccounts + '/' + f.totalAccounts + ' 个账号 · 预计覆盖 ' + (f.coverageDays ? f.coverageDays.toFixed(1) + ' 天' : '-') : '-');
  }

  function renderTopAccounts() {
    var top = document.getElementById('topAccountsBody');
    if (!top) return;
    var list = state.analytics.topAccounts || [];
    top.innerHTML = list.map(function (a, i) {
      return '<tr><td>'+ (i + 1) +'</td><td class="font-mono">'+esc(a.email)+'</td><td><span class="tag tag-gray">'+esc(sourceName(a.source))+'</span></td><td class="font-mono">'+fmt(a.calls)+'</td><td class="font-mono">'+fmt(a.tokens)+'</td><td class="font-mono">'+fmtMs(a.avgLatencyMs)+'</td><td><span class="font-mono" style="color:'+(a.successRate >= 0.9 ? 'var(--success)' : a.successRate >= 0.7 ? 'var(--warning)' : 'var(--danger)')+'">'+pct(a.successRate)+'</span></td><td><span class="font-mono font-semibold" style="color:var(--accent)">'+Number(a.score || 0).toFixed(1)+'</span></td></tr>';
    }).join('') || '<tr><td colspan="8" style="text-align:center;padding:24px;color:var(--muted)">暂无账号数据</td></tr>';
  }

  function renderOverviewActivity() {
    var timeline = document.querySelector('#page-overview .timeline-item') && document.querySelector('#page-overview .timeline-item').parentElement;
    if (!timeline) return;
    timeline.innerHTML = state.logs.slice(0, 5).map(function (l) {
      var label = l.status === 'success' ? '请求成功' : '请求失败';
      var detail = l.model || l.errorMessage || '未知请求';
      return '<div class="timeline-item"><div class="flex items-start justify-between gap-3"><div><div class="text-[13px] font-semibold">'+label+' <span class="font-mono" style="color:var(--accent)">'+esc(detail)+'</span></div><div class="text-[12px] mt-0.5" style="color:var(--fg-2)">账号 #'+(l.accountId || '-')+' · '+(l.totalTokens || 0)+' tokens · '+(l.durationMs || 0)+'ms</div></div><span class="text-[11px] font-mono whitespace-nowrap" style="color:var(--muted)">'+ago(l.createdAt)+'</span></div></div>';
    }).join('') || '<div style="padding:20px;color:var(--muted)">暂无活动</div>';
  }

  // ─── 告警中心（真实告警记录）───────────────────────────────
  function renderAlertsReal() {
    var body = document.getElementById('alertsBody'); if (!body) return;
    var sum = state.alertSummary || {};
    var k = document.querySelectorAll('#page-alerts .font-display');
    if (k[0]) k[0].textContent = sum.severe || 0;
    if (k[1]) k[1].textContent = sum.warning || 0;
    if (k[2]) k[2].textContent = sum.info || 0;
    if (k[3]) k[3].textContent = sum.mttrMin ? Math.round(sum.mttrMin) + 'm' : '—';
    var list = state.alerts.filter(function (a) { return state.alertTab === 'all' || a.status === 'open'; });
    body.innerHTML = list.map(function (a) {
      var levelTag = a.level === 'severe' ? 'tag-red' : a.level === 'info' ? 'tag-blue' : 'tag-amber';
      var levelName = a.level === 'severe' ? '严重' : a.level === 'info' ? '信息' : '警告';
      var btn = a.status === 'open' ? '<button class="btn btn-ghost" onclick="resolveAlert('+a.id+')">处理</button>' : '<span class="tag tag-gray">已解决</span>';
      return '<tr><td><span class="tag '+levelTag+'">'+levelName+'</span></td><td><b>'+esc(a.title)+'</b><div class="text-[12px] mt-0.5" style="color:var(--fg-2)">'+esc(a.message)+'</div></td><td class="font-mono text-[12px]">'+(a.sourceType === 'account' && a.sourceId ? 'account #'+a.sourceId : 'system')+'</td><td class="text-[12px]">'+ago(a.createdAt)+'</td><td>'+(a.status === 'open' ? '<span class="tag tag-amber">未处理</span>' : '<span class="tag tag-green">已解决</span>')+'</td><td>'+btn+'</td></tr>';
    }).join('') || '<tr><td colspan="6" style="text-align:center;padding:30px;color:var(--muted)">暂无告警</td></tr>';
    var openTab = document.querySelector('#page-alerts .tab.active');
    var tabs = document.querySelectorAll('#page-alerts .tab');
    if (tabs[0]) tabs[0].classList.toggle('active', state.alertTab === 'open');
    if (tabs[1]) tabs[1].classList.toggle('active', state.alertTab === 'all');
    var resolveAllBtn = document.getElementById('resolveAllBtn');
    if (resolveAllBtn) resolveAllBtn.style.display = (sum.open || 0) > 0 ? '' : 'none';
  }

  // ─── 路由策略（真实配置读写）────────────────────────────────
  function renderRoutingReal() {
    var el = document.getElementById('routingWeights'); if (!el) return;
    var active = state.accounts.filter(function (a) { return a.enabled && a.status === 'active'; }).length;
    var channels = state.analytics.channels || [];
    var maxCalls = Math.max.apply(null, channels.map(function (c) { return c.calls; }).concat([1]));
    el.innerHTML = (channels.length ? channels.map(function (c) {
      var w = c.calls ? (c.calls / maxCalls * 100) : 0;
      return '<div class="flex items-center justify-between"><span>'+esc(c.channel)+'</span><span class="font-mono">'+fmt(c.calls)+' 次调用 · '+pct(c.successRate)+'</span></div><div class="progress"><div class="progress-fill" style="width:'+w+'%;background:var(--accent)"></div></div>';
    }).join('') : '<div class="text-[12px]" style="color:var(--muted)">暂无调用数据</div>') + '<p class="text-[12px] mt-3" style="color:var(--muted)">当前 ' + active + ' 个活跃账号 · 策略：轮询 + 最少在途，失败自动切换（重试 ' + (state.settings['retry_count'] || '3') + ' 次）。</p>';
    var retry = document.getElementById('routingRetry');
    if (retry) retry.value = state.settings['retry_count'] || '3';
    var failover = document.getElementById('routingFailover');
    if (failover) failover.checked = (state.settings['failover_enabled'] === 'false') ? false : true;
    var fvOn = (state.settings['failover_enabled'] === 'false') ? false : true;
    var rc = state.settings['retry_count'] || '3';
    var rTag = document.getElementById('fvRetryTag');
    if (rTag) { rTag.textContent = fvOn ? '重试 ' + rc + ' 次' : '不重试'; rTag.className = 'tag ' + (fvOn ? 'tag-green' : 'tag-gray'); }
    var rTitle = document.getElementById('fvRetryTitle');
    if (rTitle) rTitle.textContent = '请求重试 ' + rc + ' 次';
  }

  // ─── 系统设置（真实读写）────────────────────────────────────
  function renderSettingsReal() {
    var modelBox = document.getElementById('settingsModels');
    if (modelBox) modelBox.innerHTML = providerModels.map(function (m) { return '<span class="tag tag-blue font-mono">'+esc(m)+'</span>'; }).join(' ');
    var host = document.getElementById('settingsHost');
    if (host) host.textContent = window.location.protocol + '//' + window.location.host;
    var host2 = document.getElementById('settingsHost2');
    if (host2) host2.textContent = window.location.protocol + '//' + window.location.host;
    var auth = document.getElementById('settingsApiKey');
    if (auth && document.activeElement !== auth) auth.value = state.apiKey || '';
    var form = document.getElementById('settingsForm');
    if (form) {
      // 代理相关项（group=proxy）已迁移到独立的「代理出口」菜单，通用设置表单不再渲染。
      form.innerHTML = state.settingsDefs.filter(function (d) { return d.group !== 'proxy'; }).map(function (d) {
        var val = state.settings[d.key] != null ? state.settings[d.key] : d.default;
        var input;
        if (d.type === 'bool') {
          input = '<label class="switch"><input type="checkbox" data-key="'+d.key+'" '+(val === 'true' ? 'checked' : '')+'><div class="slider"></div></label>';
        } else if (d.type === 'select') {
          input = '<select class="input font-mono" data-key="'+d.key+'" style="max-width:220px">' + (d.options || []).map(function (o) {
            return '<option value="'+esc(o)+'"'+(String(val) === String(o) ? ' selected' : '')+'>'+esc(o)+'</option>';
          }).join('') + '</select>';
        } else {
          input = '<input class="input font-mono" data-key="'+d.key+'" value="'+esc(val)+'" style="max-width:220px">';
        }
        return '<div class="flex items-center justify-between p-3 rounded-lg" style="background:var(--bg)"><div><div class="text-[13px] font-semibold">'+esc(d.label)+'</div><div class="text-[12px] mt-0.5" style="color:var(--fg-2)">'+esc(d.description)+'</div></div>'+input+'</div>';
      }).join('') + '<div class="pt-2 flex items-center gap-2"><button class="btn btn-primary" onclick="saveSettings()">保存配置</button></div>';
    }
  }

  // ─── 图表（全部真实聚合数据）────────────────────────────────
  function destroyChart(name) { if (charts[name]) { charts[name].destroy(); delete charts[name]; } }
  function drawChart(id, cfg) {
    var c = document.getElementById(id);
    if (!c) return;
    if (charts[id]) charts[id].destroy();
    try { charts[id] = new Chart(c, cfg); } catch (e) { /* canvas 未就绪 */ }
  }

  function renderChartsReal() {
    if (typeof Chart === 'undefined') return;
    renderTrafficChart();
    renderPoolChart();
    renderHourlyChart();
    renderModelChart();
    renderChannelChart();
    renderHeatmap();
  }

  function renderTrafficChart() {
    var daily = state.analytics.daily || [];
    var labels = daily.map(function (p) { var d = p.label || ''; return d ? d.slice(5) : ''; });
    var total = daily.reduce(function (n, p) { return n + (p.total || 0); }, 0);
    setText('#trafficTotal', fmt(total));
    // 更新范围 tab 激活态
    document.querySelectorAll('#page-overview [data-range]').forEach(function (t) {
      t.classList.toggle('active', t.dataset.range === state.days + 'd');
    });
    drawChart('chartTraffic', { type: 'line', data: {
      labels: labels,
      datasets: [
        { label: '成功请求', data: daily.map(function (p) { return p.success || 0; }), borderColor: '#0B3D2E', backgroundColor: 'rgba(11,61,46,0.08)', fill: true, tension: .35, pointRadius: 2 },
        { label: '失败请求', data: daily.map(function (p) { return p.error || 0; }), borderColor: '#C2410C', backgroundColor: 'rgba(194,65,12,0.08)', fill: true, tension: .35, pointRadius: 2 }
      ]
    }, options: { responsive: true, maintainAspectRatio: false, interaction: { mode: 'index', intersect: false }, plugins: { legend: { display: false } }, scales: { x: { ticks: { maxTicksLimit: 8, color: '#8A8F96' } }, y: { beginAtZero: true, ticks: { color: '#8A8F96' } } } } });
  }

  function renderPoolChart() {
    var active = state.accounts.filter(function (a) { return a.status === 'active' && a.enabled; }).length;
    var exhausted = state.accounts.filter(function (a) { return a.status === 'exhausted'; }).length;
    var error = state.accounts.filter(function (a) { return a.status === 'error'; }).length;
    var disabled = state.accounts.filter(function (a) { return !a.enabled; }).length;
    drawChart('chartPool', { type: 'doughnut', data: { labels: ['在线', '额度耗尽', '异常', '停用'], datasets: [{ data: [active, exhausted, error, disabled], backgroundColor: ['#15803D', '#B45309', '#B91C1C', '#8A8F96'] }] }, options: { responsive: true, maintainAspectRatio: false, cutout: '62%', plugins: { legend: { display: false } } } });
    setText('#poolLegendActive', active); setText('#poolLegendExhausted', exhausted); setText('#poolLegendError', error); setText('#poolLegendDisabled', disabled);
  }

  function renderHourlyChart() {
    var hourly = state.analytics.hourly || [];
    var labels = hourly.map(function (p) { var h = (p.label || '').split(' ')[1] || ''; return h ? h.slice(0, 5) : ''; });
    drawChart('chartHourly', { type: 'bar', data: { labels: labels, datasets: [{ label: '调用量', data: hourly.map(function (p) { return p.total || 0; }), backgroundColor: 'rgba(11,61,46,0.75)', borderRadius: 3 }] }, options: { responsive: true, maintainAspectRatio: false, plugins: { legend: { display: false } }, scales: { x: { ticks: { maxTicksLimit: 12, color: '#8A8F96' } }, y: { beginAtZero: true, ticks: { color: '#8A8F96' } } } } });
  }

  function renderModelChart() {
    var models = state.analytics.models || [];
    var colors = ['#0B3D2E', '#C2410C', '#1D4ED8', '#B45309', '#6D28D9', '#15803D', '#0E7490', '#BE185D', '#4D7C0F', '#52525B'];
    drawChart('chartModel', { type: 'doughnut', data: { labels: models.map(function (m) { return m.model; }), datasets: [{ data: models.map(function (m) { return m.count; }), backgroundColor: colors.slice(0, models.length) }] }, options: { responsive: true, maintainAspectRatio: false, cutout: '55%', plugins: { legend: { position: 'bottom' } } } });
  }

  function renderChannelChart() {
    var channels = state.analytics.channels || [];
    if (!channels.length) { destroyChart('chartRadar'); var c = document.getElementById('chartRadar'); if (c) c.parentElement.innerHTML = '<div class="text-[12px]" style="color:var(--muted);padding:40px 0;text-align:center">暂无调用数据</div>'; return; }
    var maxCalls = Math.max.apply(null, channels.map(function (x) { return x.calls; }).concat([1]));
    var maxTokens = Math.max.apply(null, channels.map(function (x) { return x.tokens; }).concat([1]));
    var maxCost = Math.max.apply(null, channels.map(function (x) { return x.cost; }).concat([1]));
    var maxLat = Math.max.apply(null, channels.map(function (x) { return x.avgLatencyMs; }).concat([1]));
    drawChart('chartRadar', { type: 'radar', data: { labels: ['调用量', '成功率', '低延迟', 'Token', '成本'], datasets: channels.map(function (c, i) {
      return { label: c.channel, data: [
        c.calls / maxCalls,
        c.successRate || 0,
        maxLat ? 1 - (c.avgLatencyMs / maxLat) : 0,
        c.tokens / maxTokens,
        maxCost ? 1 - (c.cost / maxCost) : 0
      ], borderColor: ['#0B3D2E', '#C2410C', '#1D4ED8'][i % 3], backgroundColor: ['rgba(11,61,46,0.12)', 'rgba(194,65,12,0.12)', 'rgba(29,78,216,0.12)'][i % 3], pointRadius: 2 };
    }) }, options: { responsive: true, maintainAspectRatio: false, scales: { r: { beginAtZero: true, max: 1, ticks: { display: false }, grid: { color: 'rgba(138,143,150,0.2)' } } }, plugins: { legend: { position: 'bottom' } } } });
  }

  function renderHeatmap() {
    var hm = document.getElementById('heatmap');
    if (!hm) return;
    var cells = state.analytics.heatmap || [];
    if (!cells.length) { hm.innerHTML = '<div class="text-[12px]" style="color:var(--muted);padding:20px 0;text-align:center">暂无热力分布</div>'; return; }
    var max = 1;
    cells.forEach(function (c) { if (c.count > max) max = c.count; });
    var days = ['日', '一', '二', '三', '四', '五', '六'];
    var byWd = {};
    cells.forEach(function (c) { (byWd[c.weekday] = byWd[c.weekday] || {})[c.hour] = c.count; });
    var html = '<div class="flex gap-1 items-center"><div class="w-4 shrink-0"></div>';
    for (var h = 0; h < 24; h++) html += '<div class="text-[9px] font-mono" style="width:14px;color:var(--muted);text-align:center">' + h + '</div>';
    html += '</div>';
    for (var w = 0; w < 7; w++) {
      html += '<div class="flex gap-1 items-center"><div class="w-4 shrink-0 text-[10px]" style="color:var(--muted)">' + days[w] + '</div>';
      for (var h2 = 0; h2 < 24; h2++) {
        var cnt = (byWd[w] && byWd[w][h2]) || 0;
        var level = cnt === 0 ? 0 : Math.min(5, Math.ceil(cnt / max * 5));
        html += '<div class="heat-cell heat-' + level + '" title="' + days[w] + ' ' + String(h2).padStart(2, '0') + ':00 · ' + cnt + ' 次"></div>';
      }
      html += '</div>';
    }
    hm.innerHTML = html;
  }

  // ─── 操作（全部真实写后端）──────────────────────────────────
  window.toggleAccount = function (id, enabled) { api('/api/accounts/'+id, {method:'PATCH',body:JSON.stringify({enabled:enabled})}).then(function(){toast('账号状态已更新');return loadAll();}).catch(function(e){toast(e.message);}); };
  window.deleteAccount = function (id) { if (!confirm('确定删除这个账号？')) return; api('/api/accounts/'+id,{method:'DELETE'}).then(function(){toast('账号已删除');return loadAll();}).catch(function(e){toast(e.message);}); };
  window.exportAccounts = function () {
    if (!confirm('导出文件包含账号密码和登录凭据，确定继续？')) return;
    fetch('/api/accounts/export', { headers: { 'Authorization': 'Bearer ' + key() } }).then(function (r) {
      if (!r.ok) return r.json().then(function (d) { throw new Error(d.error && d.error.message || '导出失败'); });
      return r.text();
    }).then(function (content) { download('account.json', content); toast('账号已导出'); }).catch(function (e) { toast(e.message); });
  };
  window.importAccountsFile = function (input) {
    var files = input.files ? Array.prototype.slice.call(input.files) : [];
    if (!files.length) return;
    var imported = 0, refreshing = 0, failed = [];
    // ponytail: 逐个文件顺序导入，每个文件都是一份 account.json；单个失败不影响其余文件。
    var chain = files.reduce(function (p, file) {
      return p.then(function () {
        return file.text().then(function (content) {
          return api('/api/accounts/import', { method: 'POST', body: content });
        }).then(function (d) { imported += d.imported; refreshing += (d.refreshing || 0); }).catch(function (e) {
          failed.push(file.name + ': ' + e.message);
        });
      });
    }, Promise.resolve());
    chain.then(function () {
      if (imported) toast('已导入 ' + imported + ' 个账号' + (refreshing ? '，正在后台刷新额度…' : ''));
      if (failed.length) toast('部分文件导入失败：' + failed.join('；'));
      return loadAll();
    }).then(function () {
      // 后端导入后异步探测额度并写库；此处有限次轮询列表，让表格额度随探测陆续到位而更新。
      // 上限约 5 次 / 每 2s，避免无限刷新；无启用账号需刷新时直接跳过。
      if (!refreshing) return;
      var tries = 0, max = 5;
      var timer = setInterval(function () {
        tries++;
        loadAll();
        if (tries >= max) { clearInterval(timer); toast('额度刷新完成'); }
      }, 2000);
    }).finally(function () { input.value = ''; });
  };
  window.submitAccount = function () {
    var f=document.getElementById('drawer');
    var inputs=f.querySelectorAll('input');
    var email=inputs[0]&&inputs[0].value, token=inputs[1]&&inputs[1].value, workspace=inputs[2]&&inputs[2].value, subdomain=inputs[3]&&inputs[3].value;
    if(!email||!token||!workspace){toast('请填写邮箱、access_token 和 workspace_id');return;}
    var t={access_token:token,user_id:'dashboard',workspace_id:workspace};
    if(subdomain)t.workspace_subdomain=subdomain;
    api('/api/accounts',{method:'POST',body:JSON.stringify({email:email,tokens:t})}).then(function(){closeDrawer();toast('账号已加入号池');return loadAll();}).catch(function(e){toast(e.message);});
  };
  window.openDrawer = function () {
    var f=document.getElementById('drawer');if(!f)return;
    var body=f.querySelector('.flex-1');
    if(body&&!body.dataset.real){body.dataset.real='1';body.innerHTML='<div class="space-y-4"><div><label class="text-[12px] font-semibold block mb-1.5">邮箱标识</label><input class="input" placeholder="account@example.com"></div><div><label class="text-[12px] font-semibold block mb-1.5">Postman token（桌面版填 access_token；web 版填 postman.sid）</label><input class="input font-mono" type="password" placeholder="token / postman.sid"></div><div><label class="text-[12px] font-semibold block mb-1.5">workspace_id（= 登录态 teamId）</label><input class="input font-mono" placeholder="workspace UUID"></div><div><label class="text-[12px] font-semibold block mb-1.5">workspace_subdomain（web 版必填，如 abc123；桌面版可留空）</label><input class="input font-mono" placeholder="如 abc123"></div><p class="text-[12px]" style="color:var(--muted)">web 版获取：F12 → Application → Cookies 复制 postman.sid；Console 执行 fetch(\'https://god.postman.co/api/users/me\',{credentials:\'include\'}).then(r=>r.json()).then(m=>console.log(m.id, (m.user_organizations||{}).organizations)) 得到 user_id / workspace_id（orgs[0].id）/ subdomain（m.username 小写）。token 只写入服务端 SQLite，不会回显到面板。</p></div>';}
    f.classList.add('show');document.getElementById('drawerBackdrop').classList.add('show');
  };
  window.resolveAlert = function (id) { api('/api/alerts/'+id+'/resolve', {method:'POST',body:'{}'}).then(function(){toast('告警已处理');return loadAll();}).catch(function(e){toast(e.message);}); };
  window.resolveAllAlerts = function () { if (!confirm('确定处理全部未处理告警？')) return; api('/api/alerts/resolve-all', {method:'POST',body:'{}'}).then(function(){toast('全部告警已处理');return loadAll();}).catch(function(e){toast(e.message);}); };
  window.saveApiKey = function () {
    var el = document.getElementById('settingsApiKey');
    var val = el ? el.value.trim() : '';
    // 先写本地再 PUT：改 key 后本次 PUT 请求就带新 key，避免改完立刻 401。
    localStorage.setItem('ps2api_api_key', val);
    api('/api/settings', {method:'PUT', body:JSON.stringify({apiKey:val})}).then(function(){
      toast(val ? 'API Key 已保存并生效' : 'API Key 已清空（鉴权已关闭）');
      return loadAll();
    }).catch(function(e){toast(e.message);});
  };
  window.saveSettings = function () {
    var payload = {};
    document.querySelectorAll('#settingsForm [data-key]').forEach(function (el) {
      payload[el.dataset.key] = el.type === 'checkbox' ? (el.checked ? 'true' : 'false') : el.value;
    });
    api('/api/settings', {method:'PUT', body:JSON.stringify({settings:payload})}).then(function(){toast('配置已保存并生效');return loadAll();}).catch(function(e){toast(e.message);});
  };
  // ─── 代理出口（独立页面）──────────────────────────────────────
  // parseProxyList 把配置串按换行/逗号/分号/空白拆成去重列表（与后端 parseProxyURLs 对齐）。
  function parseProxyList(raw) {
    var out = [], seen = {};
    String(raw || '').split(/[\s,;]+/).forEach(function (s) {
      s = s.trim();
      if (s && !seen[s]) { seen[s] = true; out.push(s); }
    });
    return out;
  }
  // proxyResultCells 渲染一行的「状态/延迟/出口IP/地区/运营商」5 个单元格。
  function proxyResultCells(t) {
    var muted = 'style="color:var(--muted)"';
    if (!t) {
      return '<td class="py-2 pr-3" ' + muted + '>未测试</td><td class="py-2 pr-3" ' + muted + '>-</td><td class="py-2 pr-3" ' + muted + '>-</td><td class="py-2 pr-3" ' + muted + '>-</td><td class="py-2 pr-3" ' + muted + '>-</td>';
    }
    if (t.loading) {
      return '<td class="py-2 pr-3" style="color:var(--fg-2)">测试中…</td><td class="py-2 pr-3">-</td><td class="py-2 pr-3">-</td><td class="py-2 pr-3">-</td><td class="py-2 pr-3">-</td>';
    }
    var r = t.result || {};
    var statusHtml = r.ok ? '<span class="tag tag-green">连通' + (r.status ? ' · ' + r.status : '') + '</span>' : '<span class="tag tag-red">失败</span>';
    var errNote = (!r.ok && r.error) ? '<div class="text-[11px] mt-0.5" style="color:var(--err,#dc2626)">' + esc(r.error) + '</div>' : '';
    var geoNote = (r.ok && !r.egressIp && r.geoError) ? '<div class="text-[11px] mt-0.5" ' + muted + '>出口信息不可用</div>' : '';
    var region = [r.countryCode, r.region, r.city].filter(Boolean).join(' · ') || '-';
    return '<td class="py-2 pr-3">' + statusHtml + errNote + '</td>' +
           '<td class="py-2 pr-3 font-mono">' + esc(r.ok ? fmtMs(r.latencyMs) : '-') + '</td>' +
           '<td class="py-2 pr-3 font-mono">' + esc(r.egressIp || '-') + geoNote + '</td>' +
           '<td class="py-2 pr-3">' + esc(region) + '</td>' +
           '<td class="py-2 pr-3" style="max-width:180px;word-break:break-all">' + esc(r.org || '-') + '</td>';
  }
  function renderProxiesReal() {
    var settings = state.settings || {};
    var toggle = document.getElementById('proxyEnabledToggle');
    if (toggle) toggle.checked = settings['proxy_enabled'] === 'true';
    var fallbackToggle = document.getElementById('proxyFallbackToggle');
    if (fallbackToggle) fallbackToggle.checked = settings['proxy_fallback_direct'] === 'true';
    var body = document.getElementById('proxyListRows');
    if (!body) return;
    var list = parseProxyList(settings['proxy_urls']);
    var empty = document.getElementById('proxyListEmpty');
    var cnt = document.getElementById('proxyRowsCount');
    if (cnt) cnt.textContent = list.length ? ('共 ' + list.length + ' 个出口') : '';
    if (empty) empty.style.display = list.length ? 'none' : 'block';
    state.proxyTests = state.proxyTests || {};
    body.innerHTML = list.map(function (url, i) {
      return '<tr style="border-bottom:1px solid var(--border)">' +
        '<td class="py-2 pr-3 font-mono text-[12px]" style="max-width:260px;word-break:break-all">' + esc(url) + '</td>' +
        proxyResultCells(state.proxyTests[url]) +
        '<td class="py-2 pr-3 text-right" style="white-space:nowrap">' +
          '<button class="btn" style="padding:4px 12px;font-size:12px" onclick="testProxy(' + i + ')">测试</button> ' +
          '<button class="btn btn-ghost" style="padding:4px 12px;font-size:12px" onclick="removeProxy(' + i + ')">删除</button>' +
        '</td>' +
      '</tr>';
    }).join('');
  }
  // ─── 添加出口抽屉 ─────────────────────────────────────────────
  function proxyDrawerValues() {
    var type = (document.getElementById('proxyType') || {}).value || 'socks5';
    var host = ((document.getElementById('proxyHost') || {}).value || '').trim();
    var port = ((document.getElementById('proxyPort') || {}).value || '').trim();
    var user = ((document.getElementById('proxyUser') || {}).value || '').trim();
    var pass = ((document.getElementById('proxyPass') || {}).value || '').trim();
    return { type: type, host: host, port: port, user: user, pass: pass };
  }
  // buildProxyURL 由表单字段拼出标准代理串，如 socks5://user:pass@host:port。
  function buildProxyURL(v) {
    var auth = v.user ? (encodeURIComponent(v.user) + (v.pass ? ':' + encodeURIComponent(v.pass) : '') + '@') : '';
    return v.type + '://' + auth + v.host + (v.port ? ':' + v.port : '');
  }
  function updateProxyPreview() {
    var el = document.getElementById('proxyPreview');
    if (el) {
      var v = proxyDrawerValues();
      el.textContent = buildProxyURL({ type: v.type, host: v.host || 'host', port: v.port || 'port', user: v.user, pass: v.pass ? '***' : '' });
    }
    // 表单一改动，之前的测试结果就作废
    var res = document.getElementById('proxyDraftResult');
    if (res) { res.style.display = 'none'; res.innerHTML = ''; }
  }
  // validatedProxyURL 校验抽屉表单并返回代理串；不合法时 toast 并返回 ''。
  function validatedProxyURL() {
    var v = proxyDrawerValues();
    if (!v.host) { toast('请填写主机'); return ''; }
    if (!v.port || !/^\d+$/.test(v.port) || +v.port < 1 || +v.port > 65535) { toast('端口需为 1-65535 的数字'); return ''; }
    if (v.pass && !v.user) { toast('填了密码就需要填用户名'); return ''; }
    return buildProxyURL(v);
  }
  // renderDraftResult 把一次测试结果渲染到抽屉内的结果区。
  function renderDraftResult(state2) {
    var box = document.getElementById('proxyDraftResult');
    if (!box) return;
    box.style.display = 'block';
    if (state2.loading) { box.innerHTML = '<span style="color:var(--fg-2)">测试中…</span>'; return; }
    var r = state2.result || {};
    if (r.ok) {
      var region = [r.countryCode, r.region, r.city].filter(Boolean).join(' · ') || '-';
      box.innerHTML = '<span class="tag tag-green">连通' + (r.status ? ' · ' + r.status : '') + '</span>' +
        '<div class="mt-1.5 font-mono">延迟 ' + esc(fmtMs(r.latencyMs)) + ' · 出口 ' + esc(r.egressIp || '-') + '</div>' +
        '<div class="mt-0.5" style="color:var(--fg-2)">' + esc(region) + (r.org ? ' · ' + esc(r.org) : '') + '</div>' +
        (!r.egressIp && r.geoError ? '<div class="mt-0.5" style="color:var(--muted)">出口信息不可用</div>' : '');
    } else {
      box.innerHTML = '<span class="tag tag-red">失败</span>' +
        (r.error ? '<div class="mt-1" style="color:var(--err,#dc2626)">' + esc(r.error) + '</div>' : '');
    }
  }
  window.openProxyDrawer = function () {
    var d = document.getElementById('proxyDrawer'); if (!d) return;
    ['proxyHost', 'proxyPort', 'proxyUser', 'proxyPass'].forEach(function (id) { var e = document.getElementById(id); if (e) e.value = ''; });
    var t = document.getElementById('proxyType'); if (t) t.value = 'socks5';
    ['proxyType', 'proxyHost', 'proxyPort', 'proxyUser', 'proxyPass'].forEach(function (id) {
      var e = document.getElementById(id); if (e) { e.oninput = updateProxyPreview; e.onchange = updateProxyPreview; }
    });
    updateProxyPreview();
    d.classList.add('show');
    document.getElementById('proxyDrawerBackdrop').classList.add('show');
    var h = document.getElementById('proxyHost'); if (h) h.focus();
  };
  window.closeProxyDrawer = function () {
    var d = document.getElementById('proxyDrawer');
    var b = document.getElementById('proxyDrawerBackdrop');
    if (d) d.classList.remove('show');
    if (b) b.classList.remove('show');
  };
  window.submitProxy = function () {
    var url = validatedProxyURL();
    if (!url) return;
    var list = parseProxyList((state.settings || {})['proxy_urls']);
    if (list.indexOf(url) !== -1) { toast('该出口已存在'); return; }
    list.push(url);
    api('/api/settings', { method: 'PUT', body: JSON.stringify({ settings: { proxy_urls: list.join('\n') } }) })
      .then(function () { closeProxyDrawer(); toast('出口已添加并生效'); return loadAll(); })
      .catch(function (e) { toast(e.message); });
  };
  // testProxyDraft 在添加前，用表单当前值实测一次连通性与出口信息。
  window.testProxyDraft = function () {
    var url = validatedProxyURL();
    if (!url) return;
    var btn = document.getElementById('proxyDraftTestBtn');
    if (btn) btn.disabled = true;
    renderDraftResult({ loading: true });
    return api('/api/proxy-test', { method: 'POST', body: JSON.stringify({ url: url }) })
      .then(function (data) { renderDraftResult({ result: data.result || {} }); })
      .catch(function (e) { renderDraftResult({ result: { ok: false, error: e.message } }); })
      .then(function () { if (btn) btn.disabled = false; });
  };
  window.removeProxy = function (i) {
    var list = parseProxyList((state.settings || {})['proxy_urls']);
    var url = list[i];
    if (!url) return;
    if (!confirm('删除出口 ' + url + '？')) return;
    list.splice(i, 1);
    api('/api/settings', { method: 'PUT', body: JSON.stringify({ settings: { proxy_urls: list.join('\n') } }) })
      .then(function () { toast('出口已删除'); if (state.proxyTests) delete state.proxyTests[url]; return loadAll(); })
      .catch(function (e) { toast(e.message); });
  };
  // testProxy 对第 i 个（已保存的）出口做详细测试，返回连通性 + 出口 IP/地区。
  window.testProxy = function (i) {
    var list = parseProxyList((state.settings || {})['proxy_urls']);
    var url = list[i];
    if (!url) return;
    state.proxyTests = state.proxyTests || {};
    state.proxyTests[url] = { loading: true };
    renderProxiesReal();
    return api('/api/proxy-test', { method: 'POST', body: JSON.stringify({ url: url }) })
      .then(function (data) { state.proxyTests[url] = { result: data.result || {} }; renderProxiesReal(); })
      .catch(function (e) { state.proxyTests[url] = { result: { ok: false, error: e.message } }; renderProxiesReal(); });
  };
  window.testAllProxies = function () {
    var list = parseProxyList((state.settings || {})['proxy_urls']);
    if (!list.length) { toast('尚未配置任何出口代理'); return; }
    list.forEach(function (_, i) { testProxy(i); });
  };
  window.toggleProxyEnabled = function (on) {
    api('/api/settings', { method: 'PUT', body: JSON.stringify({ settings: { proxy_enabled: on ? 'true' : 'false' } }) })
      .then(function () { toast(on ? '出口代理已启用' : '出口代理已关闭'); return refreshCurrentPage(); })
      .catch(function (e) { toast(e.message); });
  };
  window.toggleProxyFallbackDirect = function (on) {
    api('/api/settings', { method: 'PUT', body: JSON.stringify({ settings: { proxy_fallback_direct: on ? 'true' : 'false' } }) })
      .then(function () { toast(on ? '代理全挂兜底直连已开启' : '已关闭兜底直连（严格只走代理）'); return refreshCurrentPage(); })
      .catch(function (e) { toast(e.message); });
  };
  window.saveRouting = function () {
    var retry = document.getElementById('routingRetry');
    var failover = document.getElementById('routingFailover');
    var payload = { settings: {} };
    if (retry) payload.settings['retry_count'] = retry.value;
    if (failover) payload.settings['failover_enabled'] = failover.checked ? 'true' : 'false';
    api('/api/settings', {method:'PUT', body:JSON.stringify(payload)}).then(function(){toast('路由策略已保存并生效');return loadAll();}).catch(function(e){toast(e.message);});
  };

  window.loadDashboard = loadAll;
  window.toggleNotif = function () { toast((state.alertSummary.open || 0) ? '有 ' + state.alertSummary.open + ' 条未处理告警' : '暂无未处理告警'); };
  window.toggleCacheProbe = function () {
    var next = state.cacheProbe && state.cacheProbe.enabled ? 'false' : 'true';
    api('/api/settings', {method:'PUT', body:JSON.stringify({settings:{cache_probe_enabled:next}})})
      .then(function(){ toast(next === 'true' ? '缓存探针已开启' : '缓存探针已停止'); return refreshCurrentPage(); })
      .catch(function(e){toast(e.message);});
  };
  window.resetCacheProbe = function () {
    if (!confirm('清空探针数据，开始新的度量窗口？')) return;
    api('/api/cache-probe', {method:'DELETE'})
      .then(function(){ toast('探针窗口已重置'); return refreshCurrentPage(); })
      .catch(function(e){toast(e.message);});
  };
  window.refreshData = function () { loadAll().then(function () { toast('数据已刷新'); }); };
  window.syncStatus = function () { loadAll().then(function () { toast('状态已同步'); }); };
  window.refreshQuota = function () {
    toast('正在探测账号额度…');
    api('/api/refresh-quota', { method: 'POST', body: '{}' }).then(function (d) {
      var msg = '探测完成：' + (d.ok || 0) + ' 个账号额度已刷新' + ((d.failed || 0) > 0 ? '，' + d.failed + ' 个失败' : '');
      loadAll().then(function () { toast(msg); });
    }).catch(function (e) { toast('探测失败：' + e.message); });
  };
  // ─── 账号连通性测试（直连 / 网关，完整现场）────────────────
  window.testAccount = function (id) {
    var acc = state.accounts.filter(function (a) { return a.id === id; })[0];
    var who = acc ? acc.email : ('#' + id);
    var old = document.getElementById('acctTestOverlay'); if (old) old.remove();
    var ov = document.createElement('div');
    ov.id = 'acctTestOverlay';
    ov.style.cssText = 'position:fixed;inset:0;z-index:9999;display:flex;align-items:center;justify-content:center;background:rgba(0,0,0,.55);padding:24px';
    ov.onclick = function (e) { if (e.target === ov) window.closeAccountTest(); };
    ov.innerHTML =
      '<div class="card" style="width:min(900px,96vw);max-height:90vh;display:flex;flex-direction:column;overflow:hidden">' +
        '<div class="flex items-center justify-between p-5" style="border-bottom:1px solid var(--border)">' +
          '<div><div class="text-[11px] uppercase tracking-wider" style="color:var(--muted)">连通性测试</div>' +
          '<h2 class="font-display text-[20px] font-medium">' + esc(who) + ' <span class="text-[12px]" style="color:var(--muted)">ID ' + id + '</span></h2></div>' +
          '<button class="icon-btn" onclick="closeAccountTest()">✕</button>' +
        '</div>' +
        '<div class="p-5" style="border-bottom:1px solid var(--border)">' +
          '<div class="flex items-end gap-2 flex-wrap">' +
            '<div style="min-width:150px">' +
              '<label class="text-[12px] font-semibold block mb-1.5">模型</label>' +
              '<select id="acctTestModel" class="input">' + acctTestModelOptions() + '</select>' +
            '</div>' +
            '<div style="flex:1;min-width:220px">' +
              '<label class="text-[12px] font-semibold block mb-1.5">用户输入（可编辑）</label>' +
              '<input id="acctTestPrompt" class="input" value="如何使用curl发起请求" placeholder="如何使用curl发起请求">' +
            '</div>' +
            '<button class="btn btn-primary" id="acctTestDirect" onclick="runAccountTest(' + id + ',&#39;direct&#39;)">直连测试</button>' +
            '<button class="btn btn-ghost" id="acctTestGateway" onclick="runAccountTest(' + id + ',&#39;gateway&#39;)">代理测试</button>' +
            '<button class="btn btn-ghost" id="acctTestService" onclick="runAccountTest(' + id + ',&#39;service&#39;)">网关测试</button>' +
          '</div>' +
          '<div id="acctTestStatus" class="text-[12px] mt-2" style="color:var(--muted)">选择一种模式发起测试</div>' +
        '</div>' +
        '<div class="flex-1 overflow-y-auto p-5" id="acctTestResult"><div class="text-[13px]" style="color:var(--muted)">直连测试：绕过出口代理，直接请求上游 Postman（针对本账号）。<br>代理测试：走网关真实出站路径（按账号粘性选代理出口；未配置代理即本机直连）。<br>网关测试：回环调用本服务对外端点（claude→/v1/messages，其它→/v1/responses），带面板 API Key 走完整网关链路，端到端验证整条服务。<br>测试会真实消耗极少量额度；完整请求/响应现场会写入服务端测试日志文件，便于排查。</div></div>' +
      '</div>';
    document.body.appendChild(ov);
  };
  window.closeAccountTest = function () { var o = document.getElementById('acctTestOverlay'); if (o) o.remove(); };
  // 测试可选模型（对外名，与后端 PostmanModelMap 对齐）；默认 claude-opus-4-8。
  function acctTestModelOptions() {
    var models = ['claude-opus-4-8', 'claude-opus-4-7', 'claude-opus-4-6', 'claude-opus-4-5', 'claude-sonnet-4-6', 'claude-sonnet-4-5', 'claude-haiku-4-5', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-5.5', 'gpt-5.4', 'gpt-5.2', 'auto'];
    return models.map(function (m) {
      return '<option value="' + m + '"' + (m === 'claude-opus-4-8' ? ' selected' : '') + '>' + esc(m) + '</option>';
    }).join('');
  }
  window.runAccountTest = function (id, mode) {
    var status = document.getElementById('acctTestStatus');
    var out = document.getElementById('acctTestResult');
    var bd = document.getElementById('acctTestDirect'), bg = document.getElementById('acctTestGateway'), bs = document.getElementById('acctTestService');
    // 每次测试开始前，禁用全部按钮并清空上一轮结果。
    if (bd) bd.disabled = true; if (bg) bg.disabled = true; if (bs) bs.disabled = true;
    var label = mode === 'direct' ? '直连' : (mode === 'service' ? '网关' : '代理');
    var promptEl = document.getElementById('acctTestPrompt');
    var prompt = promptEl ? promptEl.value : '';
    var modelEl = document.getElementById('acctTestModel');
    var model = modelEl ? modelEl.value : '';
    if (status) status.textContent = label + '测试中…（SSE 实时输出，最长约 60s）';
    // 初始化结果区骨架：meta（请求现场）待填 + 响应体实时区（逐行追加）+ done（响应头/概览）待填。
    if (out) out.innerHTML =
      '<div id="acctTestMeta"></div>' +
      '<div class="mb-4"><div class="text-[11px] uppercase tracking-wider mb-1" style="color:var(--muted)">响应内容（响应体 · SSE 逐行）</div>' +
      '<pre id="acctTestRespBody" style="white-space:pre-wrap;word-break:break-all;background:var(--bg-2,rgba(127,127,127,.08));border:1px solid var(--border);border-radius:8px;padding:10px;font-size:11.5px;max-height:340px;overflow:auto;margin:0"></pre></div>' +
      '<div id="acctTestDone"></div>';
    var respPre = function () { return document.getElementById('acctTestRespBody'); };

    fetch('/api/accounts/' + id + '/test', {
      method: 'POST',
      headers: { 'Authorization': 'Bearer ' + key(), 'Content-Type': 'application/json' },
      body: JSON.stringify({ mode: mode, model: model, prompt: prompt })
    }).then(function (resp) {
      if (!resp.ok || !resp.body) {
        return resp.text().then(function (t) { throw new Error('HTTP ' + resp.status + ' ' + t); });
      }
      var reader = resp.body.getReader();
      var dec = new TextDecoder();
      var buf = '';
      function pump() {
        return reader.read().then(function (r) {
          if (r.done) {
            if (buf.trim()) { try { handleAcctTestEvent(JSON.parse(buf), label); } catch (e) {} }
            return;
          }
          buf += dec.decode(r.value, { stream: true });
          var idx;
          while ((idx = buf.indexOf('\n')) >= 0) {
            var chunk = buf.slice(0, idx); buf = buf.slice(idx + 1);
            if (chunk.trim()) { try { handleAcctTestEvent(JSON.parse(chunk), label); } catch (e) {} }
          }
          return pump();
        });
      }
      return pump();
    }).catch(function (e) {
      if (status) status.textContent = '❌ 测试请求失败：' + e.message;
    }).finally(function () {
      if (bd) bd.disabled = false; if (bg) bg.disabled = false; if (bs) bs.disabled = false;
    });

    // 处理单条 NDJSON 事件：meta（请求现场）/ line（响应体逐行）/ done（收尾）。
    function handleAcctTestEvent(ev, label) {
      if (!ev || !ev.type) return;
      if (ev.type === 'meta') {
        var m = ev.meta || {};
        var metaEl = document.getElementById('acctTestMeta');
        if (metaEl) metaEl.innerHTML =
          acctTestBlock('请求地址', esc((m.method || 'POST') + ' ' + (m.url || ''))) +
          acctTestBlock('请求头', acctTestHeaderText(m.requestHeaders)) +
          acctTestBlock('请求参数（请求体）', esc(m.requestBody || ''));
      } else if (ev.type === 'line') {
        var pre = respPre();
        if (pre) { pre.textContent += ev.line + '\n'; pre.scrollTop = pre.scrollHeight; }
      } else if (ev.type === 'done') {
        if (ev.error && !ev.result) {
          if (status) status.textContent = '❌ ' + ev.error;
          return;
        }
        var r = ev.result || {};
        if (status) status.textContent = (r.ok ? '✅ ' : '❌ ') + label + '测试完成 · 状态码 ' + (r.status || 0) + ' · ' + (r.durationMs || 0) + ' ms';
        var doneEl = document.getElementById('acctTestDone');
        var overview = '模式: ' + esc(r.mode || '') + '   出口: ' + esc(r.egress || '-') + '   状态码: ' + (r.status || 0) + '   耗时: ' + (r.durationMs || 0) + ' ms' + (r.error ? ('\n错误: ' + esc(r.error)) : '');
        if (doneEl) doneEl.innerHTML =
          acctTestBlock('概览', overview) +
          (r.logFile ? acctTestBlock('测试日志文件（服务端本地）', esc(r.logFile)) : '') +
          acctTestBlock('响应头', acctTestHeaderText(r.responseHeaders));
        // 兜底：若逐行区为空（例如无换行的错误页），用完整响应体补上。
        var pre = respPre();
        if (pre && !pre.textContent && r.responseBody) pre.textContent = r.responseBody;
      }
    }
  };
  function acctTestBlock(title, content) {
    return '<div class="mb-4"><div class="text-[11px] uppercase tracking-wider mb-1" style="color:var(--muted)">' + title + '</div>' +
      '<pre style="white-space:pre-wrap;word-break:break-all;background:var(--bg-2,rgba(127,127,127,.08));border:1px solid var(--border);border-radius:8px;padding:10px;font-size:11.5px;max-height:260px;overflow:auto;margin:0">' + content + '</pre></div>';
  }
  function acctTestHeaderText(h) {
    if (!h) return '(无)';
    var keys = Object.keys(h).sort();
    if (!keys.length) return '(无)';
    return keys.map(function (k) {
      return (h[k] || []).map(function (v) { return esc(k) + ': ' + esc(v); }).join('\n');
    }).join('\n');
  }

  window.refreshAccountQuota = function (id) {
    var acc = state.accounts.filter(function (a) { return a.id === id; })[0];
    var who = acc ? acc.email : ('#' + id);
    toast('正在刷新 ' + who + ' 的额度…');
    api('/api/accounts/' + id + '/refresh-quota', { method: 'POST', body: '{}' }).then(function (d) {
      var r = d.result || {};
      var msg = (d.ok ? who + ' 额度已刷新：剩余 ' + fmt(r.remaining) + ' / ' + fmt(r.limit) : who + ' 刷新失败：' + (r.error || '未获取到额度'));
      loadAll().then(function () { toast(msg); });
    }).catch(function (e) { toast('刷新失败：' + e.message); });
  };
  window.exportQuota = function () {
    download('postman2api-quota-' + new Date().toISOString().slice(0, 10) + '.json', JSON.stringify({ exportedAt: new Date().toISOString(), accounts: state.accounts }, null, 2));
    toast('额度快照已导出');
  };
  window.checkHealth = function () { loadAll().then(function () { toast('主动检查完成'); }); };
  window.exportReport = function () {
    var data = JSON.stringify({ exported: new Date().toISOString(), stats: state.stats, accounts: state.accounts, analytics: state.analytics, alerts: state.alerts }, null, 2);
    download('postman2api-report-' + new Date().toISOString().slice(0, 10) + '.json', data);
    toast('报表已导出（真实数据）');
  };
  window.setTrafficRange = function (days) { state.days = days; loadAll().then(function () { toast('已切换为最近 ' + days + ' 天'); }); };

  // ─── 事件绑定 ───────────────────────────────────────────────
  document.addEventListener('click', function (e) {
    var range = e.target.closest && e.target.closest('[data-range]');
    if (range) { var d = parseInt((range.dataset.range || '14').replace('d', ''), 10); if (d > 0) setTrafficRange(d); return; }
    var alertTab = e.target.closest && e.target.closest('#page-alerts .tab');
    if (alertTab) {
      state.alertTab = (alertTab.textContent || '').indexOf('全部') >= 0 ? 'all' : 'open';
      renderAlertsReal();
      return;
    }
  });
  document.addEventListener('input', function (e) {
    if (e.target && e.target.id === 'poolSearch') { state.poolQuery = e.target.value; state.poolPage = 1; renderPoolsReal(); }
  });
  document.addEventListener('change', function (e) {
    if (e.target && e.target.id === 'poolStatus') { state.poolStatus = e.target.value || 'ALL'; state.poolPage = 1; renderPoolsReal(); }
    if (e.target && e.target.id === 'statsRange') {
      var v = e.target.value;
      var days = v === 'month' ? new Date().getDate() : parseInt(v, 10);
      if (days > 0 && days !== state.days) { state.days = days; loadAll().then(function () { toast('统计范围已更新'); }); }
    }
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { closeDrawer(); closeProxyDrawer(); }
  });

  function startDashboard() {
    bootstrapDashboard().then(function () {
      loadAll();
      setInterval(function () { refreshCurrentPage(); }, 5000);
    }).catch(function (err) {
      var app = document.getElementById('dashboard-app');
      if (app) app.innerHTML = '<div style="padding:32px;color:#B91C1C;font-family:monospace">Dashboard load failed: ' + esc(err.message) + '</div>';
      toast('加载控制台失败：' + err.message);
    });
  }
  if (document.readyState === 'loading') {
    window.addEventListener('DOMContentLoaded', startDashboard);
  } else {
    startDashboard();
  }
}());
