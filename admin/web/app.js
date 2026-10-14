'use strict';
// VoxelPort operator dashboard. Polls /api/overview every 5 s and the chart
// history every 15 s. Everything user-supplied (server titles, names, IPs)
// goes through esc() before it touches innerHTML.

const $ = (s) => document.querySelector(s);
const esc = (v) => String(v ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

const S1 = css('--series-1');
const S2 = css('--series-2');
const BRAND = css('--brand');

const FREE_EGRESS = 100 * 1024 ** 3; // AWS free data out per month

let range = '1h';
let lastOK = 0;
let ov = null;
const armed = new Map(); // "action:id" -> expiry ms, for two-click confirmation
const openDetails = new Set();

// ─── Formatting ─────────────────────────────────────────────────────────────
function fmtBytes(n) {
  n = Number(n) || 0;
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n >= 100 || i === 0 ? n.toFixed(0) : n.toFixed(1)} ${u[i]}`;
}
function fmtRate(bytesPerSec) {
  const bits = (Number(bytesPerSec) || 0) * 8;
  if (bits >= 1e9) return `${(bits / 1e9).toFixed(2)} Gbps`;
  if (bits >= 1e6) return `${(bits / 1e6).toFixed(bits >= 1e8 ? 0 : 1)} Mbps`;
  if (bits >= 1e3) return `${(bits / 1e3).toFixed(0)} kbps`;
  return `${bits.toFixed(0)} bps`;
}
function fmtNum(n) { return (Number(n) || 0).toLocaleString('en-IN'); }
function fmtDur(sec) {
  sec = Math.max(0, Math.floor(sec));
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m`;
  return `${sec}s`;
}
function ago(ts) {
  if (!ts) return '—';
  const s = (Date.now() - new Date(ts).getTime()) / 1000;
  return s < 60 ? 'just now' : `${fmtDur(s)} ago`;
}
function hhmm(ms) { const d = new Date(ms); return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); }
function shortDate(ms) { return new Date(ms).toLocaleDateString([], { day: 'numeric', month: 'short' }); }
function localDay(d) { return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`; }
function dayLabel(iso) { const d = new Date(iso + 'T00:00:00Z'); return d.toLocaleDateString([], { day: 'numeric', month: 'short', timeZone: 'UTC' }); }

// ─── API ────────────────────────────────────────────────────────────────────
async function api(path, opts) {
  const r = await fetch(path, { credentials: 'same-origin', ...opts });
  if (r.status === 401) { location.href = '/login.html'; throw new Error('signed out'); }
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}

function toast(msg, bad) {
  const t = $('#toast');
  t.textContent = msg;
  t.className = 'toast' + (bad ? ' bad' : '');
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, 3200);
}

const ACTION_TEXT = { kick: 'Kicked', ban: 'Blocked', unban: 'Unblocked', hide: 'Hidden from the server list', unhide: 'Back on the server list', release_name: 'Address released', supporter: 'Marked as a supporter', unsupporter: 'Supporter badge removed' };

// A button that asks for a short address first (see grantShort).
const grantBtn = (id, region) => `<button type="button" class="btn sm" data-grant="${esc(id)}" data-region="${esc(region)}">Short address…</button>`;

// The relays the dashboard watches: India (this machine) and, when
// configured, North America. Each server, address and session belongs to one.
const naOn = () => Boolean(ov && 'relay_na' in ov);
function relays() {
  const out = [{ id: 'in', label: 'India', r: ov.relay, err: ov.relay_err }];
  if (naOn()) out.push({ id: 'na', label: 'North America', r: ov.relay_na, err: ov.relay_na_err });
  return out;
}
const regionTag = (id) => (naOn() ? `<span class="tag region-${id}">${id === 'na' ? 'NA' : 'IN'}</span>` : '');

async function act(action, id, region = 'in') {
  const key = `${action}:${id}:${region}`;
  const exp = armed.get(key);
  if (!exp || exp < Date.now()) {
    armed.set(key, Date.now() + 4000);
    renderAll();
    setTimeout(renderAll, 4100);
    return;
  }
  armed.delete(key);
  try {
    await api('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-VP-Admin': '1' }, body: JSON.stringify(/^[0-9a-f]{8,}$/.test(id) ? { action, id, region } : { action, name: id, region }) });
    toast(ACTION_TEXT[action] || 'Done');
  } catch (e) {
    toast(e.message, true);
  }
  refresh();
}

function actBtn(action, id, label, opts = {}) {
  const region = opts.region || 'in';
  const key = `${action}:${id}:${region}`;
  const isArmed = (armed.get(key) || 0) > Date.now();
  return `<button type="button" class="btn sm ${opts.danger ? 'danger' : ''} ${isArmed ? 'armed' : ''}" data-act="${esc(action)}" data-id="${esc(id)}" data-region="${esc(region)}">${isArmed ? `Confirm ${esc(label.toLowerCase())}?` : esc(label)}</button>`;
}

// Gives a supporter a 1–2 character address (replacing the one they have now).
async function grantShort(id, region) {
  const name = (prompt('Short address to give this host (1–2 letters or numbers, e.g. "vp").\nIt replaces their current address, and nobody else can claim it.') || '').trim().toLowerCase();
  if (!name) return;
  try {
    await api('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-VP-Admin': '1' }, body: JSON.stringify({ action: 'grant_name', id, name, region }) });
    toast(`Gave them ${name}.voxelport.in`);
  } catch (e) {
    toast(e.message, true);
  }
  refresh();
}

document.addEventListener('click', (e) => {
  const g = e.target.closest('[data-grant]');
  if (g) { grantShort(g.dataset.grant, g.dataset.region); return; }
  const b = e.target.closest('[data-act]');
  if (b) act(b.dataset.act, b.dataset.id, b.dataset.region);
});
document.addEventListener('toggle', (e) => {
  const d = e.target;
  if (d.matches?.('details[data-key]')) d.open ? openDetails.add(d.dataset.key) : openDetails.delete(d.dataset.key);
}, true);

// ─── Charts ─────────────────────────────────────────────────────────────────
function niceMax(v) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

/** Line chart with one y-axis, a crosshair and a tooltip. series: [{key,label,color,fmt}] */
function lineChart(el, pts, series, yFmt, opts = {}) {
  const W = el.clientWidth || 400, H = el.clientHeight || 170;
  const m = { l: 52, r: 10, t: 8, b: 20 };
  if (!pts.length) { el.innerHTML = `<div class="empty">${range === '1h' || range === '24h' ? 'Collecting data… the first points appear within a minute.' : 'Not enough saved data for this range yet. Longer ranges fill in as the hours go by.'}</div>`; return; }
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t || t0 + 1;
  let max = opts.max || 0;
  if (!opts.max) for (const p of pts) for (const s of series) max = Math.max(max, s.get(p));
  max = opts.max || Math.max(opts.minMax || 0, niceMax(max * 1.1));
  const x = (t) => m.l + (t1 === t0 ? 0 : (t - t0) / (t1 - t0)) * (W - m.l - m.r);
  const y = (v) => H - m.b - (v / max) * (H - m.t - m.b);

  let svg = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(el.dataset.label || '')}"><g class="grid axis">`;
  for (let i = 0; i <= 4; i++) {
    const v = (max / 4) * i, yy = y(v);
    svg += `<line x1="${m.l}" x2="${W - m.r}" y1="${yy}" y2="${yy}"/><text x="${m.l - 6}" y="${yy + 3.5}" text-anchor="end">${esc(yFmt(v))}</text>`;
  }
  const ticks = 4;
  const long = t1 - t0 > 36 * 3600e3;
  for (let i = 0; i <= ticks; i++) {
    const t = t0 + ((t1 - t0) / ticks) * i;
    svg += `<text x="${x(t)}" y="${H - 4}" text-anchor="${i === 0 ? 'start' : i === ticks ? 'end' : 'middle'}">${esc(long ? shortDate(t) : hhmm(t))}</text>`;
  }
  svg += '</g>';
  for (const s of series) {
    const d = pts.map((p, i) => `${i ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(s.get(p)).toFixed(1)}`).join('');
    if (series.length === 1) svg += `<path class="area" fill="${s.color}" d="${d}L${x(t1)},${y(0)}L${x(t0)},${y(0)}Z"/>`;
    svg += `<path class="line" stroke="${s.color}" d="${d}"/>`;
  }
  svg += `<g class="hover" visibility="hidden"><line class="cross" y1="${m.t}" y2="${H - m.b}"/>${series.map((s) => `<circle class="hover-dot" r="4" fill="${s.color}"/>`).join('')}</g>`;
  svg += `<rect x="${m.l}" y="0" width="${W - m.l - m.r}" height="${H}" fill="transparent" class="hit"/></svg><div class="tip" hidden></div>`;
  el.innerHTML = svg;

  const hit = el.querySelector('.hit'), hover = el.querySelector('.hover'), tip = el.querySelector('.tip');
  const cross = hover.querySelector('.cross'), dots = hover.querySelectorAll('circle');
  const move = (ev) => {
    const rect = el.getBoundingClientRect();
    const px = (ev.clientX - rect.left) * (W / rect.width);
    const t = t0 + ((px - m.l) / (W - m.l - m.r)) * (t1 - t0);
    let lo = 0, hi = pts.length - 1;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (pts[mid].t < t) lo = mid + 1; else hi = mid; }
    if (lo > 0 && Math.abs(pts[lo - 1].t - t) < Math.abs(pts[lo].t - t)) lo--;
    const p = pts[lo], xx = x(p.t);
    hover.setAttribute('visibility', 'visible');
    cross.setAttribute('x1', xx); cross.setAttribute('x2', xx);
    series.forEach((s, i) => { dots[i].setAttribute('cx', xx); dots[i].setAttribute('cy', y(s.get(p))); });
    tip.innerHTML = `<div class="t">${esc(new Date(p.t).toLocaleString([], { hour: '2-digit', minute: '2-digit', day: 'numeric', month: 'short' }))}</div>` +
      series.map((s) => `<div>${series.length > 1 ? `<i style="display:inline-block;width:8px;height:8px;border-radius:2px;background:${s.color};margin-right:6px"></i>` : ''}${esc(s.label)}: <b>${esc(s.fmt(s.get(p)))}</b></div>`).join('');
    tip.hidden = false;
    const left = (xx / W) * rect.width;
    tip.style.left = `${Math.min(Math.max(0, left + 12), rect.width - tip.offsetWidth)}px`;
    tip.style.top = '4px';
  };
  hit.addEventListener('pointermove', move);
  hit.addEventListener('pointerleave', () => { hover.setAttribute('visibility', 'hidden'); tip.hidden = true; });
}

