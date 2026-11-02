'use strict';

// ---------------------------------------------------------------------------
// helpers

const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => Array.from(el.querySelectorAll(sel));

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ESC[c]);

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
function fmtBytes(n) {
  n = Number(n) || 0;
  if (n <= 0) return '0 B';
  let i = 0;
  while (n >= 1024 && i < UNITS.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n.toFixed(0) : n >= 100 ? n.toFixed(0) : n >= 10 ? n.toFixed(1) : n.toFixed(2)) + ' ' + UNITS[i];
}
const fmtRate = (n) => fmtBytes(n) + '/s';
const fmtPct = (v, d = 1) => (v == null || isNaN(v) ? '-' : Number(v).toFixed(d) + '%');
const pct = (a, b) => (b > 0 ? (a / b) * 100 : 0);
function fmtDur(s) {
  s = Math.max(0, Math.floor(s || 0));
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d} 天 ${h} 时`;
  if (h > 0) return `${h} 时 ${m} 分`;
  return `${m} 分`;
}
// All dates are shown in the server's time zone so that they line up with
// the day / week / month buckets computed by the server.
let DTF = null;
function tzParts(ts) {
  if (!DTF) {
    const o = { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' };
    try { DTF = new Intl.DateTimeFormat('en-GB', { ...o, timeZone: S.cfg?.timezone }); }
    catch { DTF = new Intl.DateTimeFormat('en-GB', o); }
  }
  const p = {};
  for (const x of DTF.formatToParts(new Date(ts * 1000))) p[x.type] = x.value;
  if (p.hour === '24') p.hour = '00';
  return p;
}
function fmtTime(ts, mode = 'full') {
  if (!ts) return '-';
  const p = tzParts(ts);
  const date = `${p.year}-${p.month}-${p.day}`, time = `${p.hour}:${p.minute}`;
  if (mode === 'date') return date;
  if (mode === 'md') return `${p.month}-${p.day}`;
  if (mode === 'time') return time;
  return `${date} ${time}`;
}
function ago(ts) {
  if (!ts) return '从未';
  const s = Math.max(0, Math.floor(Date.now() / 1000 - ts));
  if (s < 60) return `${s} 秒前`;
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
  return `${Math.floor(s / 86400)} 天前`;
}
function shortOS(os) {
  if (!os) return '';
  return os.replace(/GNU\/Linux\s*/i, '').replace(/\s*\(.*?\)\s*/g, ' ').replace(/\s+LTS$/, '').trim();
}

class AuthError extends Error {}

async function api(path, opts = {}) {
  const res = await fetch(path, { credentials: 'same-origin', ...opts });
  if (res.status === 401) throw new AuthError('unauthorized');
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

// ---------------------------------------------------------------------------
// state

const S = {
  cfg: null,
  nodes: [],
  byId: new Map(),
  view: 'nodes',
  filter: '',
  nodeFilter: 'all',
  detail: null,
  range: '24h',
  dPeriod: 'day',
  statsPeriod: 'month',
  statsOffset: 0,
  stats: null,
  statsSort: { key: 'total', asc: false },
  history: null,
  pingHistory: null,
  alerts: [],
  lastOK: 0,
};

// ---------------------------------------------------------------------------
// node list

function bar(p, label, title) {
  p = Math.max(0, Math.min(100, p || 0));
  const cls = p >= 90 ? 'bad' : p >= 75 ? 'warn' : '';
  return `<div class="bar ${cls}"${title ? ` title="${esc(title)}"` : ''}><i style="width:${p.toFixed(1)}%"></i><span>${esc(label ?? fmtPct(p, 0))}</span></div>`;
}

function pingClass(p) {
  if (p.loss == null) return 'idle';
  if (p.loss >= 100) return 'bad';
  if (p.loss >= 10 || (p.latency ?? 0) >= 300) return 'warn';
  return 'ok';
}
function pingChips(n) {
  if (!n.pings || !n.pings.length) return '';
  const chips = n.pings.map((p) => {
    const lat = p.latency == null ? (p.loss >= 100 ? "超时" : "-") : `${p.latency < 10 ? p.latency.toFixed(1) : Math.round(p.latency)}ms`;
    const loss = p.loss == null ? '' : ` · ${p.loss < 10 ? p.loss.toFixed(1) : Math.round(p.loss)}%`;
    return `<span class="chip ${pingClass(p)}" title="${esc(p.host)}"><b>${esc(p.name)}</b> ${lat}${loss}</span>`;
  }).join('');
  return `<div class="pings">${chips}</div>`;
}

function nodeRow(n) {
  const s = n.state, h = n.host || {}, on = n.online, c = n.cycle || {};
  const sub = [n.location, shortOS(h.os), h.virt].filter(Boolean).join(' · ');
  let uptime;
  if (on && s) uptime = fmtDur(s.uptime);
  else uptime = `<span class="sub">${n.last_seen ? '离线 · ' + ago(n.last_seen) : '等待首次上报'}</span>`;
  const live = on && s;
  let traffic = `<span class="down">↓${fmtBytes(c.rx)}</span> <span class="up">↑${fmtBytes(c.tx)}</span>`;
  if (c.limit > 0) {
    const used = pct(c.rx + c.tx, c.limit);
    const lc = used >= 100 ? 'bad' : used >= 85 ? 'warn' : '';
    traffic += `<div class="bar mini ${lc}" title="已用 ${fmtPct(used)} / ${fmtBytes(c.limit)}"><i style="width:${Math.min(100, used).toFixed(1)}%"></i></div>`;
  }
  const cpu = s ? bar(s.cpu, fmtPct(s.cpu, 0), `steal ${fmtPct(s.steal)} · iowait ${fmtPct(s.iowait)}`) : bar(0, '-');
  const mem = s ? bar(pct(s.mem_used, s.mem_total), null, `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)}`) : bar(0, '-');
  const disk = s ? bar(pct(s.disk_used, s.disk_total), null, `${fmtBytes(s.disk_used)} / ${fmtBytes(s.disk_total)}`) : bar(0, '-');
  return `<div class="row node ${on ? '' : 'off'}" data-id="${esc(n.id)}">
    <div class="name"><span class="dot ${on ? 'on' : ''}"></span><div><div class="t">${esc(n.name)}</div><div class="s">${esc(sub)}</div></div></div>
    <div><span class="lbl">在线</span>${uptime}</div>
    <div><span class="lbl">负载</span>${s ? s.load1.toFixed(2) : '-'}</div>
    <div><span class="lbl">网络</span><span class="down">↓${live ? fmtRate(s.rx_rate) : '-'}</span> <span class="up">↑${live ? fmtRate(s.tx_rate) : '-'}</span></div>
    <div><span class="lbl">流量</span>${traffic}</div>
    <div><span class="lbl">CPU</span>${cpu}</div>
    <div><span class="lbl">内存</span>${mem}</div>
    <div><span class="lbl">硬盘</span>${disk}</div>
    ${pingChips(n)}
  </div>`;
}

function matches(n) {
  if (S.nodeFilter === 'online' && !n.online) return false;
  if (S.nodeFilter === 'offline' && n.online) return false;
  if (!S.filter) return true;
  const h = n.host || {};
  return [n.name, n.group, n.location, n.id, h.os, h.hostname, h.virt]
    .some((v) => (v || '').toLowerCase().includes(S.filter));
}

function renderNodes() {
  const list = S.nodes.filter(matches);
  const hasGroups = list.some((n) => n.group);
  let html = '', group = null;
  for (const n of list) {
    if (hasGroups && n.group !== group) {
      group = n.group;
      html += `<div class="group-title">${esc(group || '未分组')}</div>`;
    }
    html += nodeRow(n);
  }
  $('#node-rows').innerHTML = html;
  $('#no-nodes').classList.toggle('hidden', S.nodes.length > 0);
  $('#nodes').classList.toggle('hidden', S.nodes.length === 0);
}

function renderSummary() {
  const on = S.nodes.filter((n) => n.online);
  let rx = 0, tx = 0, crx = 0, ctx = 0;
  for (const n of on) { rx += n.state?.rx_rate || 0; tx += n.state?.tx_rate || 0; }
  for (const n of S.nodes) { crx += n.cycle?.rx || 0; ctx += n.cycle?.tx || 0; }
  const cards = [
    ['在线节点', `${on.length}<small> / ${S.nodes.length}</small>`],
    ['实时下行', `<span class="down">${fmtRate(rx)}</span>`],
    ['实时上行', `<span class="up">${fmtRate(tx)}</span>`],
    ['本周期下行', fmtBytes(crx)],
    ['本周期上行', fmtBytes(ctx)],
    ['活动告警', S.alerts.length ? `<span style="color:var(--bad)">${S.alerts.length}</span>` : '<span style="color:var(--ok)">0</span>'],
  ];
  $('#summary').innerHTML = cards.map(([k, v]) => `<div class="card"><div class="k">${k}</div><div class="v">${v}</div></div>`).join('');
}

function renderAlerts() {
  const now = Date.now() / 1000;
  $('#alerts').innerHTML = S.alerts.map((a) => {
    const what = a.kind === 'offline'
      ? `<b>${esc(a.node_name)}</b> 失联`
      : `<b>${esc(a.node_name)}</b> → <b>${esc(a.target_name)}</b> 丢包 100%`;
    return `<a class="alert" href="#/node/${encodeURIComponent(a.node_id)}"><span class="dot"></span>${what}<span class="sub">持续 ${fmtDur(now - a.since)} · 自 ${fmtTime(a.since)}</span></a>`;
  }).join('');
}

async function refreshNodes() {
  try {
    const data = await api('/api/nodes');
    S.nodes = data.nodes || [];
    S.alerts = data.alerts || [];
    renderAlerts();
    S.byId = new Map(S.nodes.map((n) => [n.id, n]));
    S.lastOK = Date.now();
    renderSummary();
    if (S.view === 'nodes') renderNodes();
    if (S.detail) renderDetailInfo();
    renderFooter();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    renderFooter(e.message);
  }
}

function renderFooter(err) {
  const v = S.cfg ? `ServerStatus ${esc(S.cfg.version)} · 时区 ${esc(S.cfg.timezone)}` : '';
  $('#footer').innerHTML = err ? `<span style="color:var(--bad)">连接服务器失败：${esc(err)}</span> · ${v}` : v;
}

// ---------------------------------------------------------------------------
// detail modal

function infoItem(k, v, title) {
  return `<div><div class="k">${esc(k)}</div><div class="v" title="${esc(title ?? v)}">${esc(v)}</div></div>`;
}

function renderDetailInfo() {
  const n = S.byId.get(S.detail);
  if (!n) return;
  const h = n.host || {}, s = n.state, c = n.cycle || {};
  $('#d-name').innerHTML = `<span class="dot ${n.online ? 'on' : ''}" style="display:inline-block;margin-right:8px;vertical-align:middle"></span>${esc(n.name)}`;
  $('#d-sub').textContent = [n.group, n.location, n.online ? '在线' : '离线 · 最后上报 ' + ago(n.last_seen)].filter(Boolean).join(' · ');
  const items = [
    infoItem('主机名', h.hostname || '-'),
    infoItem('系统', h.os || '-'),
    infoItem('内核 / 架构', `${h.kernel || '-'} / ${h.arch || '-'}`),
    infoItem('虚拟化', h.virt || '-'),
    infoItem('CPU', h.cpu_model ? `${h.cpu_model} × ${h.cpu_cores}` : `${h.cpu_cores || '-'} 核`),
    infoItem('启动时间', fmtTime(h.boot_time)),
  ];
  if (s) {
    items.push(
      infoItem('在线时长', fmtDur(s.uptime)),
      infoItem('CPU 使用', `${fmtPct(s.cpu)}  (steal ${fmtPct(s.steal)}, iowait ${fmtPct(s.iowait)})`),
      infoItem('负载 1/5/15', `${s.load1.toFixed(2)} / ${s.load5.toFixed(2)} / ${s.load15.toFixed(2)}`),
      infoItem('内存', `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)} (${fmtPct(pct(s.mem_used, s.mem_total))})`),
      infoItem('Swap', s.swap_total ? `${fmtBytes(s.swap_used)} / ${fmtBytes(s.swap_total)}` : '未启用'),
      infoItem('硬盘', `${fmtBytes(s.disk_used)} / ${fmtBytes(s.disk_total)} (${fmtPct(pct(s.disk_used, s.disk_total))})`),
      infoItem('实时网络', `↓ ${fmtRate(s.rx_rate)}  ↑ ${fmtRate(s.tx_rate)}`),
      infoItem('开机以来流量', `↓ ${fmtBytes(s.net_rx)}  ↑ ${fmtBytes(s.net_tx)}`),
      infoItem('TCP / UDP', `${s.tcp} / ${s.udp}`),
      infoItem('进程 / 线程', `${s.procs} / ${s.threads}`),
    );
  }
  const cyc = `↓ ${fmtBytes(c.rx)}  ↑ ${fmtBytes(c.tx)}` + (c.limit > 0 ? `  (${fmtPct(pct(c.rx + c.tx, c.limit))} / ${fmtBytes(c.limit)})` : '');
  items.push(
    infoItem(`本周期流量 (每月 ${c.reset_day} 日重置)`, cyc, `自 ${fmtTime(c.start, 'date')} 起`),
    infoItem('最后上报', n.last_seen ? `${fmtTime(n.last_seen)} (${ago(n.last_seen)})` : '-'),
    infoItem('Agent 版本', h.agent_version || '-'),
  );
  $('#d-info').innerHTML = items.join('');
  renderPingTable(n);
}

function openDetail(id) {
  if (!S.byId.has(id)) return;
  const changed = S.detail !== id;
  S.detail = id;
  $('#detail').classList.remove('hidden');
  document.body.style.overflow = 'hidden';
  renderDetailInfo();
  if (changed) {
    S.history = null;
    S.pingHistory = null;
    loadHistory();
    loadNodeStats();
  }
}

function closeDetail() {
  S.detail = null;
  $('#detail').classList.add('hidden');
  document.body.style.overflow = '';
  hideTooltip();
}

const RANGE_SECONDS = { '1h': 3600, '6h': 21600, '24h': 86400, '7d': 604800, '30d': 2592000, '90d': 7776000, '1y': 31536000 };

async function loadHistory() {
  const id = S.detail;
  if (!id) return;
  loadPingHistory();
  try {
    const h = await api(`/api/nodes/${encodeURIComponent(id)}/history?range=${S.range}`);
    if (S.detail !== id) return;
    S.history = h;
    drawHistory();
  } catch (e) {
    if (e instanceof AuthError) showLogin();
  }
}

function legend(title, series) {
  return `<span>${esc(title)}</span>` + series.map((s) => `<span class="legend"><i style="background:${s.color}"></i>${esc(s.name)}</span>`).join('');
}

function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

async function loadPingHistory() {
  const id = S.detail;
  const n = S.byId.get(id);
  $('#d-ping').classList.toggle('hidden', !n || !n.pings || !n.pings.length);
  if (!n || !n.pings || !n.pings.length) return;
  try {
    const h = await api(`/api/nodes/${encodeURIComponent(id)}/ping?range=${S.range}`);
    if (S.detail !== id) return;
    S.pingHistory = h;
    drawPingHistory();
  } catch (e) {
    if (e instanceof AuthError) showLogin();
  }
}

function palette() {
  return ['--c1', '--c2', '--c3', '--c4', '--c5', '--c6', '--c7', '--c8'].map(cssVar);
}

function drawPingHistory() {
  const h = S.pingHistory;
  if (!h) return;
  const now = Math.floor(Date.now() / 1000);
  const pal = palette();
  const base = { t: h.t, step: h.step, from: now - RANGE_SECONDS[S.range], to: now };
  const lat = h.targets.map((t, i) => ({ name: t.name, color: pal[i % pal.length], data: t.rtt }));
  const loss = h.targets.map((t, i) => ({ name: t.name, color: pal[i % pal.length], data: t.loss }));
  $('#t-rtt').innerHTML = legend('延迟', lat);
  $('#t-loss').innerHTML = legend('丢包率', loss);
  drawChart($('#c-rtt'), { ...base, fmt: (v) => `${v < 10 ? v.toFixed(1) : Math.round(v)} ms`, series: lat });
  drawChart($('#c-loss'), { ...base, max: 100, fmt: (v) => fmtPct(v, 0), series: loss });
}

function renderPingTable(n) {
  const el = $('#d-ping-table');
  if (!n.pings || !n.pings.length) { el.innerHTML = ''; return; }
  const rows = n.pings.map((p) => `<tr><td><span class="chip ${pingClass(p)}">${esc(p.name)}</span></td><td>${esc(p.host)}</td>
    <td>${p.latency == null ? '-' : p.latency.toFixed(1) + ' ms'}</td><td>${p.loss == null ? '-' : fmtPct(p.loss)}</td><td>${p.sent || '-'}</td></tr>`).join('');
  el.innerHTML = `<thead><tr><th>目标</th><th>地址</th><th>实时延迟</th><th>丢包率 (近 1-2 分钟)</th><th>探测次数</th></tr></thead><tbody>${rows}</tbody>`;
}

function drawHistory() {
  const h = S.history;
  if (!h) return;
  const now = Math.floor(Date.now() / 1000);
  const base = { t: h.t, step: h.step, from: now - RANGE_SECONDS[S.range], to: now };
  const c1 = cssVar('--c1'), c2 = cssVar('--c2'), c3 = cssVar('--c3'), c4 = cssVar('--c4');
  const defs = [
    ['cpu', 'CPU', { max: 100, fmt: (v) => fmtPct(v, 0), series: [
      { name: '平均', color: c1, data: h.cpu, fill: true },
      { name: '峰值', color: c4, data: h.cpu_max },
    ] }],
    ['mem', '内存 / 硬盘', { max: 100, fmt: (v) => fmtPct(v, 0), series: [
      { name: '内存', color: c3, data: h.mem, fill: true },
      { name: '硬盘', color: c4, data: h.disk },
    ] }],
    ['net', '网络速率', { fmt: fmtRate, binary: true, series: [
      { name: '下行', color: c1, data: h.rx, fill: true },
      { name: '上行', color: c2, data: h.tx, fill: true },
    ] }],
    ['load', '负载 (1 分钟)', { fmt: (v) => v.toFixed(2), series: [{ name: '负载', color: c4, data: h.load, fill: true }] }],
    ['tcp', 'TCP 连接', { fmt: (v) => Math.round(v), series: [{ name: 'TCP', color: c3, data: h.tcp, fill: true }] }],
    ['proc', '进程数', { fmt: (v) => Math.round(v), series: [{ name: '进程', color: c2, data: h.procs, fill: true }] }],
  ];
  for (const [key, title, opt] of defs) {
    $(`#t-${key}`).innerHTML = legend(title, opt.series);
    drawChart($(`#c-${key}`), { ...base, ...opt });
  }
}

