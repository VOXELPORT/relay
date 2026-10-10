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

const ACTION_TEXT = { kick: 'Kicked', ban: 'Blocked', unban: 'Unblocked', hide: 'Hidden from the server list', unhide: 'Back on the server list', release_name: 'Address released' };

async function act(action, id) {
  const key = `${action}:${id}`;
  const exp = armed.get(key);
  if (!exp || exp < Date.now()) {
    armed.set(key, Date.now() + 4000);
    renderAll();
    setTimeout(renderAll, 4100);
    return;
  }
  armed.delete(key);
  try {
    await api('/api/action', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-VP-Admin': '1' }, body: JSON.stringify(/^[0-9a-f]{8,}$/.test(id) ? { action, id } : { action, name: id }) });
    toast(ACTION_TEXT[action] || 'Done');
  } catch (e) {
    toast(e.message, true);
  }
  refresh();
}

function actBtn(action, id, label, opts = {}) {
  const key = `${action}:${id}`;
  const isArmed = (armed.get(key) || 0) > Date.now();
  return `<button type="button" class="btn sm ${opts.danger ? 'danger' : ''} ${isArmed ? 'armed' : ''}" data-act="${esc(action)}" data-id="${esc(id)}">${isArmed ? `Confirm ${esc(label.toLowerCase())}?` : esc(label)}</button>`;
}

document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-act]');
  if (b) act(b.dataset.act, b.dataset.id);
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
  $('#services').innerHTML = Object.entries(services || {}).map(([k, v]) => {
    const cls = v === 'active' ? 'good' : v === 'failed' ? 'bad' : 'warn';
    return `<span class="pill" title="${esc(k)}: ${esc(v)}"><span class="dot ${cls}"></span>${esc(names[k] || k)}${v === 'active' ? '' : ` · ${esc(v)}`}</span>`;
  }).join('');
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
  $('#kpis').innerHTML = [
    kpi('Servers online', r ? fmtNum(t.tunnels) : '—', r ? `${fmtNum(t.tunnels_served)} sessions since relay start` : 'Relay not reachable'),
    kpi('Players now', r ? fmtNum(t.players) : '—', r ? `${fmtNum(t.conns)} connections since start` : ''),
    kpi('Relay traffic now', fmtRate((sys.relay_in || 0) + (sys.relay_out || 0)), r ? `${fmtBytes((t.bytes_in || 0) + (t.bytes_out || 0))} since start` : ''),
    kpi('Website visitors now', web ? fmtNum(web.live) : '—', web ? `${fmtNum(today.visitors)} today · ${fmtNum(today.views)} page loads` : 'Stats not reachable'),
    kpi('App downloads', gh != null ? fmtNum(gh + (store || 0)) : '—', `GitHub ${gh != null ? fmtNum(gh) : '—'} · Store ${dl.store_configured ? (store != null ? fmtNum(store) + ' (90d)' : '…') : 'not linked'}`),
    kpi('Machine', `${(sys.cpu || 0).toFixed(0)}% CPU`, `RAM ${fmtBytes(sys.mem_used)} / ${fmtBytes(sys.mem_total)}${sys.temp_c ? ` · ${sys.temp_c.toFixed(0)}°C` : ''}`),
  ].join('');
}