/** Daily bars (one series) with a per-bar tooltip. data: [[isoDay, value]] */
function barChart(el, data, label) {
  const W = el.clientWidth || 400, H = el.clientHeight || 150;
  const m = { l: 36, r: 6, t: 8, b: 20 };
  const max = niceMax(Math.max(1, ...data.map((d) => d[1])) * 1.1);
  const bw = (W - m.l - m.r) / data.length;
  const y = (v) => H - m.b - (v / max) * (H - m.t - m.b);
  let svg = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(label)}"><g class="grid axis">`;
  for (let i = 0; i <= 2; i++) {
    const v = (max / 2) * i;
    svg += `<line x1="${m.l}" x2="${W - m.r}" y1="${y(v)}" y2="${y(v)}"/><text x="${m.l - 6}" y="${y(v) + 3.5}" text-anchor="end">${fmtNum(Math.round(v))}</text>`;
  }
  [0, Math.floor(data.length / 2), data.length - 1].forEach((i, k) => {
    if (data[i]) svg += `<text x="${m.l + bw * i + bw / 2}" y="${H - 4}" text-anchor="${k === 0 ? 'start' : k === 2 ? 'end' : 'middle'}">${esc(dayLabel(data[i][0]))}</text>`;
  });
  svg += '</g>';
  data.forEach(([day, v], i) => {
    const h = Math.max(v > 0 ? 2 : 0, H - m.b - y(v));
    const bx = m.l + bw * i + 1, w = Math.max(1, bw - 2);
    svg += `<rect x="${bx}" y="${H - m.b - h}" width="${w}" height="${h}" rx="${Math.min(3, w / 2)}" fill="${S1}"/>`;
    svg += `<rect x="${m.l + bw * i}" y="${m.t}" width="${bw}" height="${H - m.t - m.b}" fill="transparent" data-i="${i}" class="bar-hit"/>`;
  });
  svg += '</svg><div class="tip" hidden></div>';
  el.innerHTML = svg;
  const tip = el.querySelector('.tip');
  el.querySelectorAll('.bar-hit').forEach((r) => {
    r.addEventListener('pointerenter', () => {
      const [day, v] = data[Number(r.dataset.i)];
      tip.innerHTML = `<div class="t">${esc(dayLabel(day))}</div>${esc(label)}: <b>${fmtNum(v)}</b>`;
      tip.hidden = false;
      const rect = el.getBoundingClientRect();
      const left = ((m.l + bw * Number(r.dataset.i)) / W) * rect.width;
      tip.style.left = `${Math.min(Math.max(0, left), rect.width - tip.offsetWidth)}px`;
      tip.style.top = '4px';
    });
    r.addEventListener('pointerleave', () => { tip.hidden = true; });
  });
}