// ---------------------------------------------------------------------------
// SVG line chart

function niceMax(v, binary) {
  if (!(v > 0)) return 1;
  if (binary && v >= 1024) {
    const p = Math.pow(1024, Math.floor(Math.log(v) / Math.log(1024)));
    return niceMax(v / p) * p;
  }
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / p;
  return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? 2.5 : n <= 5 ? 5 : 10) * p;
}

function drawChart(el, opt) {
  el._opt = opt;
  const W = el.clientWidth || 400, H = el.clientHeight || 190;
  const pl = 58, pr = 8, pt = 8, pb = 20;
  const { t, step } = opt;
  const x0 = opt.from, x1 = opt.to;
  let dmax = 0;
  for (const s of opt.series) for (const v of s.data || []) if (v > dmax) dmax = v;
  const ymax = opt.max ?? niceMax(dmax * 1.1, opt.binary);
  const X = (v) => pl + ((v - x0) / (x1 - x0 || 1)) * (W - pl - pr);
  const Y = (v) => pt + (1 - Math.min(v, ymax) / ymax) * (H - pt - pb);

  let svg = `<svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">`;
  for (let i = 0; i <= 4; i++) {
    const v = (ymax * i) / 4, y = Y(v);
    svg += `<line class="gridline" x1="${pl}" x2="${W - pr}" y1="${y}" y2="${y}"/>`;
    svg += `<text class="axis" x="${pl - 6}" y="${y + 3}" text-anchor="end">${esc(opt.fmt(v))}</text>`;
  }
  const span = x1 - x0;
  const ticks = Math.max(2, Math.min(6, Math.floor((W - pl) / 90)));
  for (let i = 0; i <= ticks; i++) {
    const tv = x0 + (span * i) / ticks;
    const anchor = i === 0 ? 'start' : i === ticks ? 'end' : 'middle';
    svg += `<text class="axis" x="${X(tv)}" y="${H - 5}" text-anchor="${anchor}">${fmtTime(tv, span > 2 * 86400 ? 'md' : 'time')}</text>`;
  }
  if (!t || !t.length) {
    svg += `<text class="nodata" x="${(W + pl) / 2}" y="${H / 2}" text-anchor="middle">暂无数据</text></svg>`;
    el.innerHTML = svg;
    return;
  }
  const gap = step * 1.5;
  const base = Y(0);
  for (const s of opt.series) {
    // null = no data: the line is interrupted instead of dropping to zero
    let line = '', area = '', segStart = -1, prev = -1;
    const closeSeg = () => {
      if (segStart < 0) return;
      if (s.fill) area += `L${X(t[prev] + step / 2).toFixed(1)},${base}L${X(t[segStart] + step / 2).toFixed(1)},${base}Z`;
      if (prev === segStart) svg += `<circle cx="${X(t[prev] + step / 2)}" cy="${Y(s.data[prev])}" r="2" fill="${s.color}"/>`;
      segStart = -1;
    };
    for (let i = 0; i < t.length; i++) {
      const v = s.data[i];
      if (v == null) { closeSeg(); continue; }
      const x = X(t[i] + step / 2).toFixed(1), y = Y(v).toFixed(1);
      const brk = segStart < 0 || t[i] - t[prev] > gap;
      if (brk) { closeSeg(); segStart = i; }
      line += `${brk ? 'M' : 'L'}${x},${y}`;
      area += `${brk ? 'M' : 'L'}${x},${y}`;
      prev = i;
    }
    closeSeg();
    if (s.fill && area) svg += `<path d="${area}" fill="${s.color}" fill-opacity="0.12" stroke="none"/>`;
    if (line) svg += `<path d="${line}" fill="none" stroke="${s.color}" stroke-width="1.5" stroke-linejoin="round"/>`;
  }
  svg += `<line class="cursor" x1="0" x2="0" y1="${pt}" y2="${H - pb}" visibility="hidden"/>`;
  svg += `<rect class="hover" x="${pl}" y="0" width="${W - pl - pr}" height="${H}" fill="transparent"/></svg>`;
  el.innerHTML = svg;

  const rect = $('rect.hover', el), cursor = $('line.cursor', el);
  const onMove = (ev) => {
    const box = el.getBoundingClientRect();
    const px = ((ev.clientX - box.left) / box.width) * W;
    const tv = x0 + ((px - pl) / (W - pl - pr)) * span - step / 2;
    let lo = 0, hi = t.length - 1;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (t[mid] < tv) lo = mid + 1; else hi = mid; }
    if (lo > 0 && Math.abs(t[lo - 1] - tv) < Math.abs(t[lo] - tv)) lo--;
    const cx = X(t[lo] + step / 2);
    cursor.setAttribute('x1', cx); cursor.setAttribute('x2', cx); cursor.setAttribute('visibility', 'visible');
    const rows = opt.series.map((s) => `<div class="tt-row"><i style="background:${s.color}"></i>${esc(s.name)}：<b>${s.data[lo] == null ? '-' : esc(opt.fmt(s.data[lo]))}</b></div>`).join('');
    const label = step >= 86400 ? fmtTime(t[lo], 'date') : `${fmtTime(t[lo])}${step > 60 ? ' – ' + fmtTime(t[lo] + step, 'time') : ''}`;
    showTooltip(`<div class="muted">${label}</div>${rows}`, ev.clientX, ev.clientY);
  };
  rect.addEventListener('mousemove', onMove);
  rect.addEventListener('touchstart', (e) => onMove(e.touches[0]), { passive: true });
  rect.addEventListener('touchmove', (e) => onMove(e.touches[0]), { passive: true });
  rect.addEventListener('mouseleave', () => { cursor.setAttribute('visibility', 'hidden'); hideTooltip(); });
}

