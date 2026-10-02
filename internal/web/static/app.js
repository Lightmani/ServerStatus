'use strict';

// All agent-supplied strings are written with textContent, never innerHTML:
// an authenticated agent is still untrusted input for the browser.

const REFRESH_MS = 2000;
const SVG_NS = 'http://www.w3.org/2000/svg';
const expanded = new Set();
let lastData = null;

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function fmtBytes(n, digits = 1) {
  if (!n || n < 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n.toFixed(0) : n.toFixed(digits)) + ' ' + units[i];
}
const fmtRate = (n) => fmtBytes(n) + '/s';

function fmtDuration(s) {
  s = Math.max(0, Math.floor(s));
  const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
  if (d > 0) return `${d} 天 ${h} 时`;
  if (h > 0) return `${h} 时 ${m} 分`;
  if (m > 0) return `${m} 分`;
  return `${s} 秒`;
}

function fmtAgo(now, t) {
  if (!t) return '从未上报';
  return fmtDuration(now - t) + '前';
}

const pct = (u) => (u && u.total > 0 ? (u.used / u.total) * 100 : 0);

function bar(percent, title) {
  const wrap = el('div', 'bar');
  if (percent >= 90) wrap.classList.add('crit');
  else if (percent >= 70) wrap.classList.add('warn');
  const fill = el('div', 'fill');
  fill.style.width = Math.min(100, Math.max(0, percent)).toFixed(1) + '%';
  wrap.append(fill, el('span', 'label', Math.round(percent) + '%'));
  if (title) wrap.title = title;
  return wrap;
}

function pair(a, b) {
  const p = el('div', 'pair');
  p.append(el('span', 'rx', a), el('span', 'tx', b));
  return p;
}

function td(cls, child) {
  const c = el('td', cls);
  if (child instanceof Node) c.append(child);
  else if (child !== undefined) c.textContent = child;
  return c;
}

function nodeRow(n, now) {
  const r = n.report;
  const tr = el('tr', 'row' + (n.online ? '' : ' offline'));
  tr.tabIndex = 0;
  tr.setAttribute('aria-expanded', expanded.has(n.id) ? 'true' : 'false');

  const name = el('div', 'name');
  const dot = el('span', 'dot');
  dot.title = n.online ? '在线' : '离线';
  const text = el('div');
  const title = el('div', 'name-text', n.name);
  title.title = n.id;
  const sub = el('div', 'sub');
  if (n.location) sub.append(el('span', 'tag', n.location), ' ');
  sub.append(n.online ? (r && r.host.virtualization ? r.host.virtualization : '') : '离线 · ' + fmtAgo(now, n.last_seen));
  text.append(title, sub);
  name.append(dot, text);
  tr.append(td('c-name', name));

  if (!r) {
    for (const cls of ['c-uptime', 'c-load', 'c-net', 'c-traffic', 'c-bar', 'c-bar', 'c-bar c-disk']) tr.append(td(cls, '–'));
  } else {
    tr.append(td('c-uptime', fmtDuration(r.uptime)));
    tr.append(td('c-load', r.load.map((x) => x.toFixed(2)).join(' ')));
    tr.append(td('c-net', n.online ? pair('↓ ' + fmtRate(r.network.rx_rate), '↑ ' + fmtRate(r.network.tx_rate)) : '–'));
    tr.append(td('c-traffic', pair('↓ ' + fmtBytes(r.network.rx_bytes), '↑ ' + fmtBytes(r.network.tx_bytes))));
    tr.append(td('c-bar', bar(n.online ? r.cpu : 0, `${r.host.cpu_cores} 核`)));
    tr.append(td('c-bar', bar(pct(r.memory), `${fmtBytes(r.memory.used)} / ${fmtBytes(r.memory.total)}`)));
    tr.append(td('c-bar c-disk', bar(pct(r.disk), `${fmtBytes(r.disk.used)} / ${fmtBytes(r.disk.total)}`)));
  }

  const toggle = () => {
    if (expanded.has(n.id)) expanded.delete(n.id); else expanded.add(n.id);
    render(lastData);
  };
  tr.addEventListener('click', toggle);
  tr.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); }
  });
  return tr;
}

function kv(k, v) {
  const d = el('div', 'kv');
  const val = el('span', 'v', v);
  val.title = v;
  d.append(el('span', 'k', k), val);
  return d;
}

function sparkline(label, series, opts) {
  const box = el('div', 'chart');
  const head = el('div', 'chart-head');
  head.append(el('span', '', label), el('span', '', opts.current));
  box.append(head);

  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 100 40');
  svg.setAttribute('preserveAspectRatio', 'none');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', label);

  let max = opts.max || 0;
  if (!opts.max) for (const s of series) for (const v of s.values) max = Math.max(max, v);
  if (max <= 0) max = 1;

  for (const s of series) {
    const vals = s.values;
    if (vals.length < 2) continue;
    const pts = vals.map((v, i) => `${(i / (vals.length - 1)) * 100},${40 - (v / max) * 38}`);
    const area = document.createElementNS(SVG_NS, 'polygon');
    area.setAttribute('points', `0,40 ${pts.join(' ')} 100,40`);
    area.setAttribute('class', 'area');
    area.style.fill = s.color;
    const line = document.createElementNS(SVG_NS, 'polyline');
    line.setAttribute('points', pts.join(' '));
    line.setAttribute('class', 'line');
    line.style.stroke = s.color;
    svg.append(area, line);
  }
  box.append(svg);
  return box;
}