// ─── Sections ───────────────────────────────────────────────────────────────
function renderServices(services) {
  const names = { 'voxelport-relay': 'Relay', 'voxelport-web': 'Website', 'voxelport-admin': 'Admin', cloudflared: 'Cloudflare tunnel', tailscaled: 'Tailscale', wings: 'Pelican', docker: 'Docker' };
  let html = Object.entries(services || {}).map(([k, v]) => {
    const cls = v === 'active' ? 'good' : v === 'failed' ? 'bad' : 'warn';
    return `<span class="pill" title="${esc(k)}: ${esc(v)}"><span class="dot ${cls}"></span>${esc(names[k] || k)}${v === 'active' ? '' : ` · ${esc(v)}`}</span>`;
  }).join('');
  if (naOn()) {
    const up = Boolean(ov.relay_na);
    html += `<span class="pill" title="North America relay operator API${up ? '' : ': ' + esc(ov.relay_na_err)}"><span class="dot ${up ? 'good' : 'bad'}"></span>NA relay${up ? '' : ' · unreachable'}</span>`;
  }
  $('#services').innerHTML = html;
}

function kpi(label, value, sub) {
  return `<div class="kpi"><span class="kpi-label">${esc(label)}</span><span class="kpi-value">${esc(value)}</span><span class="kpi-sub">${esc(sub)}</span></div>`;
}

function webTotals(web, days) {
  const keys = Object.keys(web?.days || {}).sort().slice(-days);
  let views = 0, visitors = 0;
  for (const k of keys) { views += web.days[k].views; visitors += web.days[k].visitors; }
  return { views, visitors };
}