function showTooltip(html, x, y) {
  const tt = $('#tooltip');
  tt.innerHTML = html;
  tt.classList.remove('hidden');
  const w = tt.offsetWidth, h = tt.offsetHeight;
  let left = x + 14, top = y + 14;
  if (left + w > window.innerWidth - 8) left = x - w - 14;
  if (top + h > window.innerHeight - 8) top = y - h - 14;
  tt.style.left = Math.max(8, left) + 'px';
  tt.style.top = Math.max(8, top) + 'px';
}
function hideTooltip() { $('#tooltip').classList.add('hidden'); }

// ---------------------------------------------------------------------------
// period statistics

function availCell(st) {
  if (st.availability == null) return '-';
  const a = st.availability;
  const color = a >= 99.9 ? 'var(--ok)' : a >= 99 ? 'var(--warn)' : 'var(--bad)';
  return `<span style="color:${color}">${a.toFixed(2)}%</span>`;
}

async function loadNodeStats() {
  const id = S.detail;
  if (!id) return;
  const count = { day: 30, week: 26, month: 24 }[S.dPeriod];
  try {
    const data = await api(`/api/nodes/${encodeURIComponent(id)}/stats?period=${S.dPeriod}&count=${count}`);
    if (S.detail !== id) return;
    const rows = data.stats.slice().reverse().filter((r, i) => i === 0 || r.samples > 0 || r.expected_minutes > 0);
    const head = `<thead><tr><th>周期</th><th>可用率</th><th>CPU 均值</th><th>CPU 峰值</th><th>内存均值</th><th>负载均值</th><th>下行流量</th><th>上行流量</th><th>合计</th><th>峰值下行</th><th>峰值上行</th></tr></thead>`;
    const body = rows.map((r) => {
      const has = r.samples > 0;
      return `<tr><td>${esc(r.label)}</td><td>${availCell(r)}</td>
        <td>${has ? fmtPct(r.cpu_avg) : '-'}</td><td>${has ? fmtPct(r.cpu_max) : '-'}</td>
        <td>${has ? fmtPct(r.mem_pct) : '-'}</td><td>${has ? r.load_avg.toFixed(2) : '-'}</td>
        <td class="down">${fmtBytes(r.rx)}</td><td class="up">${fmtBytes(r.tx)}</td><td>${fmtBytes(r.rx + r.tx)}</td>
        <td>${has ? fmtRate(r.rx_rate_max) : '-'}</td><td>${has ? fmtRate(r.tx_rate_max) : '-'}</td></tr>`;
    }).join('');
    $('#d-stats').innerHTML = head + `<tbody>${body}</tbody>`;
  } catch (e) {
    if (e instanceof AuthError) showLogin();
  }
}