function detailRow(n, now) {
  const tr = el('tr', 'detail');
  const cell = el('td');
  cell.colSpan = 8;
  tr.append(cell);
  const r = n.report;
  if (!r) {
    cell.append(el('div', 'sub', '该节点尚未上报任何数据。'));
    return tr;
  }
  const h = r.host;
  const grid = el('div', 'detail-grid');
  grid.append(
    kv('主机名', h.hostname || '–'),
    kv('系统', h.os || '–'),
    kv('内核', h.kernel || '–'),
    kv('架构 / 虚拟化', `${h.arch || '–'} / ${h.virtualization || '–'}`),
    kv('CPU', `${h.cpu_model || '–'} × ${h.cpu_cores}`),
    kv('负载', r.load.map((x) => x.toFixed(2)).join(' / ')),
    kv('内存', `${fmtBytes(r.memory.used)} / ${fmtBytes(r.memory.total)}`),
    kv('Swap', r.swap.total ? `${fmtBytes(r.swap.used)} / ${fmtBytes(r.swap.total)}` : '未启用'),
    kv('硬盘', `${fmtBytes(r.disk.used)} / ${fmtBytes(r.disk.total)}`),
    kv('连接数 TCP / UDP', `${r.tcp} / ${r.udp}`),
    kv('进程数', String(r.processes)),
    kv('在线时间', fmtDuration(r.uptime)),
    kv('最后上报', fmtAgo(now, n.last_seen)),
    kv('客户端版本', r.agent_version || '–'),
  );
  if (n.ip) grid.append(kv('上报 IP', n.ip));
  grid.append(kv('节点 ID', n.id));
  cell.append(grid);

  const hist = n.history || [];
  if (hist.length > 1) {
    const charts = el('div', 'charts');
    const last = hist[hist.length - 1];
    charts.append(
      sparkline('CPU', [{ values: hist.map((s) => s.cpu), color: 'var(--ok)' }], { max: 100, current: last.cpu.toFixed(1) + '%' }),
      sparkline('内存', [{ values: hist.map((s) => s.mem), color: 'var(--warn)' }], { max: 100, current: last.mem.toFixed(1) + '%' }),
      sparkline('网络', [
        { values: hist.map((s) => s.rx), color: 'var(--rx)' },
        { values: hist.map((s) => s.tx), color: 'var(--tx)' },
      ], { current: `↓ ${fmtRate(last.rx)}  ↑ ${fmtRate(last.tx)}` }),
    );
    cell.append(charts);
  }

  if (r.disks && r.disks.length) {
    cell.append(el('div', 'section-title', '分区'));
    const disks = el('div', 'disks');
    for (const d of r.disks) {
      const row = el('div', 'disk-row');
      const mp = el('div', 'mp', `${d.mount} (${d.fstype})`);
      mp.title = `${d.mount}  ${fmtBytes(d.used)} / ${fmtBytes(d.total)}`;
      row.append(mp, bar(pct(d), mp.title));
      disks.append(row);
    }
    cell.append(disks);
  }
  return tr;
}

function renderSummary(data) {
  const s = document.getElementById('summary');
  let online = 0, rx = 0, tx = 0, rxTotal = 0, txTotal = 0;
  for (const n of data.servers) {
    if (!n.report) continue;
    rxTotal += n.report.network.rx_bytes;
    txTotal += n.report.network.tx_bytes;
    if (!n.online) continue;
    online++;
    rx += n.report.network.rx_rate;
    tx += n.report.network.tx_rate;
  }
  const item = (k, v) => { const sp = el('span', '', k); sp.append(el('b', '', v)); return sp; };
  s.replaceChildren(
    item('在线', `${online} / ${data.servers.length}`),
    item('实时', `↓ ${fmtRate(rx)}  ↑ ${fmtRate(tx)}`),
    item('累计', `↓ ${fmtBytes(rxTotal)}  ↑ ${fmtBytes(txTotal)}`),
  );
}

function render(data) {
  if (!data) return;
  const now = data.now;
  const frag = document.createDocumentFragment();
  for (const n of data.servers) {
    frag.append(nodeRow(n, now));
    if (expanded.has(n.id)) frag.append(detailRow(n, now));
  }
  // Keep keyboard focus on the same row across refreshes.
  const focusedIdx = [...document.querySelectorAll('#nodes tr.row')].indexOf(document.activeElement);
  const body = document.getElementById('nodes');
  body.replaceChildren(frag);
  if (focusedIdx >= 0) {
    const rows = body.querySelectorAll('tr.row');
    if (rows[focusedIdx]) rows[focusedIdx].focus();
  }
  document.getElementById('empty').hidden = data.servers.length > 0;
  renderSummary(data);
  document.getElementById('updated').textContent = '更新于 ' + new Date(now * 1000).toLocaleTimeString();
}

async function refresh() {
  const errBox = document.getElementById('error');
  try {
    const res = await fetch('api/servers', { cache: 'no-store', credentials: 'same-origin' });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    lastData = await res.json();
    errBox.hidden = true;
    render(lastData);
  } catch (e) {
    errBox.textContent = '无法连接服务端：' + e.message + '（将自动重试）';
    errBox.hidden = false;
  } finally {
    setTimeout(refresh, REFRESH_MS);
  }
}

refresh();