function renderKPIs() {
  const r = ov.relay, sys = ov.system || {}, web = ov.web, dl = ov.downloads || {};
  const t = r?.totals || {};
  const today = web?.days?.[web.today] || { views: 0, visitors: 0 };
  const gh = dl.github?.total;
  const store = dl.store?.total_90d;
  const na = ov.relay_na, nt = na?.totals || {}, nsys = ov.na_system || {};
  // With two regions: totals across both, split underneath.
  const both = (k) => (r ? t[k] || 0 : 0) + (na ? nt[k] || 0 : 0);
  const split = (k) => `India ${r ? fmtNum(t[k]) : '—'} · NA ${na ? fmtNum(nt[k]) : '—'}`;
  const tiles = naOn() ? [
    kpi('Servers online', r || na ? fmtNum(both('tunnels')) : '—', split('tunnels')),
    kpi('Players now', r || na ? fmtNum(both('players')) : '—', split('players')),
    kpi('Relay traffic now', fmtRate((sys.relay_in || 0) + (sys.relay_out || 0) + (nsys.relay_in || 0) + (nsys.relay_out || 0)),
      `India ${fmtRate((sys.relay_in || 0) + (sys.relay_out || 0))} · NA ${fmtRate((nsys.relay_in || 0) + (nsys.relay_out || 0))}`),
  ] : [
    kpi('Servers online', r ? fmtNum(t.tunnels) : '—', r ? `${fmtNum(t.tunnels_served)} sessions since relay start` : 'Relay not reachable'),
    kpi('Players now', r ? fmtNum(t.players) : '—', r ? `${fmtNum(t.conns)} connections since start` : ''),
    kpi('Relay traffic now', fmtRate((sys.relay_in || 0) + (sys.relay_out || 0)), r ? `${fmtBytes((t.bytes_in || 0) + (t.bytes_out || 0))} since start` : ''),
  ];
  tiles.push(
    kpi('Website visitors now', web ? fmtNum(web.live) : '—', web ? `${fmtNum(today.visitors)} today · ${fmtNum(today.views)} page loads` : 'Stats not reachable'),
    kpi('App downloads', gh != null ? fmtNum(gh + (store || 0)) : '—', `GitHub ${gh != null ? fmtNum(gh) : '—'} · Store ${dl.store_configured ? (store != null ? fmtNum(store) + ' (90d)' : '…') : 'not linked'}`),
    kpi('Machine', `${(sys.cpu || 0).toFixed(0)}% CPU`, `RAM ${fmtBytes(sys.mem_used)} / ${fmtBytes(sys.mem_total)}${sys.temp_c ? ` · ${sys.temp_c.toFixed(0)}°C` : ''}`),
  );
  if (naOn()) {
    tiles.push(kpi('AWS data out', ov.na_system ? `${fmtBytes(nsys.egress_month)}` : '—',
      ov.na_system ? `${nsys.egress_month_name} · ${((nsys.egress_month || 0) / FREE_EGRESS * 100).toFixed(0)}% of the free tier` : 'NA machine not reachable'));
  }
  $('#kpis').classList.toggle('seven', tiles.length === 7);
  $('#kpis').innerHTML = tiles.join('');
}

function renderTunnels() {
  const el = $('#tunnels');
  const rs = relays();
  const down = rs.filter((x) => !x.r);
  $('#ports-line').textContent = rs.filter((x) => x.r).map(({ label, r }) =>
    `${naOn() ? label + ': ' : ''}ports ${r.ports.min}–${r.ports.max} · ${r.ports.used} in use, ${r.ports.max - r.ports.min + 1 - r.ports.used} free`).join(' · ');
  const warn = down.map((x) => `<tr><td class="empty" colspan="6">Can't reach the ${naOn() ? esc(x.label) + ' ' : ''}relay's operator API: ${esc(x.err)}</td></tr>`).join('');
  const rows = rs.filter((x) => x.r).flatMap((x) => x.r.tunnels.map((t) => ({ t, region: x.id, host: x.r.public_host })));
  if (!rows.length) { el.innerHTML = warn + (down.length === rs.length ? '' : '<tr><td class="empty">No servers are public right now.</td></tr>'); return; }
  el.innerHTML = `<thead><tr><th>Server</th><th>Host IP</th><th class="r">Online for</th><th class="r">Players</th><th class="r">Traffic in / out</th><th class="r">Actions</th></tr></thead><tbody>` + warn +
    rows.map(({ t, region, host }) => {
      const addr = t.name ? `${t.name}.${host.split('.').slice(1).join('.')}` : `${host}:${t.port}`;
      const title = t.listing?.title;
      const players = t.players.length + t.udp_sessions;
      const key = `p:${region}:${t.id}`;
      const list = t.players.length
        ? `<details class="players" data-key="${esc(key)}" ${openDetails.has(key) ? 'open' : ''}><summary class="num">${players}</summary><ul class="mono">${t.players.map((p) => `<li>${esc(p)}</li>`).join('')}</ul></details>`
        : `<span class="num">${players}</span>`;
      return `<tr>
        <td><div><b>${esc(title || addr)}</b>${regionTag(region)}${t.udp ? '<span class="tag">Bedrock</span>' : ''}${t.listing ? '<span class="tag">Listed</span>' : ''}${t.hidden ? '<span class="tag hidden-tag">Hidden</span>' : ''}${t.supporter ? '<span class="tag supporter-tag">Supporter</span>' : ''}</div>
            <div class="muted mono">${esc(title ? addr : `port ${t.port}`)} · id ${esc(t.id)}</div></td>
        <td class="mono">${esc(t.source_ip)}</td>
        <td class="r">${esc(fmtDur((Date.now() - new Date(t.since)) / 1000))}</td>
        <td class="r">${list}${t.udp_sessions ? `<div class="muted">${t.udp_sessions} Bedrock</div>` : ''}<div class="muted">peak ${t.peak_players}</div></td>
        <td class="r num">${fmtBytes(t.bytes_in)} / ${fmtBytes(t.bytes_out)}</td>
        <td><div class="actions">
          ${t.listing ? actBtn(t.hidden ? 'unhide' : 'hide', t.id, t.hidden ? 'Unhide' : 'Hide listing', { region }) : ''}
          ${actBtn(t.supporter ? 'unsupporter' : 'supporter', t.id, t.supporter ? 'Remove supporter' : 'Make supporter', { region })}
          ${t.supporter ? grantBtn(t.id, region) : ''}
          ${t.name ? actBtn('release_name', t.id, 'Release address', { danger: true, region }) : ''}
          ${actBtn('kick', t.id, 'Kick', { danger: true, region })}
          ${actBtn('ban', t.id, 'Block', { danger: true, region })}
        </div></td></tr>`;
    }).join('') + '</tbody>';
}