const STATS_COLS = [
  { key: 'name', label: '节点', val: (r) => r.name, fmt: (r) => `${esc(r.name)}${r.group ? ` <span class="sub">${esc(r.group)}</span>` : ''}` },
  { key: 'avail', label: '可用率', val: (r) => r.stat.availability ?? -1, fmt: (r) => availCell(r.stat) },
  { key: 'cpu', label: 'CPU 均值', val: (r) => r.stat.cpu_avg, fmt: (r) => (r.stat.samples ? fmtPct(r.stat.cpu_avg) : '-') },
  { key: 'cpumax', label: 'CPU 峰值', val: (r) => r.stat.cpu_max, fmt: (r) => (r.stat.samples ? fmtPct(r.stat.cpu_max) : '-') },
  { key: 'mem', label: '内存均值', val: (r) => r.stat.mem_pct, fmt: (r) => (r.stat.samples ? fmtPct(r.stat.mem_pct) : '-') },
  { key: 'load', label: '负载均值', val: (r) => r.stat.load_avg, fmt: (r) => (r.stat.samples ? r.stat.load_avg.toFixed(2) : '-') },
  { key: 'rx', label: '下行流量', val: (r) => r.stat.rx, fmt: (r) => `<span class="down">${fmtBytes(r.stat.rx)}</span>` },
  { key: 'tx', label: '上行流量', val: (r) => r.stat.tx, fmt: (r) => `<span class="up">${fmtBytes(r.stat.tx)}</span>` },
  { key: 'total', label: '合计', val: (r) => r.stat.rx + r.stat.tx, fmt: (r) => fmtBytes(r.stat.rx + r.stat.tx) },
  { key: 'rxmax', label: '峰值下行', val: (r) => r.stat.rx_rate_max, fmt: (r) => (r.stat.samples ? fmtRate(r.stat.rx_rate_max) : '-') },
  { key: 'txmax', label: '峰值上行', val: (r) => r.stat.tx_rate_max, fmt: (r) => (r.stat.samples ? fmtRate(r.stat.tx_rate_max) : '-') },
];