function renderTunnels() {
  const r = ov.relay;
  const el = $('#tunnels');
  if (!r) { el.innerHTML = `<tr><td class="empty">Can't reach the relay's operator API: ${esc(ov.relay_err)}</td></tr>`; return; }
  $('#ports-line').textContent = `Ports ${r.ports.min}–${r.ports.max} · ${r.ports.used} in use, ${r.ports.max - r.ports.min + 1 - r.ports.used} free`;
  if (!r.tunnels.length) { el.innerHTML = '<tr><td class="empty">No servers are public right now.</td></tr>'; return; }
  const host = r.public_host;
  el.innerHTML = `<thead><tr><th>Server</th><th>Host IP</th><th class="r">Online for</th><th class="r">Players</th><th class="r">Traffic in / out</th><th class="r">Actions</th></tr></thead><tbody>` +
    r.tunnels.map((t) => {
      const addr = t.name ? `${t.name}.${host.split('.').slice(1).join('.')}` : `${host}:${t.port}`;
      const title = t.listing?.title;
      const players = t.players.length + t.udp_sessions;
      const key = `p:${t.id}`;
      const list = t.players.length
        ? `<details class="players" data-key="${esc(key)}" ${openDetails.has(key) ? 'open' : ''}><summary class="num">${players}</summary><ul class="mono">${t.players.map((p) => `<li>${esc(p)}</li>`).join('')}</ul></details>`
        : `<span class="num">${players}</span>`;
      return `<tr>
        <td><div><b>${esc(title || addr)}</b>${t.udp ? '<span class="tag">Bedrock</span>' : ''}${t.listing ? '<span class="tag">Listed</span>' : ''}${t.hidden ? '<span class="tag hidden-tag">Hidden</span>' : ''}</div>
            <div class="muted mono">${esc(title ? addr : `port ${t.port}`)} · id ${esc(t.id)}</div></td>
        <td class="mono">${esc(t.source_ip)}</td>
        <td class="r">${esc(fmtDur((Date.now() - new Date(t.since)) / 1000))}</td>
        <td class="r">${list}${t.udp_sessions ? `<div class="muted">${t.udp_sessions} Bedrock</div>` : ''}<div class="muted">peak ${t.peak_players}</div></td>
        <td class="r num">${fmtBytes(t.bytes_in)} / ${fmtBytes(t.bytes_out)}</td>
        <td><div class="actions">
          ${t.listing ? actBtn(t.hidden ? 'unhide' : 'hide', t.id, t.hidden ? 'Unhide' : 'Hide listing') : ''}
          ${t.name ? actBtn('release_name', t.id, 'Release address', { danger: true }) : ''}
          ${actBtn('kick', t.id, 'Kick', { danger: true })}
          ${actBtn('ban', t.id, 'Block', { danger: true })}
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
  const r = ov.relay, el = $('#names');
  if (!r) { el.innerHTML = ''; return; }
  if (!r.names_enabled) { el.innerHTML = '<tr><td class="empty">Custom addresses are off (no Cloudflare token).</td></tr>'; return; }
  $('#names-count').textContent = `${r.names.length} claimed`;
  if (!r.names.length) { el.innerHTML = '<tr><td class="empty">Nobody has claimed an address yet.</td></tr>'; return; }
  el.innerHTML = `<thead><tr><th>Address</th><th class="r">Port</th><th>Last used</th><th class="r"></th></tr></thead><tbody>` +
    r.names.map((n) => `<tr>
      <td><span class="dot ${n.online ? 'good' : ''}" style="display:inline-block;margin-right:6px"></span><b>${esc(n.name)}</b><div class="muted mono">id ${esc(n.id)} · since ${esc(new Date(n.created).toLocaleDateString())}</div></td>
      <td class="r num">${n.port}</td>
      <td>${n.online ? 'Online now' : esc(ago(n.last_seen))}</td>
      <td><div class="actions">${actBtn('release_name', n.id, 'Release', { danger: true })}</div></td></tr>`).join('') + '</tbody>';
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

function renderModeration() {
  const r = ov.relay, el = $('#moderation');
  if (!r) { el.innerHTML = ''; return; }
  const list = (title, items, action, label, empty) => `<div class="mod-list"><h3>${esc(title)}</h3>${items.length
    ? items.map((e) => `<div class="mod-item"><span class="mono">${esc(e)}</span>${actBtn(action, e, label)}</div>`).join('')
    : `<div class="muted">${esc(empty)}</div>`}</div>`;
  el.innerHTML = list('Hidden from the server list', r.hidden, 'unhide', 'Unhide', 'Nothing hidden.') +
    list('Blocked apps', r.banned, 'unban', 'Unblock', 'Nobody is blocked.');
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
  renderModeration();
}

// ─── Saved data ─────────────────────────────────────────────────────────────
const EVENT_TEXT = {
  server_online: (e) => `Server online · ${e.name || 'port ' + e.port}${e.bedrock ? ' · Bedrock' : ''}`,
  server_offline: (e) => `Server offline · ${e.name || 'port ' + e.port} · after ${fmtDur(e.duration_s)}`,
  address_claimed: (e) => `Address claimed · ${e.name}`,
  address_released: (e) => `Address released · ${e.name}`,
  service: (e) => `${e.service}: ${e.from} → ${e.to}`,
  admin_action: (e) => `You: ${e.action} ${e.id || e.name || ''}`,
  relay_api_down: () => 'Relay operator API unreachable',
  relay_api_back: () => 'Relay operator API back',
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
        <td><b>${esc(x.title || x.address)}</b>${x.bedrock ? '<span class="tag">Bedrock</span>' : ''}<div class="muted mono">${esc(x.title ? x.address : 'id ' + x.id)}</div></td>
        <td>${esc(new Date(x.end).toLocaleString([], { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }))}</td>
        <td class="r">${esc(fmtDur(x.duration_s))}</td>
        <td class="r num">${x.unique_players} <span class="muted">· peak ${x.peak_players}</span></td>
        <td class="r num">${fmtBytes(x.bytes_in + x.bytes_out)}</td></tr>`).join('')}</tbody>`
    : '<tr><td class="empty">Sessions appear here when a server goes offline.</td></tr>';
  const ev = d.events || [];
  $('#events').innerHTML = ev.length
    ? `<tbody>${ev.map((e) => `<tr><td class="muted" style="white-space:nowrap">${esc(new Date(e.t).toLocaleString([], { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }))}</td><td>${esc((EVENT_TEXT[e.type] || (() => e.type))(e))}</td></tr>`).join('')}</tbody>`
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
function renderCharts() {
  const n = (v) => (v >= 10 ? v.toFixed(0) : v.toFixed(v ? 1 : 0));
  lineChart($('#ch-players'), hist, [
    { key: 'players', label: 'Players', color: S1, get: (p) => p.players, fmt: n },
    { key: 'tunnels', label: 'Servers', color: S2, get: (p) => p.tunnels, fmt: n },
  ], (v) => n(v), { minMax: 4 });
  lineChart($('#ch-relay'), hist, [
    { label: 'Players → servers', color: S1, get: (p) => p.rin, fmt: fmtRate },
    { label: 'Servers → players', color: S2, get: (p) => p.rout, fmt: fmtRate },
  ], fmtRate);
  lineChart($('#ch-net'), hist, [
    { label: 'Download', color: S1, get: (p) => p.rx, fmt: fmtRate },
    { label: 'Upload', color: S2, get: (p) => p.tx, fmt: fmtRate },
  ], fmtRate);
  lineChart($('#ch-cpu'), hist, [{ label: 'CPU', color: BRAND, get: (p) => p.cpu, fmt: (v) => `${v.toFixed(0)}%` }], (v) => `${v.toFixed(0)}%`, { max: 100 });
}

function addLegends() {
  const legend = (items) => `<span class="legend">${items.map(([c, l]) => `<span><i style="background:${c}"></i>${esc(l)}</span>`).join('')}</span>`;
  const set = (id, items) => { const cap = $(id).previousElementSibling; cap.insertAdjacentHTML('beforeend', legend(items)); };
  set('#ch-players', [[S1, 'Players'], [S2, 'Servers']]);
  set('#ch-relay', [[S1, 'Players → servers'], [S2, 'Servers → players']]);
  set('#ch-net', [[S1, 'Download'], [S2, 'Upload']]);
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
    ov = await api('/api/overview');
    lastOK = Date.now();
    $('#alert').hidden = true;
    renderAll();
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

addLegends();
refresh();
loadHistory();
setInterval(refresh, 5000);
setInterval(() => { if (range === '1h' || range === '24h') loadHistory(); }, 15000);
loadSaved();
setInterval(loadSaved, 30000);