function topList(title, obj, n = 8) {
  const rows = Object.entries(obj || {}).sort((a, b) => b[1] - a[1]).slice(0, n);
  const max = rows[0]?.[1] || 1;
  return `<div class="toplist"><h3>${esc(title)}</h3>${rows.length ? `<ol>${rows.map(([k, v]) => `<li><i class="bar" style="width:${(v / max) * 100}%"></i><span title="${esc(k)}">${esc(k)}</span><b>${fmtNum(v)}</b></li>`).join('')}</ol>` : '<div class="muted">Nothing yet</div>'}</div>`;
}

function sumMaps(web, key, days) {
  const out = {};
  for (const d of Object.keys(web?.days || {}).sort().slice(-days)) {
    for (const [k, v] of Object.entries(web.days[d][key] || {})) out[k] = (out[k] || 0) + v;
  }
  return out;
}

const COUNTRY = (() => { try { return new Intl.DisplayNames(['en'], { type: 'region' }); } catch { return null; } })();

function renderWeb() {
  const web = ov.web, el = $('#web');
  if (!web) { el.innerHTML = `<div class="empty">Can't reach the website's stats: ${esc(ov.web_err)}</div>`; return; }
  const today = web.days[web.today] || { views: 0, visitors: 0 };
  const w7 = webTotals(web, 7), w30 = webTotals(web, 30);
  const days = [];
  for (let i = 29; i >= 0; i--) {
    const d = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10);
    days.push([d, web.days[d]?.visitors || 0]);
  }
  const countries = sumMaps(web, 'countries', 7);
  const named = {};
  for (const [k, v] of Object.entries(countries)) named[(COUNTRY && /^[A-Z]{2}$/.test(k) && k !== 'XX' && k !== 'T1' ? COUNTRY.of(k) : k)] = v;
  const crawlers = Object.entries(sumMaps(web, 'crawlers', 7)).sort((a, b) => b[1] - a[1]);
  el.innerHTML = `
    <div class="stat-row">
      <div class="stat"><div class="v">${fmtNum(web.live)}</div><div class="l">On the site now</div></div>
      <div class="stat"><div class="v">${fmtNum(today.visitors)}</div><div class="l">Visitors today</div></div>
      <div class="stat"><div class="v">${fmtNum(w7.visitors)}</div><div class="l">Visitors, 7 days</div></div>
      <div class="stat"><div class="v">${fmtNum(w30.views)}</div><div class="l">Page loads, 30 days</div></div>
    </div>
    <figure class="chart-card"><figcaption>Visitors per day</figcaption><div class="chart" id="ch-web" style="height:150px"></div></figure>
    <div class="toplists">
      ${topList('Top pages · 7d', sumMaps(web, 'paths', 7))}
      ${topList('Came from · 7d', sumMaps(web, 'referrers', 7))}
      ${topList('Countries · 7d', named)}
    </div>
    <div class="muted">Search engines and bots, 7 days: ${crawlers.length ? crawlers.map(([k, v]) => `${esc(k)} ${fmtNum(v)}`).join(' · ') : 'none yet'}</div>`;
  barChart($('#ch-web'), days, 'Visitors');
}

function renderDownloads() {
  const dl = ov.downloads || {}, el = $('#downloads');
  const gh = dl.github;
  let html = '';
  html += `<div class="stat-row" style="grid-template-columns:repeat(2,minmax(0,1fr))">
    <div class="stat"><div class="v">${gh ? fmtNum(gh.total) : '—'}</div><div class="l">GitHub downloads, all time</div></div>
    <div class="stat"><div class="v">${dl.store?.total_90d != null ? fmtNum(dl.store.total_90d) : '—'}</div><div class="l">Microsoft Store, 90 days</div></div>
  </div>`;
  if (dl.github_err) html += `<div class="muted">GitHub: ${esc(dl.github_err)}</div>`;
  if (gh?.releases?.length) {
    html += `<div class="table-wrap"><table class="table"><thead><tr><th>Release</th><th>Published</th><th class="r">Downloads</th></tr></thead><tbody>${
      gh.releases.slice(0, 5).map((r) => `<tr><td><b>${esc(r.tag)}</b><div class="muted">${r.assets.map((a) => `${esc(a.name)} ${fmtNum(a.count)}`).join(' · ')}</div></td><td>${esc(new Date(r.published).toLocaleDateString())}</td><td class="r">${fmtNum(r.count)}</td></tr>`).join('')
    }</tbody></table></div>`;
  }
  if (dl.store_configured) {
    if (dl.store_err) html += `<div class="muted">Store: ${esc(dl.store_err)}</div>`;
    else if (dl.store?.days) {
      const days = Object.entries(dl.store.days).sort().slice(-30);
      html += `<figure class="chart-card"><figcaption>Store installs per day <span class="muted">(Microsoft reports with a 1–2 day delay)</span></figcaption><div class="chart" id="ch-store" style="height:130px"></div></figure>`;
      el.innerHTML = html;
      barChart($('#ch-store'), days, 'Installs');
      return;
    }
  } else {
    html += `<div class="setup"><b>Microsoft Store installs aren't linked yet.</b> Partner Center → Account settings → User management → <b>Azure AD applications</b> → create an app with the <b>Manager</b> role and a key, then add <span class="mono">STORE_TENANT_ID</span>, <span class="mono">STORE_CLIENT_ID</span> and <span class="mono">STORE_CLIENT_SECRET</span> to <span class="mono">/etc/voxelport/admin.env</span> and restart voxelport-admin. Until then, see installs in <a href="https://partner.microsoft.com/dashboard/insights/analytics/overview" target="_blank" rel="noreferrer" style="color:inherit">Partner Center</a>.</div>`;
  }
  el.innerHTML = html;
}