async function loadStats() {
  try {
    S.stats = await api(`/api/stats?period=${S.statsPeriod}&offset=${S.statsOffset}`);
    renderStats();
  } catch (e) {
    if (e instanceof AuthError) showLogin();
  }
}

function renderStats() {
  const d = S.stats;
  if (!d) return;
  const endLabel = fmtTime(d.end - 1, 'date');
  $('#stats-label').textContent = S.statsPeriod === 'day' ? d.label : `${d.label}（${fmtTime(d.start, 'date')} ~ ${endLabel}）`;
  $('#stats-next').disabled = S.statsOffset === 0;
  const col = STATS_COLS.find((c) => c.key === S.statsSort.key) || STATS_COLS[8];
  const rows = d.nodes.slice().sort((a, b) => {
    const va = col.val(a), vb = col.val(b);
    const r = typeof va === 'string' ? va.localeCompare(vb) : va - vb;
    return S.statsSort.asc ? r : -r;
  });
  const head = `<thead><tr>${STATS_COLS.map((c) => `<th class="sortable ${c.key === col.key ? 'sorted' + (S.statsSort.asc ? ' asc' : '') : ''}" data-key="${c.key}">${c.label}</th>`).join('')}</tr></thead>`;
  const body = rows.map((r) => `<tr class="link" data-id="${esc(r.id)}">${STATS_COLS.map((c) => `<td>${c.fmt(r)}</td>`).join('')}</tr>`).join('');
  const t = d.total;
  const foot = `<tfoot><tr><td>合计（${rows.length} 个节点）</td><td></td><td></td><td></td><td></td><td></td>
    <td class="down">${fmtBytes(t.rx)}</td><td class="up">${fmtBytes(t.tx)}</td><td>${fmtBytes(t.rx + t.tx)}</td><td></td><td></td></tr></tfoot>`;
  $('#stats-table').innerHTML = head + `<tbody>${body || '<tr><td colspan="11" class="muted">暂无数据</td></tr>'}</tbody>` + foot;
}