function renderNames() {
  const el = $('#names');
  const rs = relays().filter((x) => x.r && x.r.names_enabled);
  if (!rs.length) {
    el.innerHTML = relays().some((x) => x.r) ? '<tr><td class="empty">Custom addresses are off (no Cloudflare token).</td></tr>' : '';
    return;
  }
  const names = rs.flatMap((x) => x.r.names.map((n) => ({ ...n, region: x.id })));
  $('#names-count').textContent = `${names.length} claimed${naOn() ? ` · ${rs.map((x) => `${x.label} ${x.r.names.length}`).join(' · ')}` : ''}`;
  if (!names.length) { el.innerHTML = '<tr><td class="empty">Nobody has claimed an address yet.</td></tr>'; return; }
  el.innerHTML = `<thead><tr><th>Address</th><th class="r">Port</th><th>Last used</th><th class="r"></th></tr></thead><tbody>` +
    names.map((n) => `<tr>
      <td><span class="dot ${n.online ? 'good' : ''}" style="display:inline-block;margin-right:6px"></span><b>${esc(n.name)}</b>${regionTag(n.region)}${n.supporter ? '<span class="tag supporter-tag">Supporter</span>' : ''}<div class="muted mono">id ${esc(n.id)} · since ${esc(new Date(n.created).toLocaleDateString())}</div></td>
      <td class="r num">${n.port}</td>
      <td>${n.online ? 'Online now' : esc(ago(n.last_seen))}${n.supporter ? '<div class="muted">kept: never expires</div>' : ''}</td>
      <td><div class="actions">
        ${actBtn(n.supporter ? 'unsupporter' : 'supporter', n.id, n.supporter ? 'Remove supporter' : 'Make supporter', { region: n.region })}
        ${n.supporter ? grantBtn(n.id, n.region) : ''}
        ${actBtn('release_name', n.id, 'Release', { danger: true, region: n.region })}</div></td></tr>`).join('') + '</tbody>';
}

function meter(label, used, total, text) {
  const pct = total ? Math.min(100, (used / total) * 100) : 0;
  const cls = pct > 90 ? 'bad' : pct > 75 ? 'warn' : '';
  return `<div class="stat"><div class="l">${esc(label)}</div><div class="v" style="font-size:17px">${esc(text)}</div><div class="meter ${cls}"><i style="width:${pct.toFixed(1)}%"></i></div></div>`;
}

function renderSystem() {
  const s = ov.system || {}, el = $('#system');
  $('#hostname').textContent = s.hostname || '';
  $('#iface-name').textContent = s.iface ? `· ${s.iface}` : '';
  const diskUsed = (s.disk_total || 0) - (s.disk_free || 0);
  el.innerHTML = `<div class="sys-grid">
    ${meter('CPU', s.cpu || 0, 100, `${(s.cpu || 0).toFixed(0)}% of ${s.cores || '?'} cores`)}
    ${meter('Memory', s.mem_used, s.mem_total, `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)}`)}
    ${meter('Disk /', diskUsed, s.disk_total, `${fmtBytes(diskUsed)} / ${fmtBytes(s.disk_total)}`)}
    <div class="stat"><div class="l">Load (1 / 5 / 15 min)</div><div class="v num" style="font-size:17px">${(s.load || []).map((v) => v.toFixed(2)).join(' · ')}</div></div>
    <div class="stat"><div class="l">Up for</div><div class="v" style="font-size:17px">${esc(fmtDur(s.uptime || 0))}</div></div>
    <div class="stat"><div class="l">Temperature</div><div class="v" style="font-size:17px">${s.temp_c ? `${s.temp_c.toFixed(0)}°C` : '—'}</div></div>
    <div class="stat"><div class="l">Internet now (down / up)</div><div class="v num" style="font-size:17px">${fmtRate(s.net_rx)} / ${fmtRate(s.net_tx)}</div></div>
    <div class="stat"><div class="l">Since boot (down / up)</div><div class="v num" style="font-size:17px">${fmtBytes(s.net_rx_total)} / ${fmtBytes(s.net_tx_total)}</div></div>
  </div>`;
}

// The North America machine (AWS): health and this month's data out, which
// AWS bills beyond the free 100 GB.
function renderNA() {
  const sec = $('#na-block');
  sec.hidden = !naOn();
  if (!naOn()) return;
  const s = ov.na_system, el = $('#na-system');
  $('#na-hostname').textContent = s?.hostname ? `${s.hostname} · AWS us-east-1` : 'AWS us-east-1';
  if (!s) { el.innerHTML = `<div class="empty">Can't reach the NA machine: ${esc(ov.na_system_err || ov.relay_na_err)}</div>`; return; }
  const memUsed = (s.mem_total || 0) - (s.mem_avail || 0);
  const diskUsed = (s.disk_total || 0) - (s.disk_free || 0);
  const egress = s.egress_month || 0;
  el.innerHTML = `<div class="sys-grid">
    ${meter(`AWS data out · ${s.egress_month_name}`, egress, FREE_EGRESS, `${fmtBytes(egress)} of 100 GB free`)}
    ${meter('CPU', s.cpu || 0, 100, `${(s.cpu || 0).toFixed(0)}% of ${s.cores || '?'} cores`)}
    ${meter('Memory', memUsed, s.mem_total, `${fmtBytes(memUsed)} / ${fmtBytes(s.mem_total)}`)}
    ${meter('Disk /', diskUsed, s.disk_total, `${fmtBytes(diskUsed)} / ${fmtBytes(s.disk_total)}`)}
    <div class="stat"><div class="l">Load (1 / 5 / 15 min)</div><div class="v num" style="font-size:17px">${(s.load || []).map((v) => v.toFixed(2)).join(' · ')}</div></div>
    <div class="stat"><div class="l">Up for</div><div class="v" style="font-size:17px">${esc(fmtDur((s.uptime || [])[0] || 0))}</div></div>
    <div class="stat"><div class="l">Internet now (down / up)</div><div class="v num" style="font-size:17px">${fmtRate(s.net_rx_rate)} / ${fmtRate(s.net_tx_rate)}</div></div>
    <div class="stat"><div class="l">Since boot (down / up)</div><div class="v num" style="font-size:17px">${fmtBytes(s.net_rx)} / ${fmtBytes(s.net_tx)}</div></div>
  </div>
  <div class="muted">Data out is counted from this dashboard's samples (it misses time the dashboard was down). AWS charges about $0.09 per GB over 100 GB a month; check Billing for the exact figure.</div>`;
}

function renderModeration() {
  const el = $('#moderation');
  const rs = relays().filter((x) => x.r);
  if (!rs.length) { el.innerHTML = ''; return; }
  // Blocks and hidden entries are sent to every relay; show them once.
  const union = (k) => [...new Set(rs.flatMap((x) => x.r[k] || []))];
  const list = (title, items, action, label, empty) => `<div class="mod-list"><h3>${esc(title)}</h3>${items.length
    ? items.map((e) => `<div class="mod-item"><span class="mono">${esc(e)}</span>${actBtn(action, e, label)}</div>`).join('')
    : `<div class="muted">${esc(empty)}</div>`}</div>`;
  el.innerHTML = list('Supporters (badge, pinned, address kept)', union('supporters'), 'unsupporter', 'Remove', 'No supporters yet. Use "Make supporter" on a server or address above.') +
    list('Hidden from the server list', union('hidden'), 'unhide', 'Unhide', 'Nothing hidden.') +
    list('Blocked apps', union('banned'), 'unban', 'Unblock', 'Nobody is blocked.');
}

function renderAll() {
  if (!ov) return;
  renderServices(ov.services);
  renderKPIs();
  renderTunnels();
  renderWeb();
  renderDownloads();
  renderNames();
  renderSystem();
  renderNA();
  renderModeration();
  document.querySelectorAll('.na-only').forEach((e) => { e.hidden = !naOn(); });
}

// ─── Saved data ─────────────────────────────────────────────────────────────
const EVENT_TEXT = {
  server_online: (e) => `Server online · ${e.name || 'port ' + e.port}${e.bedrock ? ' · Bedrock' : ''}`,
  server_offline: (e) => `Server offline · ${e.name || 'port ' + e.port} · after ${fmtDur(e.duration_s)}`,
  address_claimed: (e) => `Address claimed · ${e.name}`,
  address_released: (e) => `Address released · ${e.name}`,
  service: (e) => `${e.service}: ${e.from} → ${e.to}`,
  admin_action: (e) => `You: ${e.action} ${e.id || e.name || ''}`,
  relay_api_down: (e) => `${e.region === 'na' ? 'NA relay' : 'Relay'} operator API unreachable`,
  relay_api_back: (e) => `${e.region === 'na' ? 'NA relay' : 'Relay'} operator API back`,
  web_stats_down: () => 'Website stats unreachable',
  web_stats_back: () => 'Website stats back',
};

async function loadSaved() {
  let d;
  try { d = await api('/api/saved'); } catch { return; }
  const u = d.usage || {};
  $('#saved-usage').textContent = u.since ? `Recording since ${u.since} · ${fmtBytes(u.bytes)} in ${fmtNum(u.files)} files · nothing is ever deleted` : 'Recording started';
  const s = d.sessions || [];
  $('#sessions').innerHTML = s.length
    ? `<thead><tr><th>Server</th><th>Ended</th><th class="r">Lasted</th><th class="r">Players</th><th class="r">Traffic</th></tr></thead><tbody>${s.map((x) => `<tr>
        <td><b>${esc(x.title || x.address)}</b>${x.region === 'na' ? '<span class="tag region-na">NA</span>' : ''}${x.bedrock ? '<span class="tag">Bedrock</span>' : ''}<div class="muted mono">${esc(x.title ? x.address : 'id ' + x.id)}</div></td>
        <td>${esc(new Date(x.end).toLocaleString([], { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }))}</td>
        <td class="r">${esc(fmtDur(x.duration_s))}</td>
        <td class="r num">${x.unique_players} <span class="muted">· peak ${x.peak_players}</span></td>
        <td class="r num">${fmtBytes(x.bytes_in + x.bytes_out)}</td></tr>`).join('')}</tbody>`
    : '<tr><td class="empty">Sessions appear here when a server goes offline.</td></tr>';
  const ev = d.events || [];
  $('#events').innerHTML = ev.length
    ? `<tbody>${ev.map((e) => `<tr><td class="muted" style="white-space:nowrap">${esc(new Date(e.t).toLocaleString([], { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }))}</td><td>${esc((EVENT_TEXT[e.type] || (() => e.type))(e))}${e.region === 'na' && !/^relay_api/.test(e.type) ? ' <span class="tag region-na">NA</span>' : ''}</td></tr>`).join('')}</tbody>`
    : '<tr><td class="empty">Nothing has happened yet.</td></tr>';
}