function exportCSV() {
  const d = S.stats;
  if (!d) return;
  const cols = ['id', 'name', 'group', 'location', 'availability_pct', 'cpu_avg_pct', 'cpu_max_pct', 'mem_avg_pct', 'load_avg',
    'rx_bytes', 'tx_bytes', 'rx_rate_max_Bps', 'tx_rate_max_Bps', 'online_minutes', 'expected_minutes'];
  const q = (v) => `"${String(v ?? '').replace(/"/g, '""')}"`;
  const lines = [cols.join(',')];
  for (const r of d.nodes) {
    const s = r.stat;
    lines.push([r.id, r.name, r.group, r.location, s.availability ?? '', s.cpu_avg.toFixed(2), s.cpu_max.toFixed(2),
      s.mem_pct.toFixed(2), s.load_avg.toFixed(2), s.rx, s.tx, s.rx_rate_max, s.tx_rate_max, s.online_minutes, s.expected_minutes].map(q).join(','));
  }
  const blob = new Blob(['﻿' + lines.join('\n')], { type: 'text/csv;charset=utf-8' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = `serverstatus-${d.period}-${d.label}.csv`;
  document.body.appendChild(a);
  a.click();
  setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
}

// ---------------------------------------------------------------------------
// routing, auth, events

function route() {
  const h = location.hash.replace(/^#/, '');
  const m = h.match(/^\/node\/(.+)$/);
  S.view = h.startsWith('/stats') ? 'stats' : 'nodes';
  $('#view-nodes').classList.toggle('hidden', S.view !== 'nodes');
  $('#view-stats').classList.toggle('hidden', S.view !== 'stats');
  $$('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.view === S.view));
  if (S.view === 'stats') loadStats();
  else renderNodes();
  if (m) openDetail(decodeURIComponent(m[1]));
  else if (S.detail) closeDetail();
}

function showLogin() {
  $('#login').classList.remove('hidden');
  $('#login-pw').focus();
}

function setSeg(container, attr, value) {
  $$(`button`, container).forEach((b) => b.classList.toggle('active', b.dataset[attr] === value));
}

function bindEvents() {
  $('#node-rows').addEventListener('click', (e) => {
    const row = e.target.closest('.row.node');
    if (row) location.hash = '#/node/' + encodeURIComponent(row.dataset.id);
  });
  $('#filter').addEventListener('input', (e) => { S.filter = e.target.value.trim().toLowerCase(); renderNodes(); });
  $('#node-filter').addEventListener('click', (e) => {
    const b = e.target.closest('button'); if (!b) return;
    S.nodeFilter = b.dataset.f; setSeg($('#node-filter'), 'f', S.nodeFilter); renderNodes();
  });
  $('#d-close').addEventListener('click', () => { location.hash = S.view === 'stats' ? '#/stats' : '#/'; });
  $('#detail').addEventListener('click', (e) => { if (e.target.id === 'detail') $('#d-close').click(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && S.detail) $('#d-close').click(); });
  $('#d-range').addEventListener('click', (e) => {
    const b = e.target.closest('button'); if (!b) return;
    S.range = b.dataset.r; setSeg($('#d-range'), 'r', S.range); loadHistory();
  });
  $('#d-period').addEventListener('click', (e) => {
    const b = e.target.closest('button'); if (!b) return;
    S.dPeriod = b.dataset.p; setSeg($('#d-period'), 'p', S.dPeriod); loadNodeStats();
  });
  $('#stats-period').addEventListener('click', (e) => {
    const b = e.target.closest('button'); if (!b) return;
    S.statsPeriod = b.dataset.p; S.statsOffset = 0; setSeg($('#stats-period'), 'p', S.statsPeriod); loadStats();
  });
  $('#stats-prev').addEventListener('click', () => { S.statsOffset++; loadStats(); });
  $('#stats-next').addEventListener('click', () => { if (S.statsOffset > 0) { S.statsOffset--; loadStats(); } });
  $('#stats-csv').addEventListener('click', exportCSV);
  $('#stats-table').addEventListener('click', (e) => {
    const th = e.target.closest('th.sortable');
    if (th) {
      const key = th.dataset.key;
      S.statsSort = { key, asc: S.statsSort.key === key ? !S.statsSort.asc : key === 'name' };
      return renderStats();
    }
    const tr = e.target.closest('tr.link');
    if (tr) location.hash = '#/node/' + encodeURIComponent(tr.dataset.id);
  });
  $('#login-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    $('#login-err').textContent = '';
    try {
      await api('/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: $('#login-pw').value }) });
      $('#login').classList.add('hidden');
      $('#login-pw').value = '';
      await init();
    } catch (err) {
      $('#login-err').textContent = err instanceof AuthError ? '密码错误' : err.message;
    }
  });
  $('#logout').addEventListener('click', async () => {
    await api('/api/logout', { method: 'POST' }).catch(() => {});
    location.reload();
  });
  window.addEventListener('hashchange', route);
  let rt;
  window.addEventListener('resize', () => {
    clearTimeout(rt);
    rt = setTimeout(() => { if (S.detail) { drawHistory(); drawPingHistory(); } }, 150);
  });
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') refreshNodes(); });
}

let timers = [];
async function init() {
  timers.forEach(clearInterval);
  timers = [];
  try {
    S.cfg = await api('/api/config');
  } catch (e) {
    renderFooter(e.message);
    setTimeout(init, 5000);
    return;
  }
  DTF = null;
  document.title = S.cfg.title;
  $('#title').textContent = S.cfg.title;
  $('#logout').classList.toggle('hidden', !S.cfg.authed);
  if (S.cfg.auth_required && !S.cfg.authed) return showLogin();
  await refreshNodes();
  route();
  timers.push(setInterval(() => { if (document.visibilityState === 'visible') refreshNodes(); }, 2000));
  timers.push(setInterval(() => {
    if (document.visibilityState !== 'visible') return;
    if (S.detail) { loadHistory(); }
    if (S.view === 'stats') loadStats();
  }, 60000));
}

bindEvents();
init();