(() => {
  const now = new Date();
  $('#ex-to').value = localDay(now);
  $('#ex-from').value = localDay(new Date(now.getTime() - 6 * 86400e3));
  $('#export-form').addEventListener('submit', (e) => {
    e.preventDefault();
    const p = new URLSearchParams({ what: $('#ex-what').value, from: $('#ex-from').value, to: $('#ex-to').value, format: $('#ex-format').value });
    location.href = `/api/export?${p}`;
  });
})();

// ─── History charts ─────────────────────────────────────────────────────────
let hist = [];
// Chart definitions: [element, series, y format, options]. With a North
// America relay, the players, relay and CPU charts compare the two regions
// (India in series 1, NA in series 2) instead of splitting one relay.
function chartDefs() {
  const n = (v) => (v >= 10 ? v.toFixed(0) : v.toFixed(v ? 1 : 0));
  const pct = (v) => `${v.toFixed(0)}%`;
  const z = (v) => v || 0;
  const two = naOn();
  return [
    ['#ch-players', two ? [
      { label: 'Players · India', color: S1, get: (p) => z(p.players), fmt: n },
      { label: 'Players · NA', color: S2, get: (p) => z(p.np), fmt: n },
    ] : [
      { label: 'Players', color: S1, get: (p) => p.players, fmt: n },
      { label: 'Servers', color: S2, get: (p) => p.tunnels, fmt: n },
    ], n, { minMax: 4 }],
    ['#ch-servers', [
      { label: 'Servers · India', color: S1, get: (p) => z(p.tunnels), fmt: n },
      { label: 'Servers · NA', color: S2, get: (p) => z(p.nt), fmt: n },
    ], n, { minMax: 4 }],
    ['#ch-relay', two ? [
      { label: 'India relay', color: S1, get: (p) => z(p.rin) + z(p.rout), fmt: fmtRate },
      { label: 'NA relay', color: S2, get: (p) => z(p.nin) + z(p.nout), fmt: fmtRate },
    ] : [
      { label: 'Players → servers', color: S1, get: (p) => p.rin, fmt: fmtRate },
      { label: 'Servers → players', color: S2, get: (p) => p.rout, fmt: fmtRate },
    ], fmtRate, {}],
    ['#ch-net', [
      { label: 'Download', color: S1, get: (p) => p.rx, fmt: fmtRate },
      { label: 'Upload', color: S2, get: (p) => p.tx, fmt: fmtRate },
    ], fmtRate, {}],
    ['#ch-nanet', [
      { label: 'Download', color: S1, get: (p) => z(p.nrx), fmt: fmtRate },
      { label: 'Upload (billed)', color: S2, get: (p) => z(p.ntx), fmt: fmtRate },
    ], fmtRate, {}],
    ['#ch-cpu', two ? [
      { label: 'Home', color: S1, get: (p) => z(p.cpu), fmt: pct },
      { label: 'NA', color: S2, get: (p) => z(p.ncpu), fmt: pct },
    ] : [{ label: 'CPU', color: BRAND, get: (p) => p.cpu, fmt: pct }], pct, { max: 100 }],
  ];
}

function renderCharts() {
  const legend = (series) => (series.length > 1
    ? `<span class="legend">${series.map((s) => `<span><i style="background:${s.color}"></i>${esc(s.label)}</span>`).join('')}</span>` : '');
  for (const [id, series, fmt, opts] of chartDefs()) {
    const el = $(id);
    if (!el || el.closest('[hidden]')) continue;
    const cap = el.previousElementSibling;
    cap.querySelector('.legend')?.remove();
    cap.insertAdjacentHTML('beforeend', legend(series));
    lineChart(el, hist, series, fmt, opts);
  }
}

async function loadHistory() {
  try {
    const h = await api(`/api/history?range=${range}`);
    hist = h.points || [];
    renderCharts();
  } catch { /* shown via the overview error */ }
}

document.querySelectorAll('.seg button').forEach((b) => b.addEventListener('click', () => {
  range = b.dataset.range;
  document.querySelectorAll('.seg button').forEach((x) => x.setAttribute('aria-pressed', String(x === b)));
  loadHistory();
}));

async function refresh() {
  try {
    const hadNA = naOn();
    ov = await api('/api/overview');
    lastOK = Date.now();
    $('#alert').hidden = true;
    renderAll();
    if (naOn() !== hadNA) renderCharts(); // the first overview tells us whether there's an NA relay
  } catch (e) {
    $('#alert').hidden = false;
    $('#alert').textContent = `Couldn't refresh: ${e.message}`;
  }
}

setInterval(() => {
  $('#updated').textContent = lastOK ? `Updated ${Math.round((Date.now() - lastOK) / 1000)}s ago` : 'Loading…';
}, 1000);
let resizeTimer;
window.addEventListener('resize', () => { clearTimeout(resizeTimer); resizeTimer = setTimeout(() => { renderCharts(); if (ov) { renderWeb(); renderDownloads(); } }, 150); });

refresh();
loadHistory();
setInterval(refresh, 5000);
setInterval(() => { if (range === '1h' || range === '24h') loadHistory(); }, 15000);
loadSaved();
setInterval(loadSaved, 30000);
