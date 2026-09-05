/* TopoLight console — vanilla JS, no framework, no external requests. */
(function () {
  'use strict';

  // ---------- tiny helpers ----------
  const $ = (s, el) => (el || document).querySelector(s);
  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    if (attrs) for (const k in attrs) {
      const v = attrs[k];
      if (k === 'class') el.className = v;
      else if (k === 'style') el.style.cssText = v;
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else if (k === 'html') el.innerHTML = v;
      else if (v !== null && v !== undefined && v !== false) el.setAttribute(k, v === true ? '' : v);
    }
    kids.flat().forEach(k => { if (k === null || k === undefined || k === false) return; el.append(k.nodeType ? k : document.createTextNode(String(k))); });
    return el;
  }
  // null-safe append: Element.append(null) would render the text "null"
  const _append = Element.prototype.append;
  Element.prototype.append = function (...kids) { return _append.apply(this, kids.flat().filter(k => k !== null && k !== undefined && k !== false)); };
  const esc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const fmtBps = v => { if (!v || v < 0) return '0 b/s'; const u = ['b/s', 'kb/s', 'Mb/s', 'Gb/s', 'Tb/s']; let i = 0; while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; } return (v >= 100 ? v.toFixed(0) : v.toFixed(1)) + ' ' + u[i]; };
  const fmtPps = v => { if (!v || v < 0) return '0 pps'; const u = ['pps', 'kpps', 'Mpps', 'Gpps']; let i = 0; while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; } return (v >= 100 ? v.toFixed(0) : v.toFixed(1)) + ' ' + u[i]; };
  const fmtBytes = v => { if (!v) return '0 B'; const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0; while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; } return v.toFixed(i ? 1 : 0) + ' ' + u[i]; };
  const fmtDur = s => { s = Math.max(0, Math.floor(s)); if (s < 60) return s + 's'; if (s < 3600) return Math.floor(s / 60) + 'm ' + (s % 60) + 's'; if (s < 86400) return Math.floor(s / 3600) + 'h ' + Math.floor(s % 3600 / 60) + 'm'; return Math.floor(s / 86400) + 'd ' + Math.floor(s % 86400 / 3600) + 'h'; };
  const ago = t => { if (!t) return '—'; const d = (Date.now() - new Date(t).getTime()) / 1000; if (d < 0) return 'now'; if (d < 60) return Math.floor(d) + 's ago'; if (d < 3600) return Math.floor(d / 60) + 'm ago'; if (d < 86400) return Math.floor(d / 3600) + 'h ago'; return Math.floor(d / 86400) + 'd ago'; };
  const clock = t => { const d = t ? new Date(t) : new Date(); return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }); };
  const dateTime = t => t ? new Date(t).toLocaleString([], { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' }) : '—';
  const isZero = t => !t || t.startsWith('0001');
  const cap = s => s ? s.charAt(0).toUpperCase() + s.slice(1) : '';

  async function api(method, path, body) {
    const r = await fetch(path, { method, headers: body ? { 'Content-Type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined, credentials: 'same-origin' });
    let data = null;
    try { data = await r.json(); } catch (e) { data = null; }
    if (r.status === 401) { if (data && data.error === 'setup required') { state.setup = true; } state.user = null; route(); throw new Error(data && data.error || 'sign in required'); }
    if (!r.ok) { const err = new Error(data && data.error || ('HTTP ' + r.status)); err.status = r.status; err.data = data; throw err; }
    return data;
  }
  const get = p => api('GET', p), post = (p, b) => api('POST', p, b || {}), put = (p, b) => api('PUT', p, b || {}), del = p => api('DELETE', p);

  function toast(msg, kind, ms) {
    const t = h('div', { class: 'toast ' + (kind || '') }, msg);
    $('#toasts').append(t);
    const keep = kind === 'critical' ? 15000 : (ms || 5000);
    setTimeout(() => t.remove(), keep);
    while ($('#toasts').children.length > 4) $('#toasts').firstChild.remove();
  }

  function modal(title, bodyEl, onClose) {
    const box = h('div', { class: 'box' }, h('h2', null, title), bodyEl);
    const m = h('div', { class: 'modal', onclick: e => { if (e.target === m) close(); } }, box);
    function close() { m.remove(); document.removeEventListener('keydown', esck); if (onClose) onClose(); }
    function esck(e) { if (e.key === 'Escape') close(); }
    document.addEventListener('keydown', esck);
    document.body.append(m);
    const f = box.querySelector('input,select,textarea,button'); if (f) f.focus();
    return { close, el: m };
  }

  const ICONS = {
    overview: '<rect x="3" y="3" width="8" height="8" rx="2"/><rect x="13" y="3" width="8" height="5" rx="2"/><rect x="13" y="11" width="8" height="10" rx="2"/><rect x="3" y="14" width="8" height="7" rx="2"/>',
    topology: '<circle cx="12" cy="5" r="2.5"/><circle cx="5" cy="19" r="2.5"/><circle cx="19" cy="19" r="2.5"/><path d="M12 7.5v4M12 11.5 6.5 17M12 11.5l5.5 5.5"/>',
    alerts: '<path d="M6 16V11a6 6 0 0 1 12 0v5l2 2H4z"/><path d="M10 20a2 2 0 0 0 4 0"/>',
    devices: '<rect x="3" y="4" width="18" height="6" rx="2"/><rect x="3" y="14" width="18" height="6" rx="2"/><path d="M7 7h.01M7 17h.01"/>',
    logs: '<path d="M5 4h14v16H5z"/><path d="M8 8h8M8 12h8M8 16h5"/>',
    flow: '<path d="M4 6h6l4 4h6"/><path d="M4 12h16"/><path d="M4 18h6l4-4h6"/>',
    endpoints: '<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>',
    probes: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
    wireless: '<path d="M2 9a14 14 0 0 1 20 0"/><path d="M5.5 12.5a9 9 0 0 1 13 0"/><path d="M9 16a4.5 4.5 0 0 1 6 0"/><circle cx="12" cy="19.5" r="1.2"/>',
    reports: '<path d="M6 3h9l4 4v14H6z"/><path d="M9 12h6M9 16h6M9 8h3"/>',
    admin: '<circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M4.9 19.1 7 17M17 7l2.1-2.1"/>',
    theme: '<path d="M12 3a9 9 0 1 0 9 9c-5 0-9-4-9-9z"/>',
    logout: '<path d="M10 17l5-5-5-5M15 12H3M21 3v18"/>',
  };
  const svg = (name, cls) => { const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg'); s.setAttribute('viewBox', '0 0 24 24'); if (cls) s.setAttribute('class', cls); s.innerHTML = ICONS[name] || ''; return s; };
  const LOGO = '<svg viewBox="0 0 96 96" aria-hidden="true"><polygon points="48,6 84,27 84,69 48,90 12,69 12,27" fill="none" stroke="currentColor" stroke-width="6.5" stroke-linejoin="round"/><rect x="29" y="29" width="17.5" height="17.5" rx="3.6" fill="currentColor"/><rect x="50" y="29" width="17.5" height="17.5" rx="3.6" fill="none" stroke="currentColor" stroke-width="4.2"/><rect x="29" y="50" width="17.5" height="17.5" rx="3.6" fill="none" stroke="currentColor" stroke-width="4.2"/><rect x="50" y="50" width="17.5" height="17.5" rx="3.6" fill="currentColor"/></svg>';

  // ---------- state & routing ----------
  const state = { user: null, setup: false, status: null, sites: [], page: null, sse: null, theme: null, siteFilter: '' };
  try { state.theme = localStorage.getItem('topolight-theme'); if (state.theme) document.documentElement.dataset.theme = state.theme; } catch (e) { }
  try { state.siteFilter = localStorage.getItem('topolight-site') || ''; } catch (e) { }

  function nav(hash) { location.hash = hash; }
  window.addEventListener('hashchange', route);

  async function boot() {
    try {
      state.status = await get('/api/status');
    } catch (e) { $('#app').innerHTML = '<div class="center"><div class="card"><h2>Cannot reach the console API</h2><p class="muted">' + esc(e.message) + '</p></div></div>'; return; }
    if (!state.status.setup_done) { state.setup = true; renderSetup(); return; }
    if (!state.status.user) { renderLogin(); return; }
    state.user = state.status.user;
    state.sites = await get('/api/sites');
    if (!location.hash) location.hash = '#/overview';
    connectStream();
    route();
  }

  async function route() {
    if (state.setup) { renderSetup(); return; }
    if (!state.user) { renderLogin(); return; }
    const parts = location.hash.replace(/^#\/?/, '').split('/');
    const page = parts[0] || 'overview';
    if (state.page && state.page.destroy) state.page.destroy();
    state.page = null;
    ensureShell();
    document.querySelectorAll('.nav').forEach(n => n.classList.toggle('active', n.dataset.page === page));
    const main = $('#main');
    main.className = 'main';
    main.innerHTML = '';
    const pages = { overview: pageOverview, topology: pageTopology, alerts: pageAlerts, devices: pageDevices, device: pageDevice, logs: pageLogs, flow: pageFlow, endpoints: pageEndpoints, probes: pageProbes, wireless: pageWireless, reports: pageReports, admin: pageAdmin };
    const fn = pages[page] || pageOverview;
    try { state.page = await fn(main, parts.slice(1)) || {}; } catch (e) { main.innerHTML = '<div class="empty"><h3>Something went wrong</h3>' + esc(e.message) + '</div>'; }
  }

  // ---------- shell ----------
  function ensureShell() {
    if ($('#main')) { refreshTop(); return; }
    const app = $('#app');
    app.className = 'app';
    app.innerHTML = '';
    const side = h('aside', { class: 'side' },
      h('div', { class: 'brand', html: LOGO + '<div>TopoLight<small>' + esc(state.status.settings && state.status.settings.instance_name || '') + '</small></div>' }),
      ...[['overview', 'Overview'], ['topology', 'Topology'], ['alerts', 'Alerts'], ['devices', 'Devices'], ['logs', 'Logs'], ['flow', 'Flow'], ['endpoints', 'Endpoints'], ['probes', 'Probes'], ['wireless', 'Wireless & WAN'], ['reports', 'Reports'], ['admin', 'Admin']].map(([p, label]) => {
        const b = h('a', { class: 'nav', href: '#/' + p, 'data-page': p }, svg(p), h('span', null, label));
        if (p === 'alerts') b.append(h('span', { class: 'badge critical', id: 'nav-alerts', style: 'display:none' }, '0'));
        return b;
      }),
      h('div', { class: 'spacer' }),
      h('div', { class: 'foot', id: 'foot' })
    );
    const top = h('header', { class: 'top' },
      h('div', { class: 'live' }, h('span', { class: 'dot pulse', id: 'wsdot' }), h('span', { id: 'wstext' }, 'Live')),
      h('div', { class: 'kpi' }, h('span', { class: 'l' }, 'Up'), h('span', { class: 'v', id: 'k-up' }, '—')),
      h('div', { class: 'kpi' }, h('span', { class: 'l' }, 'Down'), h('span', { class: 'v', id: 'k-down', style: 'color:var(--crit)' }, '—')),
      h('div', { class: 'kpi' }, h('span', { class: 'l' }, 'Degraded'), h('span', { class: 'v', id: 'k-deg', style: 'color:var(--major)' }, '—')),
      h('div', { class: 'kpi' }, h('span', { class: 'l' }, 'Alerts'), h('span', { class: 'v', id: 'k-alerts' }, '—')),
      h('div', { class: 'spacer' }),
      h('select', { class: 'btn', id: 'site-filter', 'aria-label': 'Site filter', onchange: e => { state.siteFilter = e.target.value; try { localStorage.setItem('topolight-site', state.siteFilter); } catch (x) { } route(); } }),
      h('div', { class: 'search', onclick: openPalette, role: 'button', tabindex: 0 }, 'Search devices, alerts…', h('kbd', null, '/')),
      h('button', { class: 'iconbtn', title: 'Toggle theme', onclick: toggleTheme }, svg('theme')),
      h('button', { class: 'iconbtn', title: 'Sign out', onclick: async () => { await post('/api/logout'); state.user = null; location.hash = ''; renderLogin(); } }, svg('logout'))
    );
    app.append(side, top, h('main', { class: 'main', id: 'main' }));
    refreshTop();
    document.addEventListener('keydown', e => {
      if (e.target.matches('input,textarea,select')) return;
      if (e.key === '/') { e.preventDefault(); openPalette(); }
      if (e.key === 'g') { state._g = true; setTimeout(() => state._g = false, 800); return; }
      if (state._g) { const m = { o: 'overview', t: 'topology', a: 'alerts', d: 'devices', l: 'logs', f: 'flow', e: 'endpoints', p: 'probes', r: 'reports', s: 'admin' }[e.key]; if (m) nav('#/' + m); state._g = false; }
      if (e.key === '?') { toast('Keys: / search · g o/t/a/d/l/f/e/p/r/s go to page · in alerts: j/k move, a ack, r resolve, Enter open', 'ok', 8000); }
    });
  }

  function refreshTop() {
    const s = state.status; if (!s || !s.health) return;
    $('#k-up').textContent = s.health.up; $('#k-down').textContent = s.health.down; $('#k-deg').textContent = s.health.degraded;
    const a = s.alerts || {}; const n = (a.critical || 0) + (a.major || 0) + (a.minor || 0);
    $('#k-alerts').textContent = n;
    const badge = $('#nav-alerts'); badge.textContent = (a.critical || 0) + (a.major || 0); badge.style.display = badge.textContent !== '0' ? '' : 'none';
    const sel = $('#site-filter');
    sel.innerHTML = '<option value="">All sites</option>' + state.sites.map(x => `<option value="${esc(x.id)}" ${x.id === state.siteFilter ? 'selected' : ''}>${esc(x.name)}</option>`).join('');
    const lic = s.license || {};
    $('#foot').innerHTML = `${esc(s.product)} ${esc(s.version)}<br>${esc(lic.tier ? cap(lic.tier) + ' · ' : '')}${s.devices.monitored}/${lic.caps && lic.caps.max_devices ? lic.caps.max_devices : '∞'} devices<br><span class="faint">${esc(state.user.name)} · ${esc(state.user.role)}</span>`;
  }

  async function refreshStatus() { try { state.status = await get('/api/status'); refreshTop(); } catch (e) { } }

  function toggleTheme() {
    const r = document.documentElement;
    const dark = r.dataset.theme === 'dark' || (!r.dataset.theme && matchMedia('(prefers-color-scheme: dark)').matches);
    r.dataset.theme = dark ? 'light' : 'dark';
    try { localStorage.setItem('topolight-theme', r.dataset.theme); } catch (e) { }
    if (state.page && state.page.theme) state.page.theme();
  }

  // ---------- live stream ----------
  function connectStream() {
    if (state.sse) state.sse.close();
    const es = new EventSource('/api/stream');
    state.sse = es;
    const dot = () => $('#wsdot'), txt = () => $('#wstext');
    es.onopen = () => { if (dot()) { dot().className = 'dot pulse'; txt().textContent = 'Live'; } };
    es.onerror = () => { if (dot()) { dot().className = 'dot warn'; txt().textContent = 'Reconnecting…'; } };
    ['device', 'interface', 'link', 'alert', 'event', 'topology', 'discovery'].forEach(type => {
      es.addEventListener(type, ev => {
        let data = null; try { data = JSON.parse(ev.data); } catch (e) { return; }
        if (type === 'alert') {
          const fresh = data.state === 'open' && !data.root_cause && (data.occurrences === 1 || /\(re-opened\)$/.test((data.evidence || []).slice(-1)[0] || ''));
          if (fresh && (data.severity === 'critical' || data.severity === 'major')) toast(`${data.severity.toUpperCase()} · ${data.title}`, data.severity);
          refreshStatusThrottled();
        }
        if (type === 'device') { refreshStatusThrottled(); }
        if (state.page && state.page.onChange) state.page.onChange(type, data);
      });
    });
  }
  let _rs = 0;
  let _rsTimer = null;
  function refreshStatusThrottled() { const now = Date.now(); if (now - _rs > 3000) { _rs = now; refreshStatus(); } else if (!_rsTimer) { _rsTimer = setTimeout(() => { _rsTimer = null; _rs = Date.now(); refreshStatus(); }, 3000 - (now - _rs)); } }

  // ---------- command palette ----------
  async function openPalette() {
    const pal = $('#palette');
    pal.hidden = false;
    pal.innerHTML = '';
    const input = h('input', { placeholder: 'Type a device, IP, site, alert… or a command (ack all, rebuild topology)', autofocus: true });
    const list = h('div', { class: 'list' });
    const box = h('div', { class: 'box' }, input, list);
    pal.append(box);
    let devices = [];
    try { devices = await get('/api/devices'); } catch (e) { }
    let alerts = [];
    try { alerts = (await get('/api/alerts?state=active')).alerts; } catch (e) { }
    const cmds = [
      { label: 'Go to Overview', k: 'g o', run: () => nav('#/overview') }, { label: 'Go to Topology', k: 'g t', run: () => nav('#/topology') },
      { label: 'Go to Alerts', k: 'g a', run: () => nav('#/alerts') }, { label: 'Go to Devices', k: 'g d', run: () => nav('#/devices') },
      { label: 'Go to Logs', k: 'g l', run: () => nav('#/logs') }, { label: 'Go to Flow', k: 'g f', run: () => nav('#/flow') }, { label: 'Go to Endpoints', k: 'g e', run: () => nav('#/endpoints') }, { label: 'Go to Probes', k: 'g p', run: () => nav('#/probes') }, { label: 'Go to Wireless & WAN', k: 'g w', run: () => nav('#/wireless') }, { label: 'Go to Reports', k: 'g r', run: () => nav('#/reports') }, { label: 'Admin', k: 'g s', run: () => nav('#/admin') },
      { label: 'Rebuild topology now', run: async () => { await post('/api/topology/rebuild'); toast('Topology rebuild started', 'ok'); } },
      { label: 'Toggle theme', run: toggleTheme },
    ];
    let sel = 0, items = [];
    function render() {
      const q = input.value.trim().toLowerCase();
      items = [];
      cmds.filter(c => !q || c.label.toLowerCase().includes(q)).slice(0, 5).forEach(c => items.push({ label: c.label, k: c.k, run: c.run, kind: 'cmd' }));
      devices.filter(d => !q || (d.name + ' ' + d.ip + ' ' + (d.model || '')).toLowerCase().includes(q)).slice(0, 8).forEach(d => items.push({ label: d.name, sub: d.ip + ' · ' + (d.model || d.vendor || ''), status: d.status, run: () => nav('#/device/' + d.id) }));
      alerts.filter(a => !q || a.title.toLowerCase().includes(q)).slice(0, 5).forEach(a => items.push({ label: a.title, sub: a.severity + ' · ' + ago(a.opened_at), run: () => nav('#/alerts/' + a.id) }));
      sel = Math.min(sel, Math.max(0, items.length - 1));
      list.innerHTML = '';
      items.forEach((it, i) => list.append(h('div', { class: 'item' + (i === sel ? ' sel' : ''), onclick: () => { close(); it.run(); } },
        it.status ? h('span', { class: 'sdot ' + it.status }) : null, h('span', null, it.label), it.sub ? h('span', { class: 'muted small' }, it.sub) : null, it.k ? h('span', { class: 'k' }, it.k) : null)));
    }
    function close() { pal.hidden = true; pal.innerHTML = ''; document.removeEventListener('keydown', key); }
    function key(e) {
      if (e.key === 'Escape') { close(); }
      else if (e.key === 'ArrowDown') { sel = Math.min(items.length - 1, sel + 1); render(); e.preventDefault(); }
      else if (e.key === 'ArrowUp') { sel = Math.max(0, sel - 1); render(); e.preventDefault(); }
      else if (e.key === 'Enter') { const it = items[sel]; if (it) { close(); it.run(); } }
    }
    document.addEventListener('keydown', key);
    input.addEventListener('input', () => { sel = 0; render(); });
    pal.onclick = e => { if (e.target === pal) close(); };
    render();
    input.focus();
  }

  // ---------- setup wizard ----------
  function renderSetup() {
    const app = $('#app'); app.className = ''; app.innerHTML = '';
    let step = 0;
    const data = { user: '', password: '', instance: '', site: { name: '', subnets: '' }, cred: { name: 'Default', version: '3', community: '', user: 'topolight', auth_proto: 'sha', auth_pass: '', priv_proto: 'aes', priv_pass: '' }, siteId: null, credId: null };
    const card = h('div', { class: 'card' });
    app.append(h('div', { class: 'center' }, card));
    const STEPS = ['Admin', 'Site', 'SNMP', 'Discovery', 'Done'];
    function steps() { return h('div', { class: 'steps' }, ...STEPS.map((s, i) => [h('span', { class: i === step ? 'on' : '' }, (i + 1) + ' ' + s), i < STEPS.length - 1 ? '›' : null])); }
    function brand(sub) { return h('div', { class: 'brand', html: LOGO + '<div>TopoLight<small>' + esc(sub) + '</small></div>' }); }
    function show() {
      card.innerHTML = '';
      if (step === 0) {
        const err = h('div', { class: 'err' });
        const f = h('form', { class: 'form', onsubmit: async e => {
          e.preventDefault();
          data.user = $('#su', f).value.trim(); data.password = $('#sp', f).value; data.instance = $('#si', f).value.trim();
          if (data.password !== $('#sp2', f).value) { err.textContent = 'Passwords do not match.'; return; }
          try { await post('/api/setup', { User: data.user, Password: data.password, InstanceName: data.instance }); step = 1; show(); }
          catch (x) { err.textContent = x.message; }
        } },
          h('label', null, 'Instance name', h('input', { id: 'si', placeholder: 'e.g. ACME NOC', value: data.instance })),
          h('label', null, 'Admin user name', h('input', { id: 'su', required: true, minlength: 2, autocomplete: 'username', value: data.user })),
          h('div', { class: 'row' }, h('label', null, 'Password (10+ characters)', h('input', { id: 'sp', type: 'password', required: true, minlength: 10, autocomplete: 'new-password' })), h('label', null, 'Repeat password', h('input', { id: 'sp2', type: 'password', required: true, minlength: 10, autocomplete: 'new-password' }))),
          err, h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, 'Create admin →')));
        card.append(brand('Welcome — let’s get your first devices on the map in a few minutes.'), steps(), f);
      } else if (step === 1) {
        const err = h('div', { class: 'err' });
        const f = h('form', { class: 'form', onsubmit: async e => {
          e.preventDefault();
          const name = $('#sn', f).value.trim(), subnets = $('#ss', f).value.split(/[\n,]/).map(s => s.trim()).filter(Boolean);
          try { const s = await post('/api/sites', { name, subnets }); data.siteId = s.id; step = 2; show(); } catch (x) { err.textContent = x.message; }
        } },
          h('label', null, 'Site name', h('input', { id: 'sn', required: true, placeholder: 'e.g. Jakarta HQ' })),
          h('label', null, 'Subnets or ranges to discover (one per line)', h('textarea', { id: 'ss', rows: 4, placeholder: '10.20.0.0/24\n10.20.1.0/24\n192.168.10.1-192.168.10.50' })),
          h('div', { class: 'hint' }, 'Largest range per line is /20 (4,096 addresses). You can add more sites later in Admin.'),
          err, h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, 'Next: SNMP credential →')));
        card.append(brand('Step 2 — where should TopoLight look?'), steps(), f);
      } else if (step === 2) {
        const err = h('div', { class: 'err' });
        const f = credForm(data.cred, async c => {
          try { const saved = await post('/api/creds', c); data.credId = saved.id; await put('/api/sites/' + data.siteId, Object.assign({}, await get('/api/sites').then(ss => ss.find(s => s.id === data.siteId)), { cred_id: saved.id })); step = 3; show(); }
          catch (x) { err.textContent = x.message; }
        }, 'Next: start discovery →');
        card.append(brand('Step 3 — how does TopoLight talk to your devices?'), steps(), f, err, h('div', { class: 'hint', style: 'margin-top:10px' }, 'SNMPv3 with SHA + AES is recommended. Read-only is all TopoLight ever needs. The Admin page generates per-vendor config snippets for you.'));
      } else if (step === 3) {
        const prog = h('div', { class: 'progress' }, h('i', { style: 'width:0%' }));
        const stat = h('div', { class: 'muted small' }, 'Starting…');
        const found = h('div', { class: 'found' });
        const btn = h('button', { class: 'btn primary', disabled: true, onclick: () => { step = 4; show(); } }, 'Finish →');
        card.append(brand('Step 4 — discovering your network'), steps(), prog, stat, found, h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => { step = 4; show(); } }, 'Skip'), btn));
        post('/api/sites/' + data.siteId + '/discover').catch(x => { stat.textContent = x.message; });
        const seen = new Set();
        const timer = setInterval(async () => {
          try {
            const p = await get('/api/sites/' + data.siteId + '/discovery');
            if (p.total) prog.firstChild.style.width = Math.round(p.scanned / p.total * 100) + '%';
            stat.textContent = `${p.scanned || 0}/${p.total || 0} addresses scanned · ${p.answered || 0} answered ping · ${p.found || 0} SNMP devices` + (p.skipped ? ` · ${p.skipped} over licence limit` : '');
            const devs = await get('/api/devices?site=' + data.siteId);
            devs.forEach(d => { if (!seen.has(d.id)) { seen.add(d.id); found.append(h('div', null, h('span', { class: 'sdot ' + (d.monitored ? 'up' : 'unknown') }), h('b', null, d.name), h('span', { class: 'muted' }, d.ip + ' · ' + (d.vendor || 'unknown vendor')), d.monitored ? null : h('span', { class: 'badge minor' }, 'not monitored'))); } });
            if (!p.running && p.total !== undefined) { clearInterval(timer); btn.disabled = false; if (!devs.length) stat.textContent += ' — nothing answered SNMP. Check the credential and that UDP/161 is allowed from this host.'; }
          } catch (x) { }
        }, 1500);
      } else {
        card.append(brand('You’re set.'), steps(),
          h('p', null, 'Devices are being polled now. The topology map fills in as LLDP/CDP tables are read (first pass in about two minutes).'),
          h('ul', { class: 'muted' }, h('li', null, 'Point your devices’ syslog and SNMP traps at this host — Admin → Snippets writes the exact lines per vendor.'),
            h('li', null, 'Add e-mail, Telegram or a webhook under Admin → Notifications.'), h('li', null, 'Press / anywhere to search; ? shows keyboard shortcuts.')),
          h('div', { class: 'actions' }, h('button', { class: 'btn primary', onclick: () => { state.setup = false; location.hash = '#/overview'; boot(); } }, 'Open the console')));
      }
    }
    show();
  }

  function credForm(c, onSubmit, submitLabel) {
    const f = h('form', { class: 'form' });
    const ver = h('select', { onchange: () => toggle() }, h('option', { value: '3', selected: c.version === '3' && c.kind !== 'ssh' }, 'SNMP v3 (recommended)'), h('option', { value: '2c', selected: c.version === '2c' }, 'SNMP v2c'), h('option', { value: 'ssh', selected: c.kind === 'ssh' }, 'SSH (configuration backup)'), h('option', { value: 'gnmi', selected: c.kind === 'gnmi' }, 'gNMI / OpenConfig (beta)'));
    const gnmiBox = h('div', null,
      h('div', { class: 'row' }, h('label', null, 'User', h('input', { id: 'gu', value: c.kind === 'gnmi' ? (c.user || '') : '' })), h('label', null, 'Password', h('input', { id: 'gp', type: 'password', value: c.kind === 'gnmi' ? (c.password || '') : '', autocomplete: 'new-password' })), h('label', null, 'Port', h('input', { id: 'gpt', type: 'number', value: c.kind === 'gnmi' ? (c.port || 6030) : 6030, min: 1, max: 65535 }))),
      h('div', { class: 'row' }, h('label', { class: 'check' }, h('input', { id: 'gtls', type: 'checkbox', checked: c.kind === 'gnmi' ? !c.plaintext : true }), ' TLS (uncheck for plaintext h2c)'), h('label', { class: 'check' }, h('input', { id: 'gsv', type: 'checkbox', checked: c.kind === 'gnmi' ? !!c.skip_verify : true }), ' accept self-signed device certificate')),
      h('p', { class: 'small muted' }, 'Reads OpenConfig state over gRPC (system, cpus, memory, interfaces) instead of SNMP — Arista EOS (port 6030), Nokia SR Linux (57400), Cisco IOS-XR/NX-OS (57400/50051), Juniper (32767/9339), SONiC. Pick this credential on the device when you add it. Beta: routing/L2 tables, temperature and vendor health still need SNMP.'));
    const sshBox = h('div', null,
      h('div', { class: 'row' }, h('label', null, 'User', h('input', { id: 'su', value: c.kind === 'ssh' ? (c.user || '') : '' })), h('label', null, 'Password', h('input', { id: 'sp', type: 'password', value: c.password || '', autocomplete: 'new-password' })), h('label', null, 'Enable password (Cisco, optional)', h('input', { id: 'se', type: 'password', value: c.enable_pass || '', autocomplete: 'new-password' })), h('label', null, 'Port', h('input', { id: 'spt', type: 'number', value: c.port || 22, min: 1, max: 65535 }))),
      h('label', null, 'Private key (PEM, optional — used before the password)', h('textarea', { id: 'sk', rows: 3, placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----' }, c.private_key === '••••' ? '••••' : (c.private_key || ''))),
      h('p', { class: 'small muted' }, 'Read-only access is enough: TopoLight only runs "show running-config" (or the vendor equivalent). Assign the credential per site or per device.'));
    const v2 = h('div', { class: 'row' }, h('label', null, 'Community (read-only)', h('input', { id: 'cc', value: c.community || '', autocomplete: 'off' })));
    const snmpPort = h('div', null,
      h('div', { class: 'row' }, h('label', null, 'UDP port', h('input', { id: 'cpt', type: 'number', value: (c.kind === 'ssh' || c.kind === 'gnmi') ? 161 : (c.port || 161), min: 1, max: 65535 }))),
      h('p', { class: 'small muted' }, 'Leave this at 161 unless the agent answers somewhere else. Lab simulators, containers and homelab gear often listen on a high port because 161 needs privilege. The port applies to every device that uses this credential — make a second credential for agents on a different one.'));
    const v3 = h('div', null,
      h('div', { class: 'row' }, h('label', null, 'User', h('input', { id: 'cu', value: c.user || '' })),
        h('label', null, 'Auth protocol', h('select', { id: 'ca' }, ...['sha', 'sha256', 'md5', ''].map(p => h('option', { value: p, selected: (c.auth_proto || 'sha') === p }, p || 'none')))),
        h('label', null, 'Auth password', h('input', { id: 'cap', type: 'password', value: c.auth_pass || '', autocomplete: 'new-password' }))),
      h('div', { class: 'row' }, h('label', null, 'Privacy protocol', h('select', { id: 'cp' }, ...['aes', 'des', ''].map(p => h('option', { value: p, selected: (c.priv_proto || 'aes') === p }, p || 'none')))),
        h('label', null, 'Privacy password', h('input', { id: 'cpp', type: 'password', value: c.priv_pass || '', autocomplete: 'new-password' }))));
    function toggle() { const is3 = ver.value === '3', ssh = ver.value === 'ssh', gn = ver.value === 'gnmi'; v2.classList.toggle('hidden', is3 || ssh || gn); v3.classList.toggle('hidden', !is3); snmpPort.classList.toggle('hidden', ssh || gn); sshBox.classList.toggle('hidden', !ssh); gnmiBox.classList.toggle('hidden', !gn); }
    f.append(h('div', { class: 'row' }, h('label', null, 'Name', h('input', { id: 'cn', value: c.name || '', required: true })), h('label', null, 'Type', ver)), v2, v3, snmpPort, sshBox, gnmiBox,
      h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, submitLabel || 'Save')));
    toggle();
    f.onsubmit = e => {
      e.preventDefault();
      const out = ver.value === 'gnmi' ? { name: $('#cn', f).value.trim(), kind: 'gnmi', user: $('#gu', f).value.trim(), password: $('#gp', f).value, port: Number($('#gpt', f).value) || 6030, plaintext: !$('#gtls', f).checked, skip_verify: $('#gsv', f).checked }
        : ver.value === 'ssh' ? { name: $('#cn', f).value.trim(), kind: 'ssh', user: $('#su', f).value.trim(), password: $('#sp', f).value, enable_pass: $('#se', f).value, port: Number($('#spt', f).value) || 22, private_key: $('#sk', f).value.trim() }
        : { name: $('#cn', f).value.trim(), version: ver.value, community: $('#cc', f).value, user: $('#cu', f).value.trim(), auth_proto: $('#ca', f).value, auth_pass: $('#cap', f).value, priv_proto: $('#cp', f).value, priv_pass: $('#cpp', f).value, port: Number($('#cpt', f).value) || 161 };
      if (c.id) out.id = c.id;
      onSubmit(out);
    };
    return f;
  }

  // ---------- login ----------
  function renderLogin() {
    const app = $('#app'); app.className = ''; app.innerHTML = '';
    const err = h('div', { class: 'err' });
    const f = h('form', { class: 'form', onsubmit: async e => {
      e.preventDefault();
      try { await post('/api/login', { User: $('#lu', f).value.trim(), Password: $('#lp', f).value }); state.status = await get('/api/status'); state.user = state.status.user; state.sites = await get('/api/sites'); $('#app').innerHTML = ''; if (!location.hash) location.hash = '#/overview'; connectStream(); route(); }
      catch (x) { err.textContent = x.message; }
    } },
      h('label', null, 'User name', h('input', { id: 'lu', autocomplete: 'username', required: true, autofocus: true })),
      h('label', null, 'Password', h('input', { id: 'lp', type: 'password', autocomplete: 'current-password', required: true })),
      err, h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, 'Sign in')));
    app.append(h('div', { class: 'center' }, h('div', { class: 'card' }, h('div', { class: 'brand', html: LOGO + '<div>TopoLight<small>Sign in to the console</small></div>' }), f)));
  }

  // ---------- charts ----------
  function sparkline(canvas, pts, color, opts) {
    opts = opts || {};
    const dpr = window.devicePixelRatio || 1;
    const r = canvas.getBoundingClientRect();
    canvas.width = Math.max(1, r.width * dpr); canvas.height = Math.max(1, r.height * dpr);
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    if (!pts || pts.length < 2) { ctx.fillStyle = cssVar('--faint'); ctx.font = `${11 * dpr}px ${cssVar('--font')}`; ctx.fillText('no data yet', 4 * dpr, canvas.height / 2 + 4 * dpr); return; }
    const W = canvas.width, H = canvas.height, pad = 2 * dpr;
    let min = Math.min(...pts.map(p => p.v)), max = Math.max(...pts.map(p => p.v));
    if (opts.min !== undefined) min = Math.min(min, opts.min);
    if (opts.max !== undefined) max = Math.max(max, opts.max);
    if (max === min) { max = min + 1; }
    const t0 = pts[0].t, t1 = pts[pts.length - 1].t || t0 + 1;
    const X = t => pad + (t - t0) / Math.max(1, t1 - t0) * (W - pad * 2), Y = v => H - pad - (v - min) / (max - min) * (H - pad * 2);
    ctx.beginPath();
    pts.forEach((p, i) => { if (i) ctx.lineTo(X(p.t), Y(p.v)); else ctx.moveTo(X(p.t), Y(p.v)); });
    ctx.strokeStyle = color; ctx.lineWidth = 1.4 * dpr; ctx.stroke();
    ctx.lineTo(X(t1), H); ctx.lineTo(X(t0), H); ctx.closePath(); ctx.fillStyle = color; ctx.globalAlpha = .12; ctx.fill(); ctx.globalAlpha = 1;
    if (opts.threshold !== undefined && opts.threshold <= max && opts.threshold >= min) { ctx.setLineDash([3 * dpr, 3 * dpr]); ctx.beginPath(); ctx.moveTo(pad, Y(opts.threshold)); ctx.lineTo(W - pad, Y(opts.threshold)); ctx.strokeStyle = cssVar('--minor'); ctx.lineWidth = dpr; ctx.stroke(); ctx.setLineDash([]); }
    const last = pts[pts.length - 1]; ctx.beginPath(); ctx.arc(X(last.t), Y(last.v), 2.6 * dpr, 0, Math.PI * 2); ctx.fillStyle = color; ctx.fill();
  }
  function cssVar(n) { return getComputedStyle(document.documentElement).getPropertyValue(n).trim(); }

  function lineChart(canvas, series, opts) {
    // series: [{pts, color, label}]
    opts = opts || {};
    const dpr = window.devicePixelRatio || 1;
    const r = canvas.getBoundingClientRect();
    canvas.width = Math.max(1, r.width * dpr); canvas.height = Math.max(1, r.height * dpr);
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    const W = canvas.width, H = canvas.height, padL = (opts.padL || 62) * dpr, padB = 18 * dpr, padT = 8 * dpr, padR = 8 * dpr;
    const all = series.flatMap(s => s.pts);
    if (all.length < 2) { ctx.fillStyle = cssVar('--faint'); ctx.font = `${12 * dpr}px ${cssVar('--font')}`; ctx.fillText('No data in this window yet.', padL, H / 2); return; }
    const t0 = Math.min(...all.map(p => p.t)), t1 = Math.max(...all.map(p => p.t));
    let min = 0, max = Math.max(...all.map(p => p.max !== undefined && p.max > p.v ? p.max : p.v)) * 1.1;
    if (opts.max) max = Math.max(max, opts.max);
    if (max <= 0) max = 1;
    const X = t => padL + (t - t0) / Math.max(1, t1 - t0) * (W - padL - padR), Y = v => H - padB - (v - min) / (max - min) * (H - padB - padT);
    ctx.strokeStyle = cssVar('--grid'); ctx.lineWidth = dpr; ctx.fillStyle = cssVar('--faint'); ctx.font = `${10 * dpr}px ${cssVar('--mono')}`; ctx.textAlign = 'right';
    for (let i = 0; i <= 4; i++) { const v = min + (max - min) * i / 4; const y = Y(v); ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(W - padR, y); ctx.stroke(); ctx.fillText(opts.fmt ? opts.fmt(v) : ((max - min) < 5 ? v.toFixed(1) : v.toFixed(0)), padL - 4 * dpr, y + 3 * dpr); }
    ctx.textAlign = 'center';
    for (let i = 0; i <= 4; i++) { const t = t0 + (t1 - t0) * i / 4; ctx.fillText(new Date(t * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }), X(t), H - 4 * dpr); }
    series.forEach(s => {
      if (s.pts.length < 2) return;
      if (s.pts[0].max !== undefined && s.pts.some(p => p.max !== p.v)) {
        ctx.beginPath(); s.pts.forEach((p, i) => { i ? ctx.lineTo(X(p.t), Y(p.max)) : ctx.moveTo(X(p.t), Y(p.max)); });
        for (let i = s.pts.length - 1; i >= 0; i--) ctx.lineTo(X(s.pts[i].t), Y(s.pts[i].min));
        ctx.closePath(); ctx.fillStyle = s.color; ctx.globalAlpha = .15; ctx.fill(); ctx.globalAlpha = 1;
      }
      ctx.beginPath(); s.pts.forEach((p, i) => { i ? ctx.lineTo(X(p.t), Y(p.v)) : ctx.moveTo(X(p.t), Y(p.v)); });
      ctx.strokeStyle = s.color; ctx.lineWidth = 1.5 * dpr; ctx.stroke();
    });
    if (opts.threshold) { ctx.setLineDash([4 * dpr, 4 * dpr]); ctx.beginPath(); ctx.moveTo(padL, Y(opts.threshold)); ctx.lineTo(W - padR, Y(opts.threshold)); ctx.strokeStyle = cssVar('--minor'); ctx.stroke(); ctx.setLineDash([]); }
    ctx.textAlign = 'left'; ctx.font = `${11 * dpr}px ${cssVar('--font')}`;
    let lx = padL;
    series.forEach(s => { ctx.fillStyle = s.color; ctx.fillRect(lx, padT, 10 * dpr, 3 * dpr); ctx.fillStyle = cssVar('--muted'); ctx.fillText(s.label, lx + 14 * dpr, padT + 4 * dpr); lx += (14 + s.label.length * 6.5 + 12) * dpr; });
  }

  // ---------- overview ----------
  async function pageOverview(main) {
    const siteQ = state.siteFilter ? '?site=' + state.siteFilter : '';
    const [devices, alertsResp, events] = await Promise.all([get('/api/devices' + siteQ), get('/api/alerts?state=active&root_only=true' + (state.siteFilter ? '&site=' + state.siteFilter : '')), get('/api/events?limit=40')]);
    const s = state.status;
    const mon = devices.filter(d => d.monitored);
    const counts = { up: 0, down: 0, degraded: 0, unknown: 0 };
    mon.forEach(d => { counts[d.status === 'unreachable' || d.status === 'flapping' ? 'down' : (counts[d.status] !== undefined ? d.status : 'unknown')]++; });
    const health = mon.length ? Math.round((counts.up + counts.degraded * 0.5) / mon.length * 1000) / 10 : 0;
    const tiles = h('div', { class: 'grid c4' },
      tile('Estate health', health + '%', mon.length + ' monitored', () => nav('#/devices'), health < 90 ? 'var(--major)' : ''),
      tile('Down / unreachable', counts.down, counts.down ? 'needs attention' : 'all good', () => nav('#/devices/down'), counts.down ? 'var(--crit)' : ''),
      tile('Degraded', counts.degraded, 'threshold breached', () => nav('#/devices/degraded'), counts.degraded ? 'var(--major)' : ''),
      tile('Open alerts', alertsResp.alerts.length, `${(s.alerts || {}).critical || 0} critical · ${(s.alerts || {}).major || 0} major`, () => nav('#/alerts')));
    function tile(l, v, sub, click, color) { return h('div', { class: 'tile clickable', onclick: click }, h('span', { class: 'l' }, l), h('span', { class: 'v', style: color ? 'color:' + color : '' }, String(v)), h('span', { class: 'sub' }, sub)); }

    // sites
    const bySite = {};
    devices.forEach(d => { (bySite[d.site_id] = bySite[d.site_id] || []).push(d); });
    const siteRows = state.sites.map(site => {
      const ds = bySite[site.id] || [];
      const worst = ds.reduce((w, d) => Math.max(w, rank(d.status)), 0);
      const st = ['up', 'maintenance', 'unknown', 'degraded', 'unreachable', 'flapping', 'down'][worst] || 'up';
      const down = ds.filter(d => d.status === 'down' || d.status === 'unreachable').length;
      return h('tr', { class: 'row', onclick: () => { state.siteFilter = site.id; $('#site-filter').value = site.id; nav('#/topology'); } },
        h('td', null, h('span', { class: 'sdot ' + (ds.length ? st : 'unknown'), style: 'margin-right:8px' }), site.name),
        h('td', { class: 'r num' }, ds.length), h('td', { class: 'r num', style: down ? 'color:var(--crit)' : '' }, down),
        h('td', null, h('div', { class: 'ribbon' }, ...ds.slice(0, 60).map(d => h('i', { class: d.status === 'down' || d.status === 'unreachable' ? 'c' : d.status === 'degraded' ? 'm' : d.status === 'unknown' ? 'u' : '', title: d.name })))));
    });
    function rank(s) { return { up: 0, maintenance: 1, unknown: 2, degraded: 3, unreachable: 4, flapping: 5, down: 6 }[s] || 0; }

    const alertList = h('div', { class: 'list' });
    function renderAlerts(list) {
      alertList.innerHTML = '';
      if (!list.length) { alertList.append(h('div', { class: 'empty' }, h('h3', null, 'No open alerts'), 'Everything monitored is answering.')); return; }
      list.slice(0, 12).forEach(a => alertList.append(alertItem(a, alertsResp.names, () => nav('#/alerts/' + a.id))));
    }
    renderAlerts(alertsResp.alerts);

    const evList = h('div', { class: 'list' });
    function evItem(e) { return h('div', { class: 'item ' + (e.severity || 'info') }, h('span', { class: 'bar' }), h('div', null, h('div', { class: 't' }, h('span', null, clock(e.ts)), h('span', null, '·'), h('span', null, e.source), h('span', null, '·'), h('span', null, e.kind)), h('div', { class: 'd', style: 'color:var(--text)' }, e.message))); }
    events.forEach(e => evList.append(evItem(e)));
    if (!events.length) evList.append(h('div', { class: 'empty' }, 'No events yet — discovery and the first polls will appear here.'));

    const col = s.collectors || {};
    const collectorNote = !col.icmp ? h('div', { class: 'panel', style: 'padding:12px 16px;border-color:var(--minor)' }, h('b', null, 'ICMP is off: '), col.icmp_error, ' Reachability uses SNMP only until this is fixed.') : null;

    main.append(
      h('div', { class: 'page-head' }, h('h1', null, 'Overview'), h('span', { class: 'sub' }, state.siteFilter ? (state.sites.find(x => x.id === state.siteFilter) || {}).name : 'All sites'), h('div', { class: 'spacer' }),
        h('span', { class: 'hint' }, `polls ${col.poll_cycles || 0} · syslog ${col.syslog_received || 0} · traps ${col.trap_received || 0} · ${col.series || 0} series · ${fmtBytes(col.tsdb_bytes || 0)} metrics on disk`)),
      collectorNote,
      tiles,
      h('div', { class: 'split', style: 'margin-top:16px' },
        h('div', { class: 'grid', style: 'grid-template-columns:1fr' },
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h2', null, 'Sites'), h('div', { class: 'spacer' }), h('a', { class: 'btn sm', href: '#/admin/sites' }, 'Manage')),
            h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Site'), h('th', { class: 'r' }, 'Devices'), h('th', { class: 'r' }, 'Down'), h('th', null, 'Status by device'))), h('tbody', null, ...siteRows)))),
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h2', null, 'Recent events')), h('div', { style: 'padding:6px' }, evList))),
        h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h2', null, 'Open alerts'), h('span', { class: 'hint' }, 'root causes only'), h('div', { class: 'spacer' }), h('a', { class: 'btn sm', href: '#/alerts' }, 'All')), h('div', { style: 'padding:6px' }, alertList))));
    return {
      onChange(type, data) {
        if (type === 'alert') { get('/api/alerts?state=active&root_only=true').then(r => renderAlerts(r.alerts)).catch(() => { }); }
        if (type === 'event') { evList.prepend(evItem(data)); while (evList.children.length > 40) evList.lastChild.remove(); }
      }
    };
  }

  function alertItem(a, names, onclick, selected) {
    const site = names && names[a.site_id] || '';
    const el = h('div', { class: 'item ' + (a.state === 'resolved' ? 'resolved' : a.severity) + (a.state === 'acked' ? ' acked' : '') + (selected ? ' sel' : ''), onclick },
      h('span', { class: 'bar' }),
      h('div', null,
        h('div', { class: 't' }, h('span', { class: 'badge ' + (a.state === 'resolved' ? 'resolved' : a.severity) }, a.state === 'resolved' ? 'Resolved' : a.severity), h('span', null, clock(a.opened_at)), site ? h('span', null, '· ' + site) : null, a.state === 'acked' ? h('span', null, '· ✓ ' + a.acked_by) : null, a.occurrences > 1 ? h('span', null, '· ×' + a.occurrences) : null),
        h('div', { class: 'h' }, a.title),
        a.impact ? h('div', { class: 'd' }, a.impact) : null,
        a.children ? h('div', { class: 'kids' }, a.children + ' downstream alert(s) folded under this one') : null));
    return el;
  }

  // ---------- device health card (topology hover + side panel) ----------
  // One small GET per device, cached for a few seconds so orbiting the map
  // over a cluster of nodes never turns into a request storm.
  const healthCache = {};
  const HEALTH_TTL = 5000;
  async function deviceHealth(id) {
    const c = healthCache[id];
    if (c && Date.now() - c.at < HEALTH_TTL) return c.p;
    const p = get('/api/devices/' + id + '/health');
    healthCache[id] = { at: Date.now(), p };
    p.catch(() => { delete healthCache[id]; });
    return p;
  }
  function pct(v) { return v === undefined || v === null ? '—' : (Math.round(v * 10) / 10) + '%'; }
  function pctClass(v, warn, bad) { return v === undefined || v === null ? '' : v >= bad ? ' bad' : v >= warn ? ' warn' : ' ok'; }
  // healthCard renders the summary as HTML. Every number here is the last
  // poll's value straight from the store; nothing is smoothed or estimated.
  function healthCard(hh, compact) {
    const t = hh.traffic || {}, s = hh.interfaces || {};
    const rows = [];
    const stat = `<span class="sdot ${esc(hh.status)}"></span> ${esc(hh.status)}${hh.cause ? ' · upstream ' + esc(hh.cause) + ' is down' : ''}${hh.open_alerts ? ` · <span class="sev ${esc(hh.worst_severity || '')}">${hh.open_alerts} alert${hh.open_alerts > 1 ? 's' : ''}</span>` : ''}`;
    rows.push(`<div class="hc-head"><b>${esc(hh.name)}</b><span class="m">${esc(hh.ip || '')}${hh.model || hh.vendor ? ' · ' + esc(hh.model || hh.vendor) : ''} · ${esc(hh.role)}</span><span class="m">${stat}</span></div>`);
    if (!hh.monitored) rows.push('<div class="m">not monitored (over licence cap or disabled)</div>');
    const g = [];
    g.push(`<span class="hk">CPU</span><span class="hv${pctClass(hh.cpu_pct, 70, 85)}">${pct(hh.cpu_pct)}</span>`);
    g.push(`<span class="hk">Mem</span><span class="hv${pctClass(hh.mem_pct, 80, 90)}">${pct(hh.mem_pct)}</span>`);
    if (hh.temp_c !== undefined && hh.temp_c !== null) g.push(`<span class="hk">Temp</span><span class="hv${pctClass(hh.temp_c, 60, 70)}">${Math.round(hh.temp_c)} °C</span>`);
    g.push(`<span class="hk">RTT</span><span class="hv">${hh.rtt_ms === undefined || hh.rtt_ms === null ? '—' : (Math.round(hh.rtt_ms * 10) / 10) + ' ms'}</span>`);
    g.push(`<span class="hk">Loss</span><span class="hv${pctClass(hh.loss_pct, 5, 20)}">${pct(hh.loss_pct)}</span>`);
    rows.push(`<div class="hc-grid">${g.join('')}</div>`);
    const ifl = `${s.up || 0} up · <span class="${s.down ? 'bad' : ''}">${s.down || 0} down</span>${s.admin_down ? ' · ' + s.admin_down + ' shut' : ''} · ${s.total || 0} total${s.important_down ? ` · <span class="bad">${s.important_down} uplink${s.important_down > 1 ? 's' : ''} down</span>` : ''}`;
    rows.push(`<div class="hc-sec"><span class="hk">Interfaces</span><span class="hv">${ifl}</span></div>`);
    if (t.have_rates) {
      const drops = (t.in_drop_ps || 0) + (t.out_drop_ps || 0), errs = (t.in_err_ps || 0) + (t.out_err_ps || 0);
      rows.push(`<div class="hc-grid tr"><span class="hk">In</span><span class="hv">${fmtBps(t.in_bps)} <span class="faint">·</span> ${fmtPps(t.in_pps)}</span>` +
        `<span class="hk">Out</span><span class="hv">${fmtBps(t.out_bps)} <span class="faint">·</span> ${fmtPps(t.out_pps)}</span>` +
        `<span class="hk">Drops</span><span class="hv${drops > 0 ? ' warn' : ''}">${drops > 0 ? drops.toFixed(drops < 10 ? 1 : 0) + ' pkt/s' : '0'}</span>` +
        `<span class="hk">Errors</span><span class="hv${errs > 0 ? ' warn' : ''}">${errs > 0 ? errs.toFixed(errs < 10 ? 1 : 0) + ' /s' : '0'}</span></div>`);
    } else {
      rows.push('<div class="m">rates after the next poll</div>');
    }
    if (hh.top_util && hh.top_util.length) {
      rows.push('<div class="hc-sec"><span class="hk">Busiest</span><span class="hv hc-list">' + hh.top_util.map(i => { const u = Math.max(i.in_util_pct || 0, i.out_util_pct || 0); return `<span><span class="mono">${esc(i.name)}</span> <span class="${u >= 85 ? 'bad' : u >= 60 ? 'warn' : ''}">${u < 1 && u > 0 ? '<1' : Math.round(u)}%</span>${i.drop_ps ? ` <span class="warn">drop ${i.drop_ps.toFixed(1)}/s</span>` : ''}${i.err_ps ? ` <span class="warn">err ${i.err_ps.toFixed(1)}/s</span>` : ''}</span>`; }).join('') + '</span></div>');
    }
    if (hh.down && hh.down.length) {
      rows.push('<div class="hc-sec"><span class="hk bad">Down</span><span class="hv">' + hh.down.map(i => `<span class="mono">${esc(i.name)}</span>${i.important ? '★' : ''}${i.alias && !compact ? ' <span class="faint">' + esc(i.alias) + '</span>' : ''}`).join(', ') + (hh.down_more ? ` <span class="faint">+${hh.down_more} more</span>` : '') + '</span></div>');
    }
    if (!compact) rows.push(`<div class="m">polled ${ago(hh.last_poll)} · up ${fmtDur(hh.uptime_s || 0)}</div>`);
    return rows.join('');
  }

  // ---------- topology ----------
  async function pageTopology(main) {
    main.className = 'main flush';
    let mode = '3d', overlay = 'status', showGuess = false;
    try { mode = localStorage.getItem('topolight-topo-mode') || '3d'; } catch (e) { }
    const siteSel = h('select', { class: 'btn', 'aria-label': 'Site', onchange: e => { state.siteFilter = e.target.value; $('#site-filter').value = state.siteFilter; load(); } },
      h('option', { value: '' }, 'All sites'), ...state.sites.map(s => h('option', { value: s.id, selected: s.id === state.siteFilter }, s.name)));
    const modeSeg = seg([['3d', '3D'], ['2d', '2D']], mode, v => { mode = v; view.opts.mode = v; try { localStorage.setItem('topolight-topo-mode', v); } catch (e) { } });
    const ovSeg = seg([['status', 'Status'], ['util', 'Utilisation']], overlay, v => { overlay = v; view.opts.overlay = v; });
    const guess = h('label', { class: 'chip', style: 'cursor:pointer' }, h('input', { type: 'checkbox', onchange: e => { showGuess = e.target.checked; view.opts.showGuess = showGuess; } }), 'show low-confidence links');
    const ver = h('span', { class: 'chip' }, '—');
    const bar = h('div', { class: 'topo-bar' }, h('h2', null, 'Topology'), siteSel, modeSeg, ovSeg, guess, h('div', { class: 'spacer' }), ver,
      h('button', { class: 'btn sm', onclick: async () => { await post('/api/topology/rebuild'); toast('Re-reading LLDP/CDP tables…', 'ok'); } }, 'Rebuild now'));
    const canvas = h('canvas');
    const tip = h('div', { class: 'tip', style: 'display:none' });
    const wrap = h('div', { class: 'topo-wrap' }, canvas, tip,
      h('div', { class: 'tiers' }, h('span', null, 'core / router / firewall'), h('span', null, 'distribution'), h('span', null, 'access'), h('span', null, 'ap / server / other')),
      h('div', { class: 'legend' }, leg('up', 'Up'), leg('degraded', 'Degraded'), leg('down', 'Down'), leg('unreachable', 'Unreachable (suppressed)'), leg('maintenance', 'Maintenance'), h('span', { class: 'hint' }, 'drag = orbit · shift-drag = pan · scroll = zoom · double-click = reset')));
    function leg(cls, label) { return h('span', null, h('span', { class: 'sdot ' + cls }), label); }
    main.append(bar, wrap);
    let side = null, sideHealth = null;
    // hover: the identity line appears at once; the health card follows as
    // soon as /health answers (debounced so sweeping across nodes is free).
    let hoverId = null, hoverTimer = null;
    function placeTip(e) {
      const r = wrap.getBoundingClientRect();
      let x = e.clientX - r.left + 14, y = e.clientY - r.top + 14;
      const tw = tip.offsetWidth || 300, th = tip.offsetHeight || 160;
      if (x + tw > r.width - 8) x = Math.max(8, e.clientX - r.left - tw - 14);
      if (y + th > r.height - 8) y = Math.max(8, r.height - th - 8);
      tip.style.left = x + 'px'; tip.style.top = y + 'px';
    }
    function hoverHealth(n) {
      const id = n.id;
      deviceHealth(id).then(hh => { if (hoverId !== id) return; tip.innerHTML = healthCard(hh, true); }).catch(() => { });
    }
    const view = new TopoView(canvas, {
      mode, overlay, showGuess,
      onHover: (n, e) => {
        if (!n) { tip.style.display = 'none'; hoverId = null; clearTimeout(hoverTimer); return; }
        tip.style.display = 'block';
        if (n.id !== hoverId) {
          hoverId = n.id; clearTimeout(hoverTimer);
          tip.innerHTML = `<div class="hc-head"><b>${esc(n.name)}</b><span class="m">${esc(n.ip || '')} ${esc(n.model || n.vendor || '')}</span><span class="m"><span class="sdot ${esc(n.status)}"></span> ${esc(n.status)}${n.cause ? ' · caused by upstream' : ''}${n.alerts ? ' · ' + n.alerts + ' alert(s)' : ''}</span></div>` + (n.external ? '' : '<div class="m">loading health…</div>');
          if (!n.external) hoverTimer = setTimeout(() => hoverHealth(n), 120);
        }
        placeTip(e);
      },
      onSelect: n => { if (!n || n.external) { if (side) { side.remove(); side = null; } return; } showSide(n.id); }
    });
    const ro = new ResizeObserver(() => view.resize()); ro.observe(wrap);
    async function load() {
      const t = await get('/api/topology' + (state.siteFilter ? '?site=' + state.siteFilter : ''));
      view.setData(t.nodes, t.links);
      ver.textContent = `topology v${t.version} · ${t.nodes.length} nodes · ${t.links.length} links`;
      if (!t.nodes.length) { toast('No devices yet for this view — run discovery from Admin → Sites.', 'ok'); }
    }
    async function showSide(id) {
      const d = await get('/api/devices/' + id);
      if (side) side.remove();
      const dev = d.device;
      const imp = d.interfaces.filter(i => i.important || i.status === 'down' && i.admin_up).slice(0, 8);
      const healthBox = h('div', { class: 'hc', style: 'margin:10px 0' }, 'loading health…');
      sideHealth = () => deviceHealth(dev.id).then(hh => { healthBox.innerHTML = healthCard(hh, false); }).catch(() => { });
      sideHealth();
      side = h('div', { class: 'topo-side' },
        h('button', { class: 'iconbtn close', onclick: () => { side.remove(); side = null; view.selected = null; } }, '×'),
        h('div', { style: 'display:flex;gap:8px;align-items:center;margin-bottom:6px' }, h('span', { class: 'sdot ' + dev.status }), h('h2', null, dev.name), h('span', { class: 'badge ' + dev.status }, dev.status)),
        h('div', { class: 'muted small' }, `${dev.model || dev.vendor || '—'} · ${dev.ip} · ${dev.role} · up ${fmtDur(dev.uptime_s || 0)}`),
        dev.cause ? h('div', { class: 'small', style: 'color:var(--unk);margin-top:4px' }, 'Unreachable — upstream ' + (d.cause_name || '') + ' is down.') : null,
        healthBox,
        h('div', { style: 'display:flex;gap:6px;margin-bottom:10px' }, h('a', { class: 'btn sm', href: '#/device/' + dev.id }, 'Open device'), h('a', { class: 'btn sm', href: '#/logs/' + dev.id }, 'Logs'), h('button', { class: 'btn sm', onclick: () => post('/api/devices/' + dev.id + '/poll').then(() => toast('Poll queued', 'ok')) }, 'Poll now')),
        h('h3', null, 'Links'), h('table', { class: 'tbl' }, h('tbody', null, ...d.links.map(l => { const other = l.a_device === dev.id ? l.b_device : l.a_device; const lif = l.a_device === dev.id ? l.a_if : l.b_if; const oif = l.a_device === dev.id ? l.b_if : l.a_if; return h('tr', null, h('td', null, h('span', { class: 'sdot ' + (l.status || 'up') }), ' ', h('span', { class: 'mono' }, lif)), h('td', null, '→ ' + (l.external ? l.external_name : d.names[other] || other) + ' ', h('span', { class: 'mono faint' }, oif)), h('td', { class: 'r num small' }, (l.util_pct || 0).toFixed(0) + '%', l.confidence < 0.8 ? h('span', { class: 'faint' }, ' ?') : null)); }))),
        d.alerts.length ? [h('h3', { style: 'margin-top:10px' }, 'Open alerts'), h('div', { class: 'list' }, ...d.alerts.map(a => alertItem(a, {}, () => nav('#/alerts/' + a.id))))] : null,
        imp.length ? [h('h3', { style: 'margin-top:10px' }, 'Important interfaces'), h('table', { class: 'tbl' }, h('tbody', null, ...imp.map(i => h('tr', null, h('td', null, h('span', { class: 'sdot ' + i.status }), ' ', h('span', { class: 'mono' }, i.name)), h('td', { class: 'small muted' }, i.alias), h('td', { class: 'r num small' }, fmtBps(i.in_bps) + ' / ' + fmtBps(i.out_bps))))))] : null);
      wrap.append(side);
    }
    function mini(l, v, u) { return h('div', { class: 'tile', style: 'padding:8px 10px' }, h('span', { class: 'l' }, l), h('span', { class: 'v', style: 'font-size:18px' }, v === undefined || v === null ? '—' : (Math.round(v * 10) / 10) + (v === undefined ? '' : u))); }
    await load();
    return {
      destroy() { view.destroy(); ro.disconnect(); },
      theme() { view._colors(); },
      onChange(type, data) {
        if (type === 'device') {
          view.updateNode({ id: data.id, name: data.name, status: data.status, role: data.role, cause: data.cause, cpu: data.metrics && data.metrics.cpu_pct, monitored: data.monitored });
          delete healthCache[data.id];
          if (side && sideHealth && view.selected === data.id) sideHealth();
          if (hoverId === data.id) hoverHealth({ id: data.id });
        }
        if (type === 'interface' && data.device_id) { delete healthCache[data.device_id]; }
        if (type === 'link') view.updateLink(data);
        if (type === 'topology') load();
      }
    };
  }

  function seg(items, cur, onchange) {
    const el = h('div', { class: 'seg' });
    items.forEach(([v, label]) => el.append(h('button', { class: v === cur ? 'on' : '', onclick: e => { el.querySelectorAll('button').forEach(b => b.classList.remove('on')); e.target.classList.add('on'); onchange(v); } }, label)));
    return el;
  }

  // ---------- alerts ----------
  async function pageAlerts(main, params) {
    let filt = { state: 'active', severity: '', root: true };
    let selectedId = params[0] || null;
    const list = h('div', { class: 'list' });
    const detail = h('div', { class: 'panel' }, h('div', { class: 'empty' }, 'Select an alert to see evidence, impact and actions.'));
    const chips = h('div', { class: 'chips' });
    let alerts = [], names = {};
    function chip(label, on, click) { return h('button', { class: 'chip' + (on ? ' on' : ''), onclick: click }, label); }
    function renderChips() {
      chips.innerHTML = '';
      chips.append(chip('Active', filt.state === 'active', () => { filt.state = 'active'; load(); }), chip('Acked', filt.state === 'acked', () => { filt.state = 'acked'; load(); }), chip('Resolved (24h)', filt.state === 'resolved', () => { filt.state = 'resolved'; load(); }),
        h('span', { style: 'width:8px' }), ...['critical', 'major', 'minor', 'info'].map(s => chip(cap(s), filt.severity === s, () => { filt.severity = filt.severity === s ? '' : s; load(); })),
        h('span', { style: 'width:8px' }), chip('Root causes only', filt.root, () => { filt.root = !filt.root; load(); }));
    }
    async function load() {
      renderChips();
      const r = await get(`/api/alerts?state=${filt.state}&severity=${filt.severity}&root_only=${filt.root}` + (state.siteFilter ? '&site=' + state.siteFilter : ''));
      alerts = r.alerts; names = r.names;
      renderList();
      if (selectedId) showDetail(selectedId);
    }
    function renderList() {
      list.innerHTML = '';
      if (!alerts.length) { list.append(h('div', { class: 'empty' }, h('h3', null, 'Nothing here'), 'No alerts match these filters.')); return; }
      alerts.forEach(a => list.append(alertItem(a, names, () => { selectedId = a.id; location.hash = '#/alerts/' + a.id; renderList(); showDetail(a.id); }, a.id === selectedId)));
    }
    async function showDetail(id) {
      let a = alerts.find(x => x.id === id);
      if (!a) { try { a = (await get('/api/alerts?state=all')).alerts.find(x => x.id === id); } catch (e) { } }
      if (!a) return;
      detail.innerHTML = '';
      const dev = names[a.device_id] || '';
      detail.append(h('div', { class: 'ph' }, h('span', { class: 'badge ' + (a.state === 'resolved' ? 'resolved' : a.severity) }, a.state === 'resolved' ? 'Resolved' : a.severity), h('h2', null, a.title)),
        h('div', { class: 'pb' },
          h('dl', { class: 'kv' }, h('dt', null, 'State'), h('dd', null, a.state + (a.acked_by ? ' by ' + a.acked_by : '')), h('dt', null, 'Site'), h('dd', null, names[a.site_id] || '—'), h('dt', null, 'Device'), h('dd', null, a.device_id ? h('a', { href: '#/device/' + a.device_id }, dev || a.device_id) : '—'),
            h('dt', null, 'Opened'), h('dd', null, dateTime(a.opened_at) + ' (' + ago(a.opened_at) + ')'), a.state === 'resolved' ? [h('dt', null, 'Resolved'), h('dd', null, dateTime(a.resolved_at) + ' · duration ' + fmtDur((new Date(a.resolved_at) - new Date(a.opened_at)) / 1000))] : null,
            h('dt', null, 'Occurrences'), h('dd', null, a.occurrences), a.impact ? [h('dt', null, 'Impact'), h('dd', null, a.impact)] : null, a.children ? [h('dt', null, 'Folded'), h('dd', null, a.children + ' downstream alert(s)')] : null, a.root_cause ? [h('dt', null, 'Root cause'), h('dd', null, h('a', { href: '#/alerts/' + a.root_cause }, 'open root alert'))] : null, h('dt', null, 'Rule'), h('dd', null, h('a', { href: '#/admin/rules' }, a.rule))),
          a.detail ? h('p', { class: 'muted', style: 'margin:10px 0' }, a.detail) : null,
          h('h3', null, 'Evidence'), h('div', { class: 'evidence', style: 'margin:6px 0 12px' }, ...(a.evidence || []).map(e => h('span', null, e))),
          a.ack_note ? h('p', { class: 'small' }, h('b', null, 'Note: '), a.ack_note) : null,
          a.state !== 'resolved' ? h('div', { style: 'display:flex;gap:8px;flex-wrap:wrap' },
            a.state === 'open' ? h('button', { class: 'btn primary', onclick: () => ackDialog(a) }, 'Acknowledge') : null,
            h('button', { class: 'btn', onclick: async () => { await post('/api/alerts/' + a.id + '/resolve'); toast('Resolved', 'ok'); load(); } }, 'Resolve'),
            a.device_id ? h('button', { class: 'btn', onclick: () => maintenanceDialog(a.device_id, dev) }, 'Maintenance 2h') : null,
            a.device_id ? h('a', { class: 'btn', href: '#/logs/' + a.device_id }, 'Logs ±') : null,
            a.device_id ? h('a', { class: 'btn', href: '#/topology' }, 'On map') : null) : null,
          a.device_id ? h('div', { style: 'margin-top:14px' }, h('h3', null, 'Last hour'), h('canvas', { class: 'chart', id: 'alert-chart' })) : null));
      if (a.device_id) {
        const series = a.object && a.object.includes(':') ? ['if_in_bps|' + a.object, 'if_out_bps|' + a.object] : ['icmp_rtt_ms|' + a.device_id, 'icmp_loss_pct|' + a.device_id];
        try {
          const m = await get('/api/metrics?from=1h&' + series.map(s => 'series=' + encodeURIComponent(s)).join('&'));
          const isIf = series[0].startsWith('if_');
          lineChart($('#alert-chart', detail), series.map((s, i) => ({ pts: m.series[s], color: i ? cssVar('--major') : cssVar('--accent'), label: isIf ? (i ? 'out' : 'in') : (i ? 'loss %' : 'rtt ms') })), { fmt: isIf ? fmtBps : v => v.toFixed(0) });
        } catch (e) { }
      }
    }
    function ackDialog(a) {
      const note = h('textarea', { rows: 3, placeholder: 'Optional note (ticket id, what you did)…' });
      const m = modal('Acknowledge alert', h('div', { class: 'form' }, h('p', { class: 'muted' }, a.title), h('label', null, 'Note', note), h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => { await post('/api/alerts/' + a.id + '/ack', { note: note.value }); m.close(); toast('Acknowledged', 'ok'); load(); } }, 'Acknowledge'))));
    }
    main.append(h('div', { class: 'page-head' }, h('h1', null, 'Alerts'), h('div', { class: 'spacer' }), h('span', { class: 'hint' }, 'j/k move · a ack · r resolve')), chips, h('div', { class: 'split', style: 'margin-top:12px' }, h('div', { class: 'panel', style: 'padding:6px' }, list), detail));
    await load();
    const key = e => {
      if (e.target.matches('input,textarea,select') || !alerts.length) return;
      const idx = alerts.findIndex(x => x.id === selectedId);
      if (e.key === 'j' || e.key === 'k') { const n = Math.max(0, Math.min(alerts.length - 1, idx + (e.key === 'j' ? 1 : -1))); selectedId = alerts[n].id; renderList(); showDetail(selectedId); }
      if (e.key === 'a' && idx >= 0 && alerts[idx].state === 'open') ackDialog(alerts[idx]);
      if (e.key === 'r' && idx >= 0) { post('/api/alerts/' + alerts[idx].id + '/resolve').then(load); }
    };
    document.addEventListener('keydown', key);
    return { destroy() { document.removeEventListener('keydown', key); }, onChange(type) { if (type === 'alert') load(); } };
  }

  function maintenanceDialog(deviceID, name) {
    const hours = h('input', { type: 'number', value: 2, min: 1, max: 720 });
    const reason = h('input', { placeholder: 'Reason' });
    const m = modal('Maintenance window', h('div', { class: 'form' }, h('p', { class: 'muted' }, 'Silence alerts for ' + (name || 'this device') + '.'), h('div', { class: 'row' }, h('label', null, 'Hours', hours), h('label', null, 'Reason', reason)),
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', onclick: async () => {
        const from = new Date(), to = new Date(Date.now() + Number(hours.value) * 3600 * 1000);
        try { await post('/api/maintenance', { name: 'Maintenance ' + (name || ''), devices: [deviceID], from: from.toISOString(), to: to.toISOString(), reason: reason.value }); toast('Maintenance window created', 'ok'); m.close(); }
        catch (x) { toast(x.message, 'err'); }
      } }, 'Create'))));
  }

  // ---------- devices ----------
  async function pageDevices(main, params) {
    const statusFilter = params[0] || '';
    const q = h('input', { placeholder: 'Filter by name, IP, model…', style: 'max-width:280px' });
    const sel = h('select', { class: 'btn', 'aria-label': 'Status filter' }, h('option', { value: '' }, 'Any status'), ...['up', 'degraded', 'down', 'unreachable', 'flapping', 'unknown', 'maintenance'].map(s => h('option', { value: s, selected: s === statusFilter }, cap(s))));
    const tbody = h('tbody');
    let devices = [];
    async function load() {
      devices = await get('/api/devices' + (state.siteFilter ? '?site=' + state.siteFilter : ''));
      render();
    }
    function render() {
      const text = q.value.toLowerCase(), st = sel.value === 'down' ? ['down', 'unreachable', 'flapping'] : sel.value ? [sel.value] : null;
      tbody.innerHTML = '';
      const rows = devices.filter(d => (!text || (d.name + ' ' + d.ip + ' ' + (d.model || '') + ' ' + (d.vendor || '') + ' ' + (d.location || '')).toLowerCase().includes(text)) && (!st || st.includes(d.status)));
      rows.sort((a, b) => rank(b.status) - rank(a.status) || a.name.localeCompare(b.name));
      rows.forEach(d => tbody.append(h('tr', { class: 'row', onclick: () => nav('#/device/' + d.id) },
        h('td', null, h('span', { class: 'sdot ' + d.status, style: 'margin-right:8px' }), h('b', null, d.name), d.monitored ? null : h('span', { class: 'badge unknown', style: 'margin-left:6px' }, 'off')),
        h('td', { class: 'mono' }, d.ip), h('td', null, (state.sites.find(s => s.id === d.site_id) || {}).name || '—'), h('td', null, d.role), h('td', null, d.vendor || '', h('span', { class: 'muted' }, d.model ? ' ' + d.model : '')),
        h('td', { class: 'num small' }, d.metrics && d.metrics.cpu_pct !== undefined ? d.metrics.cpu_pct.toFixed(0) + '%' : '—'), h('td', { class: 'num small' }, d.metrics && d.metrics.rtt_ms !== undefined ? d.metrics.rtt_ms.toFixed(1) + ' ms' : '—'),
        h('td', { class: 'small muted' }, fmtDur(d.uptime_s || 0)), h('td', { class: 'small muted' }, ago(d.last_poll)))));
      if (!rows.length) tbody.append(h('tr', null, h('td', { colspan: 9 }, h('div', { class: 'empty' }, h('h3', null, devices.length ? 'No match' : 'No devices yet'), devices.length ? 'Try another filter.' : h('span', null, 'Run discovery from ', h('a', { href: '#/admin/sites' }, 'Admin → Sites'), ' or add one manually.')))));
    }
    function rank(s) { return { down: 6, flapping: 5, unreachable: 4, degraded: 3, unknown: 2, maintenance: 1, up: 0 }[s] || 0; }
    q.oninput = render; sel.onchange = render;
    main.append(h('div', { class: 'page-head' }, h('h1', null, 'Devices'), q, sel, h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => addDeviceDialog(load) }, '+ Add device')),
      h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Device'), h('th', null, 'IP'), h('th', null, 'Site'), h('th', null, 'Role'), h('th', null, 'Vendor / model'), h('th', null, 'CPU'), h('th', null, 'RTT'), h('th', null, 'Uptime'), h('th', null, 'Last poll'))), tbody)));
    await load();
    return { onChange(type, data) { if (type === 'device') { const i = devices.findIndex(d => d.id === data.id); if (i >= 0) devices[i] = data; else devices.push(data); render(); } } };
  }

  function addDeviceDialog(done) {
    const ip = h('input', { placeholder: '10.20.1.5', required: true }), name = h('input', { placeholder: 'optional — taken from sysName' });
    const site = h('select', null, ...state.sites.map(s => h('option', { value: s.id, selected: s.id === state.siteFilter }, s.name)));
    const po = h('input', { type: 'checkbox' });
    const cred = h('select', null, h('option', { value: '' }, 'site default / any SNMP credential'));
    get('/api/creds').then(cs => cs.filter(c => c.kind !== 'ssh').forEach(c => cred.append(h('option', { value: c.id }, c.name + (c.kind === 'gnmi' ? ' (gNMI)' : ' (SNMP v' + c.version + ')')))));
    const err = h('div', { class: 'err' });
    const m = modal('Add a device', h('form', { class: 'form', onsubmit: async e => {
      e.preventDefault();
      try { const r = await post('/api/devices', { IP: ip.value.trim(), SiteID: site.value, Name: name.value.trim(), CredID: cred.value, ping_only: po.checked }); toast(r.warning ? r.warning : 'Added ' + r.device.name, r.warning ? 'major' : 'ok'); m.close(); done && done(); }
      catch (x) { err.textContent = x.message; }
    } }, h('div', { class: 'row' }, h('label', null, 'IP address', ip), h('label', null, 'Site', site)), h('div', { class: 'row' }, h('label', null, 'Name', name), h('label', null, 'Credential', cred)), h('label', { class: 'check' }, po, ' Ping only — no SNMP (a server, printer or appliance you just want up/down and latency for)'), err, h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Add'))));
  }

  async function pageDevice(main, params) {
    const id = params[0];
    let d = await get('/api/devices/' + id);
    let tab = params[1] || 'interfaces';
    const dev = d.device;
    const head = h('div', { class: 'page-head' });
    const tiles = h('div', { class: 'grid c4' });
    const body = h('div');
    const tabs = h('div', { class: 'tabs' });
    function renderHead() {
      const dv = d.device;
      head.innerHTML = '';
      head.append(h('span', { class: 'sdot ' + dv.status, style: 'width:14px;height:14px' }), h('h1', null, dv.name), h('span', { class: 'badge ' + dv.status }, dv.status),
        h('span', { class: 'sub' }, `${dv.vendor || ''} ${dv.model || ''} · ${dv.ip} · ${(state.sites.find(s => s.id === dv.site_id) || {}).name || ''} · ${dv.role} · ${dv.os_version ? 'v' + dv.os_version + ' · ' : ''}${dv.serial ? 'SN ' + dv.serial + ' · ' : ''}up ${fmtDur(dv.uptime_s || 0)}`),
        dv.cause ? h('span', { class: 'badge unreachable' }, 'upstream ' + (d.cause_name || '') + ' down') : null, dv.monitored ? null : h('span', { class: 'badge minor' }, dv.notes || 'not monitored'), dv.ping_only ? h('span', { class: 'badge unknown', title: 'ICMP only — no SNMP, no interfaces' }, 'ping only') : null,
        h('div', { class: 'spacer' }),
        h('button', { class: 'btn sm', onclick: () => post('/api/devices/' + id + '/poll').then(() => toast('Poll queued', 'ok')) }, 'Poll now'),
        h('button', { class: 'btn sm', onclick: () => maintenanceDialog(id, dv.name) }, 'Maintenance'),
        h('button', { class: 'btn sm', onclick: () => editDevice() }, 'Edit'),
        state.user.role === 'admin' ? h('button', { class: 'btn sm danger', onclick: async () => { if (confirm('Delete ' + dv.name + ' and its history from TopoLight?')) { await del('/api/devices/' + id); nav('#/devices'); } } }, 'Delete') : null);
    }
    async function renderTiles() {
      tiles.innerHTML = '';
      const m = d.device.metrics || {};
      const defs = [['CPU', 'cpu_pct', '%', 'cpu_pct', 85], ['Memory', 'mem_pct', '%', 'mem_pct', 90], ['RTT', 'rtt_ms', ' ms', 'icmp_rtt_ms', 150], ['Packet loss', 'loss_pct', '%', 'icmp_loss_pct', 20]];
      const cans = [];
      defs.forEach(([l, k, u, series, thr]) => { const c = h('canvas'); cans.push([c, series, thr]); tiles.append(h('div', { class: 'tile' }, h('span', { class: 'l' }, l), h('span', { class: 'v' }, m[k] === undefined ? '—' : (Math.round(m[k] * 10) / 10), h('small', null, m[k] === undefined ? '' : u)), c)); });
      try {
        const r = await get('/api/metrics?from=24h&' + cans.map(c => 'series=' + encodeURIComponent(c[1] + '|' + id)).join('&'));
        cans.forEach(([c, s, thr]) => sparkline(c, r.series[s + '|' + id], cssVar('--accent'), { min: 0, threshold: thr }));
      } catch (e) { }
    }
    function renderTabs() {
      tabs.innerHTML = '';
      [['interfaces', 'Interfaces (' + d.interfaces.length + ')'], ['links', 'Links & neighbours'], ['alerts', 'Alerts & events'], ['flow', 'Flow'], ['endpoints', 'Endpoints'], ['routing', 'Routing & L2'], ...(d.device.managed || d.has_wireless ? [['wireless', 'Wireless / WAN']] : []), ['config', 'Config backup'], ['snippets', 'Config snippets']].forEach(([k, l]) => tabs.append(h('button', { class: k === tab ? 'on' : '', onclick: () => { tab = k; location.hash = '#/device/' + id + '/' + k; renderBody(); } }, l)));
    }
    async function renderBody() {
      renderTabs();
      body.innerHTML = '';
      if (tab === 'interfaces') {
        const ifs = d.interfaces.slice().sort((a, b) => (b.important - a.important) || a.ifindex - b.ifindex);
        const chart = h('canvas', { class: 'chart' });
        const chartBody = h('div', { class: 'pb', hidden: true }, chart);
        const chartPanel = h('div', { class: 'panel', style: 'margin-bottom:12px' }, h('div', { class: 'ph' }, h('h3', { id: 'ifc-title' }, 'Pick an interface for its 24-hour graph')), chartBody);
        const tb = h('tbody');
        ifs.forEach(i => tb.append(h('tr', { class: 'row', onclick: async () => {
          $('#ifc-title', chartPanel).textContent = i.name + (i.alias ? ' — ' + i.alias : '') + ' · 24h';
          chartBody.hidden = false;
          const r = await get(`/api/metrics?from=24h&series=${encodeURIComponent('if_in_bps|' + i.id)}&series=${encodeURIComponent('if_out_bps|' + i.id)}`);
          lineChart(chart, [{ pts: r.series['if_in_bps|' + i.id], color: cssVar('--accent'), label: 'in' }, { pts: r.series['if_out_bps|' + i.id], color: cssVar('--major'), label: 'out' }], { fmt: fmtBps, max: i.speed_mbps ? 0 : 0 });
        } },
          h('td', null, h('span', { class: 'star' + (i.important ? ' on' : ''), title: 'Important: alert when down', onclick: async e => { e.stopPropagation(); const r = await put('/api/interfaces/' + i.id, { important: !i.important }); i.important = r.important; e.target.classList.toggle('on', i.important); } }, '★'), ' ', h('span', { class: 'sdot ' + (i.admin_up ? i.status : 'unknown') }), ' ', h('span', { class: 'name' }, i.name)),
          h('td', { class: 'small muted' }, i.alias), h('td', { class: 'small' }, i.speed_mbps ? (i.speed_mbps >= 1000 ? i.speed_mbps / 1000 + 'G' : i.speed_mbps + 'M') : '—'),
          h('td', null, util(i.in_util_pct), ' ', h('span', { class: 'small num' }, fmtBps(i.in_bps))), h('td', null, util(i.out_util_pct), ' ', h('span', { class: 'small num' }, fmtBps(i.out_bps))),
          h('td', { class: 'small num', style: (i.in_err_rate + i.out_err_rate) > 0 ? 'color:var(--major)' : '' }, ((i.in_err_rate || 0) + (i.out_err_rate || 0)).toFixed(2) + '/s'), h('td', { class: 'small muted' }, i.admin_up ? (i.oper_up ? 'up' : 'down') : 'admin down'), h('td', { class: 'small muted' }, isZero(i.last_change) ? '—' : ago(i.last_change)))));
        body.append(chartPanel, h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl iftbl' }, h('thead', null, h('tr', null, h('th', null, 'Interface'), h('th', null, 'Description'), h('th', null, 'Speed'), h('th', null, 'In'), h('th', null, 'Out'), h('th', null, 'Errors'), h('th', null, 'State'), h('th', null, 'Changed'))), tb)));
        if (!ifs.length) body.append(h('div', { class: 'empty' }, d.device.ping_only ? 'Ping-only device: no SNMP, so no interfaces. Edit the device and untick "Ping only" once it has an SNMP credential.' : 'No interfaces yet — the inventory walk runs on the first poll.'));
      } else if (tab === 'links') {
        body.append(h('div', { class: 'grid c2' },
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Links (' + d.links.length + ')')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Local'), h('th', null, 'Remote'), h('th', null, 'Confidence'), h('th', null, 'Sources'), h('th', null, 'Util'))), h('tbody', null, ...d.links.map(l => { const mine = l.a_device === id; const other = mine ? l.b_device : l.a_device; return h('tr', null, h('td', { class: 'mono' }, h('span', { class: 'sdot ' + (l.status || 'up') }), ' ', mine ? l.a_if : l.b_if), h('td', null, l.external ? h('span', { class: 'muted' }, l.external_name) : h('a', { href: '#/device/' + other }, d.names[other] || other), ' ', h('span', { class: 'mono faint' }, mine ? l.b_if : l.a_if)), h('td', { class: 'num' }, l.confidence.toFixed(2), l.stale ? h('span', { class: 'badge unknown', style: 'margin-left:6px' }, 'stale') : null), h('td', { class: 'small muted' }, (l.sources || []).join(', ')), h('td', { class: 'num small' }, (l.util_pct || 0).toFixed(0) + '%')); })))),
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Raw neighbour observations (' + d.neighbors.length + ')')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Local port'), h('th', null, 'Remote'), h('th', null, 'Remote port'), h('th', null, 'Via'))), h('tbody', null, ...d.neighbors.map(n => h('tr', null, h('td', { class: 'mono' }, n.local_if), h('td', null, n.remote_name || n.remote_mac || n.remote_ip, h('div', { class: 'small faint mono' }, [n.remote_mac, n.remote_ip].filter(Boolean).join(' · '))), h('td', { class: 'mono small' }, n.remote_port), h('td', { class: 'small muted' }, n.source + ' · ' + ago(n.seen_at)))))))));
      } else if (tab === 'alerts') {
        const evs = await get('/api/events?device=' + id + '&limit=100');
        body.append(h('div', { class: 'grid c2' },
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Open alerts')), h('div', { class: 'list', style: 'padding:6px' }, ...(d.alerts.length ? d.alerts.map(a => alertItem(a, {}, () => nav('#/alerts/' + a.id))) : [h('div', { class: 'empty' }, 'No open alerts')]))),
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Events'), h('div', { class: 'spacer' }), h('a', { class: 'btn sm', href: '#/logs/' + id }, 'Syslog & traps')), h('div', { class: 'list', style: 'padding:6px' }, ...(evs.length ? evs.map(e => h('div', { class: 'item ' + (e.severity || 'info') }, h('span', { class: 'bar' }), h('div', null, h('div', { class: 't' }, h('span', null, dateTime(e.ts)), h('span', null, '· ' + e.source), h('span', null, '· ' + e.kind)), h('div', { class: 'd', style: 'color:var(--text)' }, e.message)))) : [h('div', { class: 'empty' }, 'No events yet')])))));
      } else if (tab === 'flow') {
        let fwin = '1h';
        const chart = h('canvas', { class: 'chart' });
        const tables = h('div');
        const chips = h('div', { class: 'tabs', style: 'margin:0' });
        async function loadFlow() {
          chips.innerHTML = ''; FLOW_WINDOWS.forEach(([v, l]) => chips.append(h('button', { class: v === fwin ? 'on' : '', onclick: () => { fwin = v; loadFlow(); } }, l)));
          tables.innerHTML = '';
          let r, ser;
          try { [r, ser] = await Promise.all([get('/api/flow?device=' + id + '&window=' + fwin), get('/api/flow/series?device=' + id + '&window=' + fwin)]); }
          catch (x) { tables.append(h('div', { class: 'empty' }, x.message)); return; }
          if (!r.summary.bytes) { tables.append(h('div', { class: 'empty' }, h('h3', null, 'No flows from ' + d.device.name + ' in this window'), h('p', null, 'Export NetFlow/IPFIX (UDP ' + (((state.status || {}).collectors || {}).flow_addr || '2055') + ') or sFlow (UDP ' + (((state.status || {}).collectors || {}).sflow_addr || '6343') + ') from ' + d.device.ip + ' to this host — see the Config snippets tab.'))); lineChart(chart, [], {}); return; }
          const pts = (ser.points || []).map(p => ({ t: p.t, v: p.b * 8 / (p.s || 60) }));
          lineChart(chart, [{ pts, color: cssVar('--accent'), label: 'bits/s' }], { fmt: fmtBps });
          tables.append(flowTables(r, { n: 15, onHost: ip => nav('#/flow/' + id + '/' + fwin) }));
        }
        body.append(h('div', { class: 'panel', style: 'margin-bottom:12px' }, h('div', { class: 'ph' }, h('h3', null, 'Throughput seen in flows'), chips, h('div', { class: 'spacer' }), h('a', { class: 'btn sm', href: '#/flow/' + id + '/' + fwin }, 'Open in Flow')), h('div', { class: 'pb' }, chart)), tables);
        await loadFlow();
      } else if (tab === 'endpoints') {
        const r = await get('/api/devices/' + id + '/endpoints');
        const byPort = {};
        r.placed.forEach(e => { (byPort[e.if_name || ('ifIndex ' + e.ifindex)] = byPort[e.if_name || ('ifIndex ' + e.ifindex)] || []).push(e); });
        const ports = Object.keys(byPort).sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
        const tb = h('tbody');
        ports.forEach(pn => {
          tb.append(h('tr', { class: 'grp' }, h('td', { colspan: 6 }, h('b', { class: 'mono' }, pn), h('span', { class: 'muted small' }, ' · ' + byPort[pn].length + ' endpoint' + (byPort[pn].length === 1 ? '' : 's')))));
          byPort[pn].forEach(e => tb.append(epRow(e, {}, { noWhere: true })));
        });
        body.append(h('div', { class: 'panel tbl-wrap' }, h('div', { class: 'ph' }, h('h3', null, 'On access ports (' + r.placed.length + ')'), h('span', { class: 'hint' }, 'from the MAC forwarding table, uplinks excluded; walked every 5 minutes')),
          r.placed.length ? h('table', { class: 'tbl' }, epHead({ noWhere: true }), tb) : h('div', { class: 'empty' }, 'Nothing placed on this device yet — either it has no bridge tables (a router/firewall), every port is an uplink, or the first 5-minute walk has not run.')));
        if (r.resolved.length) body.append(h('div', { class: 'panel tbl-wrap', style: 'margin-top:12px' }, h('div', { class: 'ph' }, h('h3', null, 'Resolved by ARP here (' + r.resolved.length + ')'), h('span', { class: 'hint' }, 'IP ↔ MAC from this device’s ARP/ND table; "Where" shows the switch port when one is known')),
          h('table', { class: 'tbl' }, epHead(), h('tbody', null, ...r.resolved.slice(0, 500).map(e => epRow(e, r.names))))));
      } else if (tab === 'routing') {
        const r = await get('/api/devices/' + id + '/routing');
        const rt = r.routing || {};
        if (!r.has) { body.append(h('div', { class: 'empty' }, h('h3', null, 'No routing or layer-2 tables yet'), 'BGP (BGP4-MIB), OSPF (OSPF-MIB), VLANs and spanning tree (Q-BRIDGE/BRIDGE-MIB) and link aggregation (IEEE8023-LAG-MIB) are walked every 5 minutes; this device answered none of them so far.')); return; }
        const kv = h('div', { class: 'grid c4', style: 'margin-bottom:12px' });
        const tile = (l, v, s) => h('div', { class: 'tile' }, h('span', { class: 'l' }, l), h('span', { class: 'v' }, v, s ? h('small', null, s) : null));
        kv.append(tile('Routes', rt.routes || '—'), tile('BGP peers', (rt.bgp || []).length ? (rt.bgp.filter(b => b.up).length + ' / ' + rt.bgp.length) : '—', rt.local_as ? ' AS ' + rt.local_as : ''), tile('OSPF neighbours', (rt.ospf || []).length ? (rt.ospf.filter(n => n.full).length + ' / ' + rt.ospf.length) : '—', rt.router_id ? ' ' + rt.router_id : ''), tile('VLANs', (rt.vlans || []).length || '—', rt.stp ? (rt.stp.is_root ? ' STP root' : ' STP') : ''));
        body.append(kv);
        const panels = [];
        if ((rt.bgp || []).length) panels.push(h('div', { class: 'panel tbl-wrap' }, h('div', { class: 'ph' }, h('h3', null, 'BGP peers')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Peer'), h('th', null, 'Remote AS'), h('th', null, 'State'), h('th', null, 'Up for'), h('th', { class: 'r' }, 'Prefixes'), h('th', null, 'Last error'))), h('tbody', null, ...rt.bgp.map(b => h('tr', null, h('td', { class: 'mono' }, b.peer), h('td', { class: 'num' }, b.remote_as), h('td', null, h('span', { class: 'badge ' + (b.up ? 'up' : 'down') }, b.state)), h('td', { class: 'small' }, b.up ? fmtDur(b.uptime_s) : '—'), h('td', { class: 'num r' }, b.prefixes || (b.up ? '—' : '')), h('td', { class: 'small muted' }, b.last_error || '')))))));
        if ((rt.ospf || []).length) panels.push(h('div', { class: 'panel tbl-wrap' }, h('div', { class: 'ph' }, h('h3', null, 'OSPF neighbours')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Neighbour'), h('th', null, 'Router ID'), h('th', null, 'State'), h('th', { class: 'r' }, 'Priority'))), h('tbody', null, ...rt.ospf.map(n => h('tr', null, h('td', { class: 'mono' }, n.neighbor), h('td', { class: 'mono' }, n.router_id), h('td', null, h('span', { class: 'badge ' + (n.full ? 'up' : 'minor') }, n.state)), h('td', { class: 'num r' }, n.priority)))))));
        if (rt.stp) { const s = rt.stp; panels.push(h('div', { class: 'panel pb' }, h('h3', null, 'Spanning tree'), h('dl', { class: 'kv', style: 'margin-top:8px' }, h('dt', null, 'Root bridge'), h('dd', { class: 'mono' }, s.root_id + (s.is_root ? ' (this bridge)' : '')), h('dt', null, 'Root port'), h('dd', null, s.is_root ? '—' : (s.root_port || '—') + ' · cost ' + s.root_cost), h('dt', null, 'Ports'), h('dd', null, s.forwarding + ' forwarding, ' + s.blocking + ' blocking'), h('dt', null, 'Topology changes'), h('dd', null, s.top_changes + ' total · last ' + fmtDur(s.last_change_s) + ' ago'), s.blocked_ports && s.blocked_ports.length ? h('dt', null, 'Blocked') : null, s.blocked_ports && s.blocked_ports.length ? h('dd', { class: 'mono small' }, s.blocked_ports.join(', ')) : null))); }
        if ((rt.lags || []).length) panels.push(h('div', { class: 'panel tbl-wrap' }, h('div', { class: 'ph' }, h('h3', null, 'Link aggregation')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Bundle'), h('th', null, 'Members'), h('th', null, 'Up'))), h('tbody', null, ...rt.lags.map(l => h('tr', null, h('td', { class: 'mono' }, l.name), h('td', { class: 'mono small' }, l.members.join(', ')), h('td', null, h('span', { class: 'badge ' + (l.up === l.members.length ? 'up' : l.up ? 'minor' : 'down') }, l.up + ' / ' + l.members.length))))))));
        body.append(h('div', { class: 'grid c2' }, ...panels));
        if ((rt.vlans || []).length) body.append(h('div', { class: 'panel tbl-wrap', style: 'margin-top:12px' }, h('div', { class: 'ph' }, h('h3', null, 'VLANs (' + rt.vlans.length + ')')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'ID'), h('th', null, 'Name'), h('th', { class: 'r' }, 'Ports'), h('th', null, 'Members'))), h('tbody', null, ...rt.vlans.map(v => h('tr', null, h('td', { class: 'num' }, v.id), h('td', null, v.name), h('td', { class: 'num r' }, v.nport), h('td', { class: 'mono small muted' }, (v.ports || []).join(', ') + (v.nport > (v.ports || []).length ? ' …' : ''))))))));
        body.append(h('p', { class: 'small faint', style: 'margin-top:8px' }, 'Walked ' + ago(rt.ts) + '. BGP/OSPF state changes, root-bridge changes, topology changes and LAG member loss raise events and alerts (Admin → Alert rules).'));
      } else if (tab === 'wireless') {
        const r = await get('/api/devices/' + id + '/wireless');
        if (r.has) {
          const w = r.wireless;
          const kv = h('div', { class: 'grid c4', style: 'margin-bottom:12px' });
          const tile = (l, v, s) => h('div', { class: 'tile' }, h('span', { class: 'l' }, l), h('span', { class: 'v' }, v, s ? h('small', null, s) : null));
          kv.append(tile('Clients', w.clients), tile(w.aps ? 'Access points' : 'Radios', w.aps ? w.aps_up + ' / ' + w.aps : (w.radios || []).length), tile('Firmware', w.version || '—', w.upgradable ? ' update available' : ''), tile(w.satisfaction ? 'Satisfaction' : 'Model', w.satisfaction ? w.satisfaction : (w.model || '—'), w.satisfaction ? '%' : ''));
          body.append(kv);
          const c = h('canvas', { class: 'chart' });
          body.append(h('div', { class: 'panel', style: 'margin-bottom:12px' }, h('div', { class: 'ph' }, h('h3', null, 'Clients, 24 h')), h('div', { class: 'pb' }, c)));
          try { const m = await get('/api/metrics?from=24h&series=' + encodeURIComponent('wifi_clients|' + id)); lineChart(c, [{ pts: m.series['wifi_clients|' + id] || [], color: cssVar('--accent'), label: 'clients' }], { fmt: v => v.toFixed(0) }); } catch (e) { }
          if ((w.radios || []).length) body.append(h('div', { class: 'panel tbl-wrap', style: 'margin-bottom:12px' }, h('div', { class: 'ph' }, h('h3', null, 'Radios')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Radio'), h('th', { class: 'r' }, 'Channel'), h('th', { class: 'r' }, 'Width'), h('th', { class: 'r' }, 'Tx power'), h('th', { class: 'r' }, 'Clients'), h('th', { class: 'r' }, 'Utilisation'))), h('tbody', null, ...w.radios.map(x => h('tr', null, h('td', { class: 'mono' }, x.name), h('td', { class: 'num r' }, x.channel || '—'), h('td', { class: 'num r' }, x.width ? x.width + ' MHz' : '—'), h('td', { class: 'num r' }, x.tx_power ? x.tx_power + ' dBm' : x.tx_level ? 'level ' + x.tx_level : '—'), h('td', { class: 'num r' }, x.clients), h('td', { class: 'num r' }, x.util_pct ? Math.round(x.util_pct) + ' %' : '—')))))));
          if (w.ssids && Object.keys(w.ssids).length) body.append(h('div', { class: 'panel pb', style: 'margin-bottom:12px' }, h('h3', null, 'Clients per SSID'), h('div', { class: 'small', style: 'margin-top:6px' }, Object.entries(w.ssids).sort((a, b) => b[1] - a[1]).map(([k, v]) => h('span', { class: 'badge unknown', style: 'margin:2px 6px 2px 0' }, k + ' · ' + v)))));
          body.append(h('p', { class: 'small faint' }, 'Reported ' + ago(w.ts) + (w.controller ? ' by ' + w.controller : '') + '.'));
        }
        if ((r.sdwan || []).length) {
          body.append(h('div', { class: 'panel tbl-wrap' }, h('div', { class: 'ph' }, h('h3', null, 'WAN paths')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Path'), h('th', null, 'Interface'), h('th', null, 'State'), h('th', { class: 'r' }, 'Latency'), h('th', { class: 'r' }, 'Jitter'), h('th', { class: 'r' }, 'Loss'), h('th', null, 'Checked'))), h('tbody', null, ...r.sdwan.map(l => h('tr', null, h('td', null, h('b', null, l.name)), h('td', { class: 'mono small' }, l.interface + (l.ip ? ' · ' + l.ip : '')), h('td', null, h('span', { class: 'badge ' + (l.up ? 'up' : 'down') }, l.state || (l.up ? 'up' : 'down'))), h('td', { class: 'num r' }, l.latency_ms ? l.latency_ms.toFixed(1) + ' ms' : '—'), h('td', { class: 'num r' }, l.jitter_ms ? l.jitter_ms.toFixed(1) + ' ms' : '—'), h('td', { class: 'num r' }, l.loss_pct !== undefined ? l.loss_pct.toFixed(1) + ' %' : '—'), h('td', { class: 'small muted' }, ago(l.ts))))))));
          const c2 = h('canvas', { class: 'chart' });
          if (r.sdwan.some(l => l.latency_ms)) body.append(h('div', { class: 'panel', style: 'margin-top:12px' }, h('div', { class: 'ph' }, h('h3', null, 'Path latency, 24 h')), h('div', { class: 'pb' }, c2)));
          try { const sk = l => 'sdwan_latency|' + id + '|' + l.name + '/' + l.interface; const q = r.sdwan.map(l => 'series=' + encodeURIComponent(sk(l))).join('&'); const m = await get('/api/metrics?from=24h&' + q); const cols = [cssVar('--accent'), cssVar('--major'), cssVar('--ok'), cssVar('--minor'), cssVar('--critical')]; lineChart(c2, r.sdwan.map((l, i) => ({ pts: m.series[sk(l)] || [], color: cols[i % cols.length], label: l.name + ' / ' + l.interface })), { fmt: v => v.toFixed(1) + ' ms' }); } catch (e) { }
        }
        if (!r.has && !(r.sdwan || []).length) body.append(h('div', { class: 'empty' }, h('h3', null, 'Nothing reported yet'), 'Wireless state comes from the controller integration (Admin → Integrations) or from WLC/Aruba controller MIBs; WAN path health from SD-WAN integrations or FortiGate SNMP.'));
      } else if (tab === 'config') {
        const r = await get('/api/devices/' + id + '/configs');
        const st = r.status || {};
        const vs = r.versions || [];
        const head = h('div', { class: 'page-head' }, h('span', { class: 'sub' }, r.has_cred ? `Pulled over SSH every ${r.every_hours} h (and after a "config changed" syslog line) with "${r.recipe.show.split('\n')[0]}". ${vs.length} version${vs.length === 1 ? '' : 's'} kept${st.last_ok ? ' · last OK ' + ago(st.last_ok) : ''}${st.error ? ' · ' : ''}` : 'No SSH credential: add one under Admin → Credentials (type SSH) and pick it on the site or in Edit device.'), st.error ? h('span', { class: 'badge minor' }, st.error) : null, h('div', { class: 'spacer' }),
          state.user.role !== 'viewer' && r.has_cred ? h('button', { class: 'btn sm primary', onclick: async e => { e.target.disabled = true; e.target.textContent = 'Connecting…'; const x = await post('/api/devices/' + id + '/backup'); toast(x.ok ? (x.changed ? 'Stored new version ' + x.version.id : 'Unchanged since ' + x.version.id) : 'Backup failed: ' + x.error, x.ok ? 'ok' : 'err', 6000); renderBody(); } }, 'Backup now') : null);
        body.append(head);
        if (!vs.length) { body.append(h('div', { class: 'empty' }, h('h3', null, 'No configuration stored yet'), r.has_cred ? 'The first backup runs within a few minutes of assigning a credential; use Backup now to fetch one immediately.' : '')); return; }
        let selA = vs[0].id, selB = vs[1] ? vs[1].id : '', showVolatile = false;
        const viewer = h('div');
        const tb = h('tbody');
        function renderList() {
          tb.innerHTML = '';
          vs.forEach((v, i) => tb.append(h('tr', { class: 'row' + (v.id === selA ? ' on' : ''), onclick: () => { selB = v.id === selA ? selB : selA; selA = v.id; renderList(); showDiff(); } },
            h('td', null, h('input', { type: 'radio', name: 'cfgA', checked: v.id === selA, 'aria-label': 'newer', onclick: e => { e.stopPropagation(); selA = v.id; renderList(); showDiff(); } })), h('td', null, h('input', { type: 'radio', name: 'cfgB', checked: v.id === selB, 'aria-label': 'older', onclick: e => { e.stopPropagation(); selB = v.id; renderList(); showDiff(); } })),
            h('td', null, h('b', { class: 'mono' }, dateTime(v.ts)), i === 0 ? h('span', { class: 'badge up', style: 'margin-left:6px' }, 'current') : null), h('td', { class: 'small muted' }, v.source), h('td', { class: 'num r small' }, v.lines + ' lines'),
            h('td', { class: 'small' }, i < vs.length - 1 ? [h('span', { style: 'color:var(--ok)' }, '+' + v.added), ' ', h('span', { style: 'color:var(--major)' }, '−' + v.removed)] : h('span', { class: 'faint' }, 'first')),
            h('td', null, h('button', { class: 'btn sm', onclick: e => { e.stopPropagation(); showFull(v); } }, 'View'), ' ', h('a', { class: 'btn sm', href: '/api/devices/' + id + '/configs/' + v.id + '?raw=1', onclick: e => e.stopPropagation() }, 'Download')))));
        }
        async function showFull(v) {
          const x = await get('/api/devices/' + id + '/configs/' + v.id);
          viewer.innerHTML = '';
          viewer.append(h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Version ' + dateTime(v.ts)), h('span', { class: 'hint' }, v.lines + ' lines · ' + fmtBytes(v.bytes)), h('div', { class: 'spacer' }), h('button', { class: 'btn sm', onclick: () => { navigator.clipboard.writeText(x.text).then(() => toast('Copied', 'ok')); } }, 'Copy')), h('pre', { class: 'snippet cfg' }, x.text)));
        }
        async function showDiff() {
          viewer.innerHTML = '';
          if (!selB || selA === selB) { const v = vs.find(x => x.id === selA); if (v) showFull(v); return; }
          const a = vs.find(x => x.id === selA), b = vs.find(x => x.id === selB);
          const older = a.ts < b.ts ? a : b, newer = a.ts < b.ts ? b : a;
          const d = await get('/api/devices/' + id + '/configs/' + older.id + '/diff/' + newer.id + '?context=3&volatile=' + (showVolatile ? '1' : '0'));
          const pre = h('div', { class: 'diff' });
          if (!d.ops.length) pre.append(h('div', { class: 'empty' }, 'Identical'));
          d.ops.forEach(o => { if (o.k === 64) { pre.append(h('div', { class: 'hunk' }, '⋯')); return; } pre.append(h('div', { class: 'dl ' + (o.k === 43 ? 'add' : o.k === 45 ? 'del' : '') }, h('span', { class: 'ln' }, o.a || ''), h('span', { class: 'ln' }, o.b || ''), h('span', { class: 'mk' }, o.k === 43 ? '+' : o.k === 45 ? '−' : ' '), h('span', { class: 'tx' }, o.l))); });
          viewer.append(h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, dateTime(older.ts) + ' → ' + dateTime(newer.ts)), h('span', { class: 'hint' }, h('span', { style: 'color:var(--ok)' }, '+' + d.added), ' ', h('span', { style: 'color:var(--major)' }, '−' + d.removed), ' lines' + (showVolatile ? '' : ' · timestamps and other volatile lines ignored')), h('div', { class: 'spacer' }), h('label', { class: 'check small' }, h('input', { type: 'checkbox', checked: showVolatile, onchange: e => { showVolatile = e.target.checked; showDiff(); } }), ' show volatile lines')), pre));
        }
        body.append(h('div', { class: 'panel tbl-wrap', style: 'margin-bottom:12px' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', { title: 'newer side of the diff' }, 'A'), h('th', { title: 'older side of the diff' }, 'B'), h('th', null, 'Stored'), h('th', null, 'Trigger'), h('th', { class: 'r' }, 'Size'), h('th', null, 'vs previous'), h('th', null, ''))), tb)), viewer);
        renderList(); showDiff();
      } else if (tab === 'snippets') {
        const sn = await get('/api/snippets?collector=' + encodeURIComponent(location.hostname) + '&cred=' + (d.device.cred_id || ''));
        const key = ({ 'cisco-ios': 'cisco-ios', 'cisco-nxos': 'cisco-nxos', 'cisco-asa': 'cisco-ios', 'fortinet-fortigate': 'fortinet', juniper: 'juniper', mikrotik: 'mikrotik', 'aruba-aos-s': 'aruba', 'aruba-aos-cx': 'aruba' })[d.device.profile_id] || 'cisco-ios';
        const sel = h('select', { class: 'btn', 'aria-label': 'Collector', onchange: () => show(sel.value) }, ...Object.keys(sn).filter(k => !k.startsWith('_')).map(k => h('option', { value: k, selected: k === key }, k)));
        const pre = h('pre', { class: 'snippet' });
        function show(k) { pre.textContent = sn[k]; }
        show(key);
        body.append(h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Device-side configuration'), sel, h('div', { class: 'spacer' }), h('button', { class: 'btn sm', onclick: () => { navigator.clipboard.writeText(pre.textContent).then(() => toast('Copied', 'ok')); } }, 'Copy')),
          h('div', { class: 'pb' }, h('p', { class: 'muted small' }, 'Replace the placeholders in angle brackets. These lines enable read-only SNMP, traps, syslog and LLDP toward this collector (', h('span', { class: 'mono' }, location.hostname), '). ', sn._ports), pre)));
      }
    }
    function util(u) { u = u || 0; return h('span', { class: 'bar' + (u > 85 ? ' sat' : u > 60 ? ' hi' : '') }, h('i', { style: 'width:' + Math.min(100, u) + '%' })); }
    function editDevice() {
      const dv = d.device;
      const name = h('input', { value: dv.name }), role = h('select', null, ...['core', 'distribution', 'access', 'router', 'firewall', 'ap', 'server', 'other'].map(r => h('option', { value: r, selected: r === dv.role }, r)));
      const domain = h('select', null, h('option', { value: 'network', selected: dv.domain === 'network' }, 'network'), h('option', { value: 'security', selected: dv.domain === 'security' }, 'security'));
      const poll = h('input', { type: 'number', value: dv.poll_every, min: 15, max: 3600 }), mon = h('input', { type: 'checkbox', checked: dv.monitored }), po = h('input', { type: 'checkbox', checked: dv.ping_only });
      const site = h('select', null, ...state.sites.map(s => h('option', { value: s.id, selected: s.id === dv.site_id }, s.name)));
      const loc = h('input', { value: dv.location || '' }), notes = h('textarea', { rows: 2 }, dv.notes || '');
      const sshSel = h('select', null, h('option', { value: '' }, 'site default')); get('/api/creds').then(cs => cs.filter(c => c.kind === 'ssh').forEach(c => sshSel.append(h('option', { value: c.id, selected: dv.ssh_cred_id === c.id }, c.name))));
      const bke = h('input', { type: 'number', value: dv.backup_every || 0, min: -1, max: 720, title: '0 = global setting, -1 = never' });
      const err = h('div', { class: 'err' });
      const m = modal('Edit ' + dv.name, h('form', { class: 'form', onsubmit: async e => {
        e.preventDefault();
        try { await put('/api/devices/' + id, { name: name.value, role: role.value, domain: domain.value, poll_every: Number(poll.value), monitored: mon.checked, ping_only: po.checked, site_id: site.value, location: loc.value, notes: notes.value, ssh_cred_id: sshSel.value, backup_every: Number(bke.value) }); m.close(); d = await get('/api/devices/' + id); renderHead(); renderTiles(); }
        catch (x) { err.textContent = x.message; }
      } }, h('div', { class: 'row' }, h('label', null, 'Name', name), h('label', null, 'Role', role), h('label', null, 'Domain', domain)), h('div', { class: 'row' }, h('label', null, 'Poll every (s)', poll), h('label', null, 'Site', site), h('label', null, 'Location', loc)), h('div', { class: 'row' }, h('label', null, 'SSH credential (config backup)', sshSel), h('label', null, 'Backup every (h; 0 = default, -1 = never)', bke)), h('label', { class: 'check' }, mon, ' Monitored'), h('label', { class: 'check' }, po, ' Ping only (ICMP, no SNMP)'), h('label', null, 'Notes', notes), err,
        h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Save'))));
    }
    renderHead(); renderTiles(); renderBody();
    main.append(head, tiles, h('div', { style: 'height:16px' }), tabs, body);
    return { onChange(type, data) { if (type === 'device' && data.id === id) { d.device = data; renderHead(); } if (type === 'interface' && data.device_id === id) { const i = d.interfaces.findIndex(x => x.id === data.id); if (i >= 0) d.interfaces[i] = data; } } };
  }

  // ---------- logs ----------
  async function pageLogs(main, params) {
    const devFilter = params[0] || '';
    const devices = await get('/api/devices');
    const q = h('input', { placeholder: 'Search text or mnemonic…', style: 'max-width:280px' });
    const dev = h('select', { class: 'btn', 'aria-label': 'Device filter' }, h('option', { value: '' }, 'Any device'), ...devices.map(d => h('option', { value: d.id, selected: d.id === devFilter }, d.name)));
    const sev = h('select', { class: 'btn', 'aria-label': 'Severity filter' }, ...[[-1, 'Any severity'], [3, 'Error and worse'], [4, 'Warning and worse'], [5, 'Notice and worse'], [6, 'Info and worse']].map(([v, l]) => h('option', { value: v }, l)));
    const win = h('select', { class: 'btn', 'aria-label': 'Time window' }, ...[['1h', 'Last hour'], ['6h', 'Last 6 hours'], ['24h', 'Last 24 hours'], ['168h', 'Last 7 days'], ['720h', 'Last 30 days']].map(([v, l]) => h('option', { value: v, selected: v === '24h' }, l)));
    const src = h('select', { class: 'btn', 'aria-label': 'Source filter' }, h('option', { value: '' }, 'Syslog + traps'), h('option', { value: 'syslog' }, 'Syslog'), h('option', { value: 'trap' }, 'Traps'));
    const hist = h('div', { class: 'hist' });
    const list = h('div');
    const count = h('span', { class: 'hint' });
    async function load() {
      const qs = `device=${dev.value}&sev=${sev.value}&q=${encodeURIComponent(q.value)}&from=${win.value}&source=${src.value}&limit=500`;
      const [r, hg] = await Promise.all([get('/api/logs?' + qs), get('/api/logs/histogram?' + qs)]);
      hist.innerHTML = ''; const mx = Math.max(1, ...hg.buckets); hg.buckets.forEach(b => hist.append(h('i', { style: 'height:' + Math.max(2, b / mx * 100) + '%', title: b + ' lines' })));
      list.innerHTML = '';
      count.textContent = r.entries.length + (r.entries.length >= 500 ? '+ lines (narrow the search)' : ' lines');
      r.entries.forEach(e => list.append(h('div', { class: 'logline' }, h('span', { class: 'faint' }, clock(e.recv)), h('span', { title: e.host }, r.names[e.device_id] || e.host), h('span', { class: 'sev s' + e.severity }, sevName(e.severity)), h('span', { class: 'msg' }, e.mnemonic ? h('b', { class: 'muted', style: 'cursor:pointer', onclick: () => { q.value = e.mnemonic; load(); } }, e.mnemonic + ' ') : null, e.message))));
      if (!r.entries.length) list.append(h('div', { class: 'empty' }, h('h3', null, 'No log lines'), 'Point your devices’ syslog at this host — the Snippets tab on any device shows the exact commands. ' + ((state.status.collectors || {}).syslog_addr ? 'Listening on ' + state.status.collectors.syslog_addr + '.' : '')));
    }
    function sevName(s) { return ['emerg', 'alert', 'crit', 'err', 'warn', 'notice', 'info', 'debug'][s] || '?'; }
    [q, dev, sev, win, src].forEach(el => el.addEventListener('change', load));
    q.addEventListener('keydown', e => { if (e.key === 'Enter') load(); });
    main.append(h('div', { class: 'page-head' }, h('h1', null, 'Logs'), q, dev, sev, win, src, h('div', { class: 'spacer' }), count), h('div', { class: 'panel', style: 'padding:10px 14px;margin-bottom:12px' }, hist), h('div', { class: 'panel' }, list));
    await load();
    return {};
  }

  // ---------- flow (NetFlow / IPFIX / sFlow) ----------
  const FLOW_WINDOWS = [['5m', '5 min'], ['15m', '15 min'], ['1h', '1 hour'], ['6h', '6 hours'], ['24h', '24 hours']];
  function flowShare(b, total) { return total ? (b / total * 100).toFixed(b / total >= 0.1 ? 0 : 1) + '%' : '—'; }
  function flowRate(bytes, span) { return fmtBps(span ? bytes * 8 / span : 0); }
  function flowBar(b, max) { return h('span', { class: 'bar' }, h('i', { style: 'width:' + (max ? Math.min(100, b / max * 100) : 0) + '%' })); }
  function protoName(p) { return ({ 1: 'icmp', 2: 'igmp', 6: 'tcp', 17: 'udp', 47: 'gre', 50: 'esp', 51: 'ah', 58: 'icmpv6', 89: 'ospf', 132: 'sctp' })[p] || String(p); }
  // flowTables renders the five top-N tables for one /api/flow response; used by the Flow page and the device tab.
  function flowTables(r, opts) {
    opts = opts || {};
    const sum = r.summary, span = sum.covered || sum.span || 1, names = r.names || {};
    const wrap = h('div');
    const nameOf = ip => names[ip] ? h('span', null, h('b', null, names[ip]), ' ', h('span', { class: 'mono faint small' }, ip)) : h('span', { class: 'mono' }, ip);
    const hostCell = ip => opts.onHost ? h('a', { href: '#', onclick: e => { e.preventDefault(); opts.onHost(ip); } }, nameOf(ip)) : nameOf(ip);
    function table(title, cols, rows, hint) {
      const tb = h('tbody', null, ...rows);
      return h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, title), hint ? h('span', { class: 'hint' }, hint) : null),
        rows.length ? h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, ...cols.map(c => h('th', { class: c[1] || '' }, c[0])))), tb)) : h('div', { class: 'empty' }, 'Nothing in this window'));
    }
    const mx = list => Math.max(1, ...list.map(x => x.b));
    const tk = sum.talkers || [], tg = sum.targets || [], cv = sum.convs || [], ap = sum.apps || [], ifs = r.ifaces || sum.ifaces || [];
    const entryRows = (list, m) => list.map(e => h('tr', null, h('td', null, hostCell(e.k)), h('td', { class: 'num r small' }, fmtBytes(e.b)), h('td', { class: 'num r small' }, flowRate(e.b, span)), h('td', { class: 'num r small muted' }, flowShare(e.b, sum.bytes)), h('td', { style: 'width:120px' }, flowBar(e.b, m))));
    const cols = [['Host'], ['Bytes', 'r'], ['Avg rate', 'r'], ['Share', 'r'], ['']];
    wrap.append(h('div', { class: 'grid c2' },
      table('Top talkers (sources)', cols, entryRows(tk.slice(0, opts.n || 25), mx(tk)), 'by bytes sent'),
      table('Top targets (destinations)', cols, entryRows(tg.slice(0, opts.n || 25), mx(tg)), 'by bytes received')));
    wrap.append(h('div', { class: 'grid c2', style: 'margin-top:12px' },
      table('Applications', [['Application'], ['Proto / port'], ['Bytes', 'r'], ['Packets', 'r'], ['Share', 'r'], ['']], ap.slice(0, opts.n || 25).map(a => h('tr', null, h('td', null, h('b', null, a.n || (protoName(a.pr) + '/' + a.po))), h('td', { class: 'mono small muted' }, protoName(a.pr) + (a.po ? '/' + a.po : '')), h('td', { class: 'num r small' }, fmtBytes(a.b)), h('td', { class: 'num r small' }, a.p.toLocaleString()), h('td', { class: 'num r small muted' }, flowShare(a.b, sum.bytes)), h('td', { style: 'width:120px' }, flowBar(a.b, mx(ap))))), 'by protocol and service port'),
      table('Interfaces of the exporter', [['Interface'], ['In', 'r'], ['Out', 'r'], ['Packets in / out', 'r']], ifs.slice().sort((a, b) => (b.ib + b.ob) - (a.ib + a.ob)).map(i => h('tr', null, h('td', null, i.if_id ? h('a', { href: '#/device/' + r.device + '/interfaces' }, h('span', { class: 'mono' }, i.name)) : h('span', { class: 'mono' }, i.name || 'ifIndex ' + i.i), i.alias ? h('div', { class: 'small muted' }, i.alias) : null), h('td', { class: 'num r small' }, flowRate(i.ib, span), h('div', { class: 'faint' }, fmtBytes(i.ib))), h('td', { class: 'num r small' }, flowRate(i.ob, span), h('div', { class: 'faint' }, fmtBytes(i.ob))), h('td', { class: 'num r small muted' }, (i.ip || 0).toLocaleString() + ' / ' + (i.op || 0).toLocaleString()))), 'as reported in the flow records (ifIndex)')));
    wrap.append(h('div', { style: 'margin-top:12px' },
      table('Conversations', [['Source'], ['Destination'], ['Application'], ['Bytes', 'r'], ['Packets', 'r'], ['Avg rate', 'r'], ['']], cv.slice(0, opts.n ? opts.n * 2 : 50).map(c => h('tr', null, h('td', null, hostCell(c.s)), h('td', null, hostCell(c.d)), h('td', { class: 'small' }, appLabel(c.pr, c.po, ap) ? h('b', null, appLabel(c.pr, c.po, ap)) : null, ' ', h('span', { class: 'mono faint' }, protoName(c.pr) + (c.po ? '/' + c.po : ''))), h('td', { class: 'num r small' }, fmtBytes(c.b)), h('td', { class: 'num r small' }, c.p.toLocaleString()), h('td', { class: 'num r small' }, flowRate(c.b, span)), h('td', { style: 'width:120px' }, flowBar(c.b, mx(cv))))), 'src → dst on the service port')));
    return wrap;
    function appLabel(pr, po, apps) { const a = apps.find(x => x.pr === pr && x.po === po); return a && a.n && a.n !== protoName(pr) + '/' + po ? a.n : ''; }
  }
  function flowEmpty(col) {
    const nf = (col || {}).flow_addr, sf = (col || {}).sflow_addr;
    return h('div', { class: 'empty' }, h('h3', null, 'No flow records yet'),
      h('p', null, 'Point your routers, firewalls and switches at this host: NetFlow v5/v9 or IPFIX to UDP ', h('b', { class: 'mono' }, nf || '2055'), ', sFlow v5 to UDP ', h('b', { class: 'mono' }, sf || '6343'), '. The Snippets tab on any device shows the exact commands. Tables fill within a minute of the first datagram.'));
  }
  async function pageFlow(main, params) {
    let exporter = params[0] || '';           // device id or raw ip
    let win = params[1] || '1h';
    const head = h('div', { class: 'page-head' });
    const body = h('div');
    const chart = h('canvas', { class: 'chart' });
    const kpi = h('div', { class: 'grid c4' });
    let exporters = [], stats = {};
    async function loadExporters() { const r = await get('/api/flow/exporters'); exporters = r.exporters || []; stats = r.stats || {}; }
    function tile(l, v, s) { return h('div', { class: 'tile' }, h('span', { class: 'l' }, l), h('span', { class: 'v' }, v, s ? h('small', null, s) : null)); }
    function renderHead() {
      head.innerHTML = '';
      const sel = h('select', { class: 'btn', 'aria-label': 'Exporter', onchange: () => { exporter = sel.value; location.hash = '#/flow/' + exporter + '/' + win; load(); } },
        h('option', { value: '' }, exporters.length ? 'All exporters' : 'No exporters yet'),
        ...exporters.map(e => h('option', { value: e.device_id || e.exporter, selected: (e.device_id || e.exporter) === exporter }, (e.name ? e.name + ' · ' : '') + e.exporter + ' · ' + fmtBytes(e.bytes_24h) + '/24h')));
      const chips = h('div', { class: 'tabs', style: 'margin:0' }, ...FLOW_WINDOWS.map(([v, l]) => h('button', { class: v === win ? 'on' : '', onclick: () => { win = v; location.hash = '#/flow/' + exporter + '/' + win; load(); } }, l)));
      head.append(h('h1', null, 'Flow'), sel, chips, h('div', { class: 'spacer' }),
        h('span', { class: 'hint' }, `${(stats.datagrams || 0).toLocaleString()} datagrams · ${(stats.records || 0).toLocaleString()} records · ${fmtBytes(stats.disk_bytes || 0)} on disk` + (stats.no_template ? ` · ${stats.no_template} waiting for templates` : '') + (stats.unknown_exporters ? ` · ${stats.unknown_exporters} unknown exporter(s)` : '')));
    }
    async function load() {
      renderHead();
      body.innerHTML = ''; kpi.innerHTML = '';
      const isDev = exporter && !/^\d+\.\d+\.\d+\.\d+$/.test(exporter) && !exporter.includes(':');
      const q = (exporter ? (isDev ? 'device=' : 'exporter=') + encodeURIComponent(exporter) : '') + '&window=' + win;
      let r, ser;
      try { [r, ser] = await Promise.all([get('/api/flow?' + q), get('/api/flow/series?' + q)]); }
      catch (x) { body.append(h('div', { class: 'empty' }, h('h3', null, 'Flow collector disabled'), x.message)); return; }
      const sum = r.summary;
      if (!sum.bytes && !exporters.length) { body.append(flowEmpty((state.status || {}).collectors)); return; }
      kpi.append(tile('Traffic', fmtBytes(sum.bytes), ' in ' + FLOW_WINDOWS.find(w => w[0] === win)[1]), tile('Average rate', flowRate(sum.bytes, sum.covered || sum.span), sum.covered && sum.covered < sum.span ? ' ' + Math.max(1, Math.round(sum.covered / 60)) + ' min of data' : ''), tile('Packets', (sum.packets || 0).toLocaleString()), tile('Flows', (sum.flows || 0).toLocaleString(), sum.overflow ? ' (top-N truncated)' : ''));
      const pts = (ser.points || []).map(p => ({ t: p.t, v: p.b * 8 / (p.s || 60) }));
      const cpanel = h('div', { class: 'panel', style: 'margin:12px 0' }, h('div', { class: 'ph' }, h('h3', null, (r.device_name || exporter || 'All exporters') + ' · throughput seen in flows · ' + FLOW_WINDOWS.find(w => w[0] === win)[1])), h('div', { class: 'pb' }, chart));
      body.append(kpi, cpanel);
      body.append(flowTables(r, { onHost: ip => { const e = exporters.find(x => x.exporter === ip); if (e) { exporter = e.device_id || e.exporter; location.hash = '#/flow/' + exporter + '/' + win; load(); } else { const dn = (r.names || {})[ip]; toast(dn ? dn + ' does not export flows itself — its traffic is shown as seen by ' + (r.device_name || 'the exporter') : ip + ' is not a monitored device', 'ok', 4000); } } }));
      requestAnimationFrame(() => lineChart(chart, [{ pts, color: cssVar('--accent'), label: 'bits/s' }], { fmt: fmtBps }));
    }
    await loadExporters();
    main.append(head, body);
    await load();
    const timer = setInterval(async () => { if (!document.contains(main)) { clearInterval(timer); return; } await loadExporters(); load(); }, 60000);
    return { destroy() { clearInterval(timer); } };
  }

  // ---------- endpoints (MAC / ARP) ----------
  function epRow(e, names, opts) {
    opts = opts || {};
    const where = e.device_id ? h('span', null, h('a', { href: '#/device/' + e.device_id + '/endpoints' }, names[e.device_id] || e.device_id), ' ', h('span', { class: 'mono' }, e.if_name || ('ifIndex ' + e.ifindex)), e.vlan ? h('span', { class: 'faint small' }, ' · VLAN ' + e.vlan) : null)
      : e.arp_device ? h('span', { class: 'muted' }, 'behind ', h('a', { href: '#/device/' + e.arp_device + '/endpoints' }, names[e.arp_device] || e.arp_device), h('span', { class: 'faint small' }, ' (ARP only — no switch has it on an access port)')) : h('span', { class: 'faint' }, '—');
    return h('tr', null,
      h('td', null, h('a', { class: 'mono', href: '#/endpoints/' + encodeURIComponent(e.mac) }, e.mac)),
      h('td', { class: 'mono small' }, (e.ips || []).length ? (e.ips || []).map((ip, i) => h('div', { class: i ? 'faint' : '' }, ip)) : h('span', { class: 'faint' }, '—')),
      h('td', { class: 'small' }, e.vendor || h('span', { class: 'faint' }, 'unknown')),
      opts.noWhere ? null : h('td', null, where),
      h('td', { class: 'small muted' }, e.ports > 1 ? h('span', { title: 'also seen on uplinks of ' + (e.ports - 1) + ' other switch(es)' }, e.ports + ' switches') : e.ports === 1 ? '1 switch' : h('span', { class: 'faint' }, '—')),
      h('td', { class: 'small muted', title: dateTime(e.first_seen) }, 'first ' + ago(e.first_seen)),
      h('td', { class: 'small muted', title: dateTime(e.last_seen) }, ago(e.last_seen), e.moves ? h('span', { class: 'badge minor', style: 'margin-left:6px', title: 'port changes' }, e.moves + '×') : null));
  }
  function epHead(opts) { opts = opts || {}; return h('thead', null, h('tr', null, h('th', null, 'MAC'), h('th', null, 'IP'), h('th', null, 'Vendor'), opts.noWhere ? null : h('th', null, 'Where'), h('th', null, 'Seen by'), h('th', null, 'First'), h('th', null, 'Last'))); }
  async function pageEndpoints(main, params) {
    const q = h('input', { placeholder: 'MAC, IP, vendor or port…', value: params[0] ? decodeURIComponent(params[0]) : '', style: 'max-width:320px' });
    const devices = await get('/api/devices');
    const dev = h('select', { class: 'btn', 'aria-label': 'Device filter' }, h('option', { value: '' }, 'Any device'), ...devices.map(d => h('option', { value: d.id }, d.name)));
    const count = h('span', { class: 'hint' });
    const list = h('div', { class: 'panel tbl-wrap' });
    async function load() {
      const r = await get(`/api/endpoints?q=${encodeURIComponent(q.value.trim())}&device=${dev.value}&limit=500`);
      list.innerHTML = '';
      const st = r.stats || {};
      count.textContent = `${r.endpoints.length}${r.endpoints.length >= 500 ? '+' : ''} shown · ${(st.endpoints || 0).toLocaleString()} known · ${(st.with_ip || 0).toLocaleString()} with IP · ${(st.seen_24h || 0).toLocaleString()} seen today`;
      if (!r.endpoints.length) {
        list.append(h('div', { class: 'empty' }, h('h3', null, st.endpoints ? 'No match' : 'No endpoints yet'), st.endpoints ? 'Try part of a MAC (aabb.cc or aa:bb:cc), an IP prefix, a vendor name or a port name.' : 'Forwarding and ARP tables are walked every 5 minutes after the first poll. Switches need BRIDGE-MIB/Q-BRIDGE-MIB readable with the same credential; Cisco IOS also needs SNMP access to the per-VLAN contexts (community@vlan or the vlan-N v3 context).'));
        return;
      }
      list.append(h('table', { class: 'tbl' }, epHead(), h('tbody', null, ...r.endpoints.map(e => epRow(e, r.names)))));
    }
    q.addEventListener('keydown', e => { if (e.key === 'Enter') load(); });
    q.addEventListener('input', () => { clearTimeout(q._t); q._t = setTimeout(load, 300); });
    dev.addEventListener('change', load);
    main.append(h('div', { class: 'page-head' }, h('h1', null, 'Endpoints'), q, dev, h('div', { class: 'spacer' }), count), list);
    await load();
    return {};
  }

  // ---------- probes (synthetic checks) ----------
  const PROBE_HELP = {
    tcp: ['host:port', 'Connects and closes. Latency = connect time.'],
    http: ['URL (https://…)', 'Expect: status or range ("200-299", "200,404"), optional body:text, optional "insecure" to skip certificate checks. Default 200-399.'],
    dns: ['name to resolve', 'Expect: address prefix the answer must contain. Resolver: server to ask (default: this host’s).'],
    tls: ['host:port (443)', 'Expect: minimum days before expiry (default 14). Reports subject, issuer, expiry and chain validity.'],
    ping: ['host or IP', '5 echo requests; fails on no reply or ≥60% loss.'],
    traceroute: ['host or IP', 'Path every 5 minutes; an event when it changes. Needs CAP_NET_RAW.'],
  };
  function probeDialog(p, done) {
    const type = h('select', null, ...Object.keys(PROBE_HELP).map(t => h('option', { value: t, selected: p && p.type === t }, t)));
    const name = h('input', { value: p ? p.name : '', placeholder: 'optional' }), target = h('input', { value: p ? p.target : '', required: true });
    const expect = h('input', { value: p ? (p.expect || '') : '' }), resolver = h('input', { value: p ? (p.resolver || '') : '', placeholder: 'dns only' });
    const every = h('input', { type: 'number', value: p ? p.every : 60, min: 10, max: 86400 }), timeout = h('input', { type: 'number', value: p ? p.timeout : 5, min: 1, max: 60 });
    const dev = h('select', null, h('option', { value: '' }, '— none —'));
    get('/api/devices').then(ds => ds.forEach(d => dev.append(h('option', { value: d.id, selected: p && p.device_id === d.id }, d.name))));
    const en = h('input', { type: 'checkbox', checked: p ? p.enabled : true });
    const help = h('p', { class: 'small muted' });
    function upd() { const hp = PROBE_HELP[type.value]; target.placeholder = hp[0]; help.textContent = hp[1]; resolver.parentElement.hidden = type.value !== 'dns'; }
    type.addEventListener('change', upd);
    const err = h('div', { class: 'err' });
    const m = modal(p ? 'Edit probe' : 'New probe', h('form', { class: 'form', onsubmit: async e => {
      e.preventDefault();
      const body = { name: name.value.trim(), type: type.value, target: target.value.trim(), expect: expect.value.trim(), resolver: resolver.value.trim(), every: Number(every.value), timeout: Number(timeout.value), device_id: dev.value, enabled: en.checked };
      try { if (p) await put('/api/probes/' + p.id, body); else await post('/api/probes', body); m.close(); done(); } catch (x) { err.textContent = x.message; }
    } }, h('div', { class: 'row' }, h('label', null, 'Type', type), h('label', null, 'Target', target)), help,
      h('div', { class: 'row' }, h('label', null, 'Name', name), h('label', null, 'Expect', expect), h('label', null, 'Resolver', resolver)),
      h('div', { class: 'row' }, h('label', null, 'Every (s)', every), h('label', null, 'Timeout (s)', timeout), h('label', null, 'Related device', dev)),
      h('label', { class: 'check' }, en, ' Enabled'), err,
      h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, p ? 'Save' : 'Add'))));
    upd();
  }
  async function pageProbes(main, params) {
    const sel = params[0] || '';
    const list = h('div', { class: 'panel tbl-wrap' });
    const detail = h('div');
    const count = h('span', { class: 'hint' });
    async function load() {
      const r = await get('/api/probes');
      list.innerHTML = '';
      const sums = r.summaries || {};
      let ok = 0, bad = 0;
      r.probes.forEach(p => { const s = sums[p.id]; if (s && s.last) { if (s.last.ok) ok++; else bad++; } });
      count.textContent = `${r.probes.length} probes · ${ok} ok · ${bad} failing`;
      if (!r.probes.length) { list.append(h('div', { class: 'empty' }, h('h3', null, 'No probes yet'), 'Synthetic checks run from this host: is the service there, how fast, and how long is the certificate good. Add one for your VPN portal, DNS, mail, the ISP gateway…')); return; }
      const tb = h('tbody');
      r.probes.forEach(p => {
        const s = sums[p.id] || {}; const l = s.last;
        const st = !p.enabled ? h('span', { class: 'badge unknown' }, 'off') : !l ? h('span', { class: 'badge unknown' }, 'pending') : l.ok ? h('span', { class: 'badge up' }, 'ok') : h('span', { class: 'badge down' }, 'fail');
        tb.append(h('tr', { class: 'row' + (p.id === sel ? ' on' : ''), onclick: () => nav('#/probes/' + p.id) },
          h('td', null, st), h('td', null, h('b', null, p.name), h('div', { class: 'small mono muted' }, p.type + ' ' + p.target)),
          h('td', { class: 'small' }, l ? (l.detail || (l.ok ? 'ok' : 'failed')) : '—', l && l.attrs && l.attrs.cert_days ? h('span', { class: 'faint' }, ' · cert ' + l.attrs.cert_days + ' d') : null),
          h('td', { class: 'num r small' }, l && l.ok ? l.ms.toFixed(1) + ' ms' : '—'), h('td', { class: 'num r small' }, s.avg_ms ? s.avg_ms.toFixed(1) + ' ms' : '—'), h('td', { class: 'num r small' }, s.uptime_pct !== undefined && l ? s.uptime_pct.toFixed(1) + '%' : '—'),
          h('td', { class: 'small muted' }, p.device_id ? h('a', { href: '#/device/' + p.device_id, onclick: e => e.stopPropagation() }, r.names[p.device_id] || '') : ''), h('td', { class: 'small muted' }, 'every ' + p.every + ' s')));
      });
      list.append(h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, ''), h('th', null, 'Probe'), h('th', null, 'Last result'), h('th', { class: 'r' }, 'Last'), h('th', { class: 'r' }, 'Avg'), h('th', { class: 'r' }, 'OK rate'), h('th', null, 'Device'), h('th', null, ''))), tb));
    }
    async function loadDetail() {
      detail.innerHTML = '';
      if (!sel) return;
      let r;
      try { r = await get('/api/probes/' + sel); } catch (x) { return; }
      const p = r.probe;
      const chart = h('canvas', { class: 'chart' });
      const hist = (r.history || []);
      const runs = h('div', { class: 'list', style: 'padding:6px;max-height:320px;overflow:auto' }, ...hist.slice().reverse().slice(0, 40).map(x => h('div', { class: 'item ' + (x.ok ? 'info' : 'major') }, h('span', { class: 'bar' }), h('div', null, h('div', { class: 't' }, h('span', null, dateTime(x.ts)), h('span', null, '· ' + (x.ok ? x.ms.toFixed(1) + ' ms' : 'failed'))), h('div', { class: 'd', style: 'color:var(--text)' }, x.detail || '', x.attrs ? h('span', { class: 'faint' }, ' ' + Object.entries(x.attrs).filter(([k]) => k !== 'ip').map(([k, v]) => k + '=' + v).join(' · ')) : null)))));
      const path = (r.path || []).length ? h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Path')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Hop'), h('th', null, 'Address'), h('th', { class: 'r' }, 'RTT'))), h('tbody', null, ...r.path.map(hp => h('tr', null, h('td', { class: 'num' }, hp.ttl), h('td', { class: 'mono' }, hp.addr || h('span', { class: 'faint' }, '* no answer')), h('td', { class: 'num r' }, hp.addr ? hp.ms.toFixed(1) + ' ms' : '')))))) : null;
      detail.append(h('div', { class: 'panel', style: 'margin:12px 0' }, h('div', { class: 'ph' }, h('h3', null, p.name), h('span', { class: 'hint mono' }, p.type + ' ' + p.target + (p.expect ? ' · expect ' + p.expect : '')), h('div', { class: 'spacer' }),
        h('button', { class: 'btn sm', onclick: async () => { const x = await post('/api/probes/' + p.id + '/run'); toast(x.ok ? 'OK in ' + x.ms.toFixed(1) + ' ms' : 'Failed: ' + x.detail, x.ok ? 'ok' : 'err'); load(); loadDetail(); } }, 'Run now'),
        h('button', { class: 'btn sm', onclick: () => probeDialog(p, () => { load(); loadDetail(); }) }, 'Edit'),
        state.user.role !== 'viewer' ? h('button', { class: 'btn sm danger', onclick: async () => { if (confirm('Delete probe ' + p.name + '?')) { await del('/api/probes/' + p.id); nav('#/probes'); } } }, 'Delete') : null),
        h('div', { class: 'pb' }, chart)),
        h('div', { class: 'grid c2' }, h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Recent runs')), runs), path));
      try {
        const m = await get(`/api/metrics?from=24h&series=${encodeURIComponent('probe_ms|' + p.id)}`);
        requestAnimationFrame(() => lineChart(chart, [{ pts: m.series['probe_ms|' + p.id], color: cssVar('--accent'), label: 'ms' }], { fmt: v => (v < 10 ? v.toFixed(1) : v.toFixed(0)) + ' ms' }));
      } catch (e) { }
    }
    main.append(h('div', { class: 'page-head' }, h('h1', null, 'Probes'), h('span', { class: 'sub' }, 'Synthetic checks from this host'), h('div', { class: 'spacer' }), count, state.user.role !== 'viewer' ? h('button', { class: 'btn primary', onclick: () => probeDialog(null, load) }, '+ Probe') : null), list, detail);
    await load(); await loadDetail();
    const timer = setInterval(() => { if (!document.contains(main)) { clearInterval(timer); return; } load(); }, 30000);
    return { destroy() { clearInterval(timer); } };
  }

  // ---------- reports ----------
  const SECTION_LABELS = { availability: 'Availability / SLA', alerts: 'Alerts & MTTR', utilisation: 'Utilisation & load', inventory: 'Inventory', changes: 'Configuration changes', flow: 'Flow top talkers', endpoints: 'Endpoints', probes: 'Probes' };
  function reportDialog(rp, sections, smtp, done) {
    const name = h('input', { value: rp ? rp.name : 'Weekly network report', required: true });
    const period = h('select', null, ...[['24h', 'Last 24 hours'], ['7d', 'Last 7 days'], ['30d', 'Last 30 days']].map(([v, l]) => h('option', { value: v, selected: rp ? rp.period === v : v === '7d' }, l)));
    const site = h('select', null, h('option', { value: '' }, 'All sites'), ...state.sites.map(s => h('option', { value: s.id, selected: rp && rp.site_id === s.id }, s.name)));
    const boxes = sections.map(s => { const cb = h('input', { type: 'checkbox', value: s, checked: rp ? rp.sections.includes(s) : true }); return h('label', { class: 'check' }, cb, ' ' + (SECTION_LABELS[s] || s)); });
    const sched = h('select', null, ...[['', 'On demand only'], ['daily', 'Daily'], ['weekly', 'Weekly (Monday)'], ['monthly', 'Monthly (1st)']].map(([v, l]) => h('option', { value: v, selected: rp && rp.schedule === v }, l)));
    const hour = h('input', { type: 'number', min: 0, max: 23, value: rp ? rp.hour : 7 });
    const mail = h('textarea', { rows: 2, placeholder: 'ops@example.com, manager@example.com' }, rp ? (rp.email_to || []).join(', ') : '');
    const en = h('input', { type: 'checkbox', checked: rp ? rp.enabled : true });
    const err = h('div', { class: 'err' });
    const m = modal(rp ? 'Edit report' : 'New report', h('form', { class: 'form', onsubmit: async e => {
      e.preventDefault();
      const body = { name: name.value.trim(), period: period.value, site_id: site.value, sections: boxes.map(b => b.firstChild).filter(cb => cb.checked).map(cb => cb.value), schedule: sched.value, hour: Number(hour.value), email_to: mail.value.split(/[,\n]/).map(x => x.trim()).filter(Boolean), enabled: en.checked };
      try { if (rp) await put('/api/reports/' + rp.id, body); else await post('/api/reports', body); m.close(); done(); } catch (x) { err.textContent = x.message; }
    } }, h('div', { class: 'row' }, h('label', null, 'Name', name), h('label', null, 'Period', period), h('label', null, 'Site', site)),
      h('div', null, h('span', { class: 'small muted' }, 'Sections'), h('div', { style: 'display:grid;grid-template-columns:1fr 1fr;gap:2px 12px;margin-top:4px' }, ...boxes)),
      h('div', { class: 'row' }, h('label', null, 'Schedule', sched), h('label', null, 'At hour (local)', hour)),
      h('label', null, 'E-mail to' + (smtp ? '' : ' — SMTP not configured, scheduled reports are stored only'), mail), h('label', { class: 'check' }, en, ' Enabled'), err,
      h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, rp ? 'Save' : 'Create'))));
  }
  async function pageWireless(main, params) {
    let tab = params[0] || 'aps';
    const tabs = h('div', { class: 'tabs' });
    const body = h('div');
    const count = h('span', { class: 'hint' });
    function renderTabs() { tabs.innerHTML = ''; [['aps', 'Access points'], ['sdwan', 'SD-WAN / WAN paths']].forEach(([k, l]) => tabs.append(h('button', { class: k === tab ? 'on' : '', onclick: () => { tab = k; location.hash = '#/wireless/' + k; render(); } }, l))); }
    async function render() {
      renderTabs(); body.innerHTML = '';
      if (tab === 'aps') {
        const r = await get('/api/wireless');
        const aps = r.aps || [];
        count.textContent = aps.length + ' access points · ' + r.clients + ' clients';
        if (!aps.length) { body.append(h('div', { class: 'empty' }, h('h3', null, 'No access points yet'), 'Add a UniFi or Meraki integration under Admin → Integrations, or monitor a Cisco WLC / Aruba controller over SNMP: its access points appear here with client counts, channels and utilisation.')); return; }
        const tb = h('tbody');
        aps.forEach(a => tb.append(h('tr', { class: 'row', onclick: () => nav('#/device/' + a.device_id + '/wireless') },
          h('td', null, h('span', { class: 'sdot ' + a.status }), h('b', null, a.name)), h('td', { class: 'small muted' }, (a.vendor || '') + ' ' + (a.model || '')), h('td', { class: 'small' }, (state.sites.find(s => s.id === a.site_id) || {}).name || ''),
          h('td', { class: 'num r' }, a.clients), h('td', { class: 'mono small' }, (a.radios || []).map(x => x.name + ':' + x.channel + (x.util_pct ? ' (' + Math.round(x.util_pct) + '%)' : '')).join(' · ') || (a.aps ? a.aps_up + '/' + a.aps + ' APs' : '—')),
          h('td', { class: 'small' }, (a.version || '—') + (a.upgradable ? ' ↑' : '')), h('td', { class: 'num r' }, a.satisfaction ? a.satisfaction + ' %' : '—'), h('td', { class: 'small muted' }, ago(a.ts)))));
        body.append(h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Access point'), h('th', null, 'Model'), h('th', null, 'Site'), h('th', { class: 'r' }, 'Clients'), h('th', null, 'Radios / channels'), h('th', null, 'Firmware'), h('th', { class: 'r' }, 'Satisfaction'), h('th', null, 'Reported'))), tb)));
      } else {
        const r = await get('/api/sdwan');
        const links = r.links || [];
        count.textContent = links.length + ' WAN paths · ' + r.down + ' down';
        if (!links.length) { body.append(h('div', { class: 'empty' }, h('h3', null, 'No WAN paths yet'), 'Meraki MX uplinks come from the Meraki integration; FortiGate SD-WAN health checks are read over SNMP (FORTINET-FORTIGATE-MIB) from any monitored FortiGate.')); return; }
        const tb = h('tbody');
        links.forEach(l => tb.append(h('tr', { class: 'row', onclick: () => nav('#/device/' + l.device_id + '/wireless') },
          h('td', null, h('b', null, l.device)), h('td', null, l.name), h('td', { class: 'mono small' }, l.interface + (l.ip ? ' · ' + l.ip : '')), h('td', null, h('span', { class: 'badge ' + (l.up ? 'up' : 'down') }, l.state || (l.up ? 'up' : 'down'))),
          h('td', { class: 'num r' }, l.latency_ms ? l.latency_ms.toFixed(1) + ' ms' : '—'), h('td', { class: 'num r' }, l.jitter_ms ? l.jitter_ms.toFixed(1) + ' ms' : '—'), h('td', { class: 'num r' }, l.loss_pct !== undefined ? l.loss_pct.toFixed(1) + ' %' : '—'), h('td', { class: 'small muted' }, ago(l.ts)))));
        body.append(h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Device'), h('th', null, 'Path'), h('th', null, 'Interface'), h('th', null, 'State'), h('th', { class: 'r' }, 'Latency'), h('th', { class: 'r' }, 'Jitter'), h('th', { class: 'r' }, 'Loss'), h('th', null, 'Checked'))), tb)));
      }
    }
    main.append(h('div', { class: 'page-head' }, h('h1', null, 'Wireless & WAN'), h('span', { class: 'sub' }, 'Access points and SD-WAN paths from controllers, cloud APIs and SNMP'), h('div', { class: 'spacer' }), count), tabs, body);
    render();
  }

  async function pageReports(main) {
    const list = h('div', { class: 'panel tbl-wrap' });
    const files = h('div', { class: 'panel tbl-wrap', style: 'margin-top:12px' });
    let sections = [], smtp = false;
    async function load() {
      const r = await get('/api/reports'); sections = r.sections; smtp = r.smtp;
      list.innerHTML = ''; files.innerHTML = '';
      if (!r.reports.length) list.append(h('div', { class: 'empty' }, h('h3', null, 'No saved reports'), 'Create one for the weekly ops meeting or the monthly SLA mail — or use Quick report above for a one-off.'));
      else {
        const tb = h('tbody');
        r.reports.forEach(rp => tb.append(h('tr', null, h('td', null, h('b', null, rp.name), h('div', { class: 'small muted' }, (rp.sections || []).map(s => SECTION_LABELS[s] || s).join(' · '))), h('td', { class: 'small' }, rp.period + (rp.site_id ? ' · ' + ((state.sites.find(s => s.id === rp.site_id) || {}).name || '') : '')),
          h('td', { class: 'small' }, rp.schedule ? cap(rp.schedule) + ' at ' + String(rp.hour).padStart(2, '0') + ':00' + ((rp.email_to || []).length ? ' → ' + rp.email_to.join(', ') : '') : 'on demand', rp.enabled ? null : h('span', { class: 'badge unknown', style: 'margin-left:6px' }, 'off')),
          h('td', { class: 'small muted' }, isZero(rp.last_run) ? 'never' : ago(rp.last_run), rp.last_error ? h('div', { style: 'color:var(--major)' }, rp.last_error) : null),
          h('td', null, h('a', { class: 'btn sm', href: '/api/reports/preview?name=' + encodeURIComponent(rp.name) + '&period=' + rp.period + '&site=' + (rp.site_id || '') + '&sections=' + (rp.sections || []).join(','), target: '_blank' }, 'Open'), ' ',
            h('button', { class: 'btn sm', onclick: async e => { e.target.disabled = true; const x = await post('/api/reports/' + rp.id + '/run' + ((rp.email_to || []).length && smtp ? '?mail=1' : '')); toast(x.ok ? 'Generated' + ((rp.email_to || []).length && smtp ? ' and mailed' : '') : 'Failed: ' + x.error, x.ok ? 'ok' : 'err'); load(); } }, (rp.email_to || []).length && smtp ? 'Run & mail' : 'Run & store'), ' ',
            h('button', { class: 'btn sm', onclick: () => reportDialog(rp, sections, smtp, load) }, 'Edit'), ' ', h('button', { class: 'btn sm danger', onclick: async () => { if (confirm('Delete report ' + rp.name + '?')) { await del('/api/reports/' + rp.id); load(); } } }, 'Delete')))));
        list.append(h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Report'), h('th', null, 'Period'), h('th', null, 'Schedule'), h('th', null, 'Last run'), h('th', null, ''))), tb));
      }
      const fl = r.files || [];
      files.append(h('div', { class: 'ph' }, h('h3', null, 'Generated reports'), h('span', { class: 'hint' }, 'last 12 per definition are kept on disk')));
      if (!fl.length) files.append(h('div', { class: 'empty' }, 'Nothing generated yet'));
      else files.append(h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Generated'), h('th', null, 'Report'), h('th', { class: 'r' }, 'Size'), h('th', null, ''))), h('tbody', null, ...fl.map(f => { const rp = r.reports.find(x => x.id === f.report_id); return h('tr', null, h('td', null, dateTime(f.ts)), h('td', null, rp ? rp.name : f.report_id), h('td', { class: 'num r small' }, fmtBytes(f.bytes)), h('td', null, h('a', { class: 'btn sm', href: '/api/reports/files/' + f.file, target: '_blank' }, 'Open'))); }))));
    }
    // quick report
    const qp = h('select', { class: 'btn', 'aria-label': 'Quick report period' }, ...[['24h', 'Last 24 h'], ['7d', 'Last 7 days'], ['30d', 'Last 30 days']].map(([v, l]) => h('option', { value: v, selected: v === '7d' }, l)));
    const qs = h('select', { class: 'btn', 'aria-label': 'Quick report site' }, h('option', { value: '' }, 'All sites'), ...state.sites.map(s => h('option', { value: s.id }, s.name)));
    const open = fmt => { window.open('/api/reports/preview?period=' + qp.value + '&site=' + qs.value + '&format=' + fmt, '_blank'); };
    main.append(h('div', { class: 'page-head' }, h('h1', null, 'Reports'), h('span', { class: 'sub' }, 'Availability, alerts, utilisation, inventory, changes, flow, endpoints, probes — HTML (print to PDF) or CSV'), h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => reportDialog(null, sections, smtp, load) }, '+ Report')),
      h('div', { class: 'panel', style: 'padding:10px 14px;margin-bottom:12px;display:flex;gap:8px;align-items:center;flex-wrap:wrap' }, h('b', null, 'Quick report'), qp, qs, h('button', { class: 'btn sm', onclick: () => open('html') }, 'Open HTML'), h('button', { class: 'btn sm', onclick: () => open('csv') }, 'Download CSV'), h('span', { class: 'hint' }, 'all sections · opens in a new tab; use the browser’s Print → Save as PDF for a PDF')),
      list, files);
    await load();
    return {};
  }

  // ---------- admin ----------
  async function pageAdmin(main, params) {
    let tab = params[0] || 'sites';
    const tabs = h('div', { class: 'tabs' });
    const body = h('div');
    const TABS = [['sites', 'Sites'], ['creds', 'Credentials'], ['notify', 'Notifications'], ['rules', 'Alert rules'], ['maintenance', 'Maintenance'], ['users', 'Users'], ['integrations', 'Integrations'], ['cluster', 'Cluster'], ['license', 'Licence'], ['settings', 'Settings'], ['about', 'System']];
    function renderTabs() { tabs.innerHTML = ''; TABS.forEach(([k, l]) => tabs.append(h('button', { class: k === tab ? 'on' : '', onclick: () => { tab = k; location.hash = '#/admin/' + k; render(); } }, l))); }
    async function render() {
      renderTabs(); body.innerHTML = '';
      const isAdmin = state.user.role === 'admin';
      if (tab === 'sites') {
        const sites = await get('/api/sites'); state.sites = sites;
        const creds = await get('/api/creds');
        const tb = h('tbody');
        sites.forEach(s => tb.append(h('tr', null, h('td', null, h('b', null, s.name)), h('td', { class: 'mono small' }, s.subnets.join(', ')), h('td', { class: 'small' }, (creds.find(c => c.id === s.cred_id) || {}).name || 'any'), h('td', null, h('button', { class: 'btn sm', onclick: () => discoverDialog(s) }, 'Discover'), ' ', h('button', { class: 'btn sm', onclick: () => siteDialog(s, render) }, 'Edit'), ' ', isAdmin ? h('button', { class: 'btn sm danger', onclick: async () => { try { await del('/api/sites/' + s.id); render(); } catch (x) { toast(x.message, 'err'); } } }, 'Delete') : null))));
        body.append(h('div', { class: 'page-head' }, h('span', { class: 'sub' }, 'A site groups devices and holds the ranges discovery sweeps. Discovery runs every ' + (state.status.settings.discovery_every || 0) + ' min; LLDP neighbours are followed automatically.'), h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => siteDialog(null, render) }, '+ Site')),
          h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Site'), h('th', null, 'Ranges'), h('th', null, 'Credential'), h('th', null, ''))), tb)));
      } else if (tab === 'creds') {
        const creds = await get('/api/creds');
        const tb = h('tbody');
        creds.forEach(c => tb.append(h('tr', null, h('td', null, h('b', null, c.name)), h('td', null, c.kind === 'ssh' ? 'SSH' : c.kind === 'gnmi' ? 'gNMI' : 'SNMP v' + c.version), h('td', { class: 'small muted' }, c.kind === 'gnmi' ? `${c.user} · port ${c.port || 6030} · ${c.plaintext ? 'plaintext' : 'TLS'}` : c.kind === 'ssh' ? `${c.user} · ${c.private_key ? 'key' : 'password'}${c.enable_pass ? ' + enable' : ''} · port ${c.port || 22}` : (c.version === '3' ? `${c.user} · ${c.auth_proto || 'noauth'}/${c.priv_proto || 'nopriv'}` : 'community ••••') + (c.port && c.port !== 161 ? ` · udp ${c.port}` : '')), h('td', null, c.kind === 'ssh' ? null : h('button', { class: 'btn sm', onclick: () => testCred(c) }, 'Test'), ' ', h('button', { class: 'btn sm', onclick: () => credDialog(c, render) }, 'Edit'), ' ', isAdmin ? h('button', { class: 'btn sm danger', onclick: async () => { await del('/api/creds/' + c.id); render(); } }, 'Delete') : null))));
        body.append(h('div', { class: 'page-head' }, h('span', { class: 'sub' }, 'Read-only SNMP credentials (discovery tries the site’s credential first, then the others), SSH for configuration backup, gNMI for OpenConfig devices.'), h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => credDialog(null, render) }, '+ Credential')),
          h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Name'), h('th', null, 'Version'), h('th', null, 'Details'), h('th', null, ''))), tb)));
      } else if (tab === 'notify') {
        const n = await get('/api/notify'); const c = n.config;
        const caps = state.status.license.caps;
        const email = h('textarea', { rows: 2, placeholder: 'noc@example.com, oncall@example.com' }, (c.email_to || []).join(', '));
        const tg = h('input', { value: c.telegram_chat || '', placeholder: 'chat id, e.g. -1001234567890', disabled: !caps.telegram });
        const wh = h('input', { value: c.webhook_url || '', placeholder: 'https://…', disabled: !caps.webhook });
        const min = h('select', null, ...['info', 'minor', 'major', 'critical'].map(s => h('option', { value: s, selected: s === c.min_severity }, cap(s))));
        const grp = h('input', { type: 'number', value: c.group_seconds || 60, min: 0, max: 900 });
        const res = h('input', { type: 'checkbox', checked: c.resolved_too }), qf = h('input', { value: c.quiet_from || '', placeholder: '22:00' }), qt = h('input', { value: c.quiet_to || '', placeholder: '07:00' }), ca = h('input', { type: 'checkbox', checked: c.critical_always });
        const out = h('div', { class: 'small' });
        body.append(h('form', { class: 'form panel pb', style: 'max-width:760px', onsubmit: async e => {
          e.preventDefault();
          try { await put('/api/notify', { email_to: email.value.split(/[,\n]/).map(s => s.trim()).filter(Boolean), telegram_chat: tg.value.trim(), webhook_url: wh.value.trim(), min_severity: min.value, group_seconds: Number(grp.value), resolved_too: res.checked, quiet_from: qf.value.trim(), quiet_to: qt.value.trim(), critical_always: ca.checked }); toast('Saved', 'ok'); }
          catch (x) { toast(x.message, 'err'); }
        } },
          h('label', null, 'E-mail recipients ' + (n.smtp_configured ? '' : '— SMTP not configured: start topolight with -smtp-host …'), email),
          h('label', null, 'Telegram chat id ' + (caps.telegram ? (n.telegram_configured ? '' : '— bot token not configured: -telegram-token or TOPOLIGHT_TELEGRAM_TOKEN') : '(Pro/Team)'), tg),
          h('label', null, 'Webhook URL ' + (caps.webhook ? (n.webhook_signed ? '(signed with X-TopoLight-Signature)' : '(unsigned — set -webhook-secret to sign)') : '(Pro/Team)'), wh),
          h('div', { class: 'row' }, h('label', null, 'Minimum severity', min), h('label', null, 'Group for (seconds)', grp), h('label', null, 'Quiet from', qf), h('label', null, 'Quiet to', qt)),
          h('label', { class: 'check' }, res, ' Also send resolutions'), h('label', { class: 'check' }, ca, ' Critical alerts ignore quiet hours'),
          h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: async () => { const r = await post('/api/notify/test'); out.innerHTML = r.results.map(esc).join('<br>'); } }, 'Send test'), h('button', { class: 'btn primary', type: 'submit' }, 'Save')), out));
      } else if (tab === 'rules') {
        const rules = await get('/api/rules');
        const tb = h('tbody');
        rules.forEach(r => {
          const en = h('input', { type: 'checkbox', checked: r.enabled, disabled: !isAdmin, 'aria-label': 'Enabled' }), enter = h('input', { type: 'number', 'aria-label': 'Enter threshold', value: r.enter || '', step: 'any', style: 'width:80px', disabled: !isAdmin }), exit = h('input', { type: 'number', 'aria-label': 'Exit threshold', value: r.exit || '', step: 'any', style: 'width:80px', disabled: !isAdmin }), cyc = h('input', { type: 'number', 'aria-label': 'Cycles', value: r.for_cycles || 0, style: 'width:70px', disabled: !isAdmin });
          const sev = h('select', { disabled: !isAdmin, 'aria-label': 'Severity' }, ...['info', 'minor', 'major', 'critical'].map(s => h('option', { value: s, selected: s === r.severity }, s)));
          const imp = h('input', { type: 'checkbox', checked: r.only_important, disabled: !isAdmin, 'aria-label': 'Important interfaces only' });
          const save = async () => { try { await put('/api/rules/' + r.id, { enter: Number(enter.value), exit: Number(exit.value), for_cycles: Number(cyc.value), severity: sev.value, escalate: r.escalate, only_important: imp.checked, enabled: en.checked }); toast('Rule saved', 'ok'); } catch (x) { toast(x.message, 'err'); } };
          [en, enter, exit, cyc, sev, imp].forEach(el => el.addEventListener('change', save));
          tb.append(h('tr', null, h('td', null, en), h('td', null, h('b', { class: 'mono' }, r.id), h('div', { class: 'small muted' }, r.description)), h('td', null, r.object), h('td', null, r.metric ? enter : h('span', { class: 'faint' }, '—')), h('td', null, r.metric ? exit : h('span', { class: 'faint' }, '—')), h('td', null, cyc), h('td', null, sev), h('td', null, r.object === 'interface' ? imp : '')));
        });
        body.append(h('div', { class: 'page-head' }, h('span', { class: 'sub' }, 'Thresholds enter/leave with hysteresis; "cycles" = consecutive polls (or minutes to auto-resolve for event rules). Changes apply immediately.')),
          h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'On'), h('th', null, 'Rule'), h('th', null, 'Object'), h('th', null, 'Enter'), h('th', null, 'Exit'), h('th', null, 'Cycles'), h('th', null, 'Severity'), h('th', null, 'Important only'))), tb)));
      } else if (tab === 'maintenance') {
        const ms = await get('/api/maintenance');
        const tb = h('tbody');
        ms.forEach(mw => tb.append(h('tr', null, h('td', null, h('b', null, mw.name), h('div', { class: 'small muted' }, mw.reason)), h('td', { class: 'small' }, dateTime(mw.from) + ' → ' + dateTime(mw.to)), h('td', { class: 'small' }, mw.devices && mw.devices.length ? mw.devices.length + ' device(s)' : (state.sites.find(s => s.id === mw.site_id) || {}).name || 'all'), h('td', null, h('button', { class: 'btn sm danger', onclick: async () => { await del('/api/maintenance/' + mw.id); render(); } }, 'Delete')))));
        body.append(h('div', { class: 'page-head' }, h('span', { class: 'sub' }, 'Objects in a window are shown in purple and never notify. Create ad-hoc windows from any alert or device.'), h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => maintenanceDialogSite(render) }, '+ Window')),
          h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Window'), h('th', null, 'When'), h('th', null, 'Scope'), h('th', null, ''))), tb)), ms.length ? null : h('div', { class: 'empty' }, 'No maintenance windows.'));
      } else if (tab === 'users') {
        const users = await get('/api/users');
        const tb = h('tbody');
        users.forEach(u => tb.append(h('tr', null, h('td', null, h('b', null, u.name)), h('td', null, u.role), h('td', { class: 'small muted' }, dateTime(u.created)), h('td', null, h('button', { class: 'btn sm', onclick: () => passwordDialog(u) }, 'Password'), ' ', u.id !== state.user.id ? h('button', { class: 'btn sm danger', onclick: async () => { await del('/api/users/' + u.id); render(); } }, 'Delete') : null))));
        const caps = state.status.license.caps;
        body.append(h('div', { class: 'page-head' }, h('span', { class: 'sub' }, caps.roles ? 'Roles: admin (everything), operator (ack, maintenance, discovery), viewer (read-only).' : 'This tier allows ' + (caps.max_users || 'unlimited') + ' user(s); operator/viewer roles are a Team feature.'), h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => userDialog(render) }, '+ User')),
          h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'User'), h('th', null, 'Role'), h('th', null, 'Created'), h('th', null, ''))), tb)));
        // API tokens
        const toks = await get('/api/tokens');
        const ttb = h('tbody');
        toks.forEach(t => ttb.append(h('tr', null, h('td', null, h('b', null, t.Name), h('div', { class: 'small mono faint' }, t.Prefix + '…')), h('td', null, t.Role), h('td', { class: 'small muted' }, dateTime(t.Created) + (t.Creator ? ' by ' + t.Creator : '')), h('td', { class: 'small muted' }, isZero(t.LastUsed) ? 'never' : ago(t.LastUsed)), h('td', null, h('button', { class: 'btn sm danger', onclick: async () => { if (confirm('Revoke token ' + t.Name + '? Scripts using it stop working immediately.')) { await del('/api/tokens/' + t.ID); render(); } } }, 'Revoke')))));
        body.append(h('div', { class: 'page-head', style: 'margin-top:20px' }, h('h3', null, 'API tokens'), h('span', { class: 'sub' }, 'For scripts and integrations: send Authorization: Bearer tl_… — same endpoints as the console, no cookie, no same-origin check. A token never outranks its creator.'), h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => tokenDialog(render) }, '+ Token')),
          h('div', { class: 'panel tbl-wrap' }, toks.length ? h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Token'), h('th', null, 'Role'), h('th', null, 'Created'), h('th', null, 'Last used'), h('th', null, ''))), ttb) : h('div', { class: 'empty' }, 'No tokens yet. Example use: curl -H "Authorization: Bearer tl_…" ' + location.origin + '/api/devices')));
      } else if (tab === 'integrations') {
        if (!isAdmin) { body.append(h('div', { class: 'empty' }, 'Integrations are managed by administrators.')); return; }
        const r = await get('/api/integrations');
        const tb = h('tbody');
        (r.integrations || []).forEach(i => tb.append(h('tr', null, h('td', null, h('b', null, i.name)), h('td', null, { unifi: 'UniFi Network', meraki: 'Cisco Meraki' }[i.kind] || i.kind), h('td', { class: 'mono small' }, i.kind === 'meraki' ? (i.site ? 'org ' + i.site : 'all organisations') : i.url + ' · site ' + (i.site || 'default')),
          h('td', null, !i.enabled ? h('span', { class: 'badge unknown' }, 'off') : i.last_error ? h('span', { class: 'badge down', title: i.last_error }, 'failing') : i.last_run ? h('span', { class: 'badge up' }, 'ok') : h('span', { class: 'badge unknown' }, 'pending')),
          h('td', { class: 'num r' }, i.devices || 0), h('td', { class: 'num r' }, i.clients || 0), h('td', { class: 'small muted' }, i.last_run ? ago(i.last_run) : '—'),
          h('td', null, h('button', { class: 'btn sm', onclick: async e => { e.target.disabled = true; const x = await post('/api/integrations/' + i.id + '/test'); e.target.disabled = false; toast(x.ok ? '✓ ' + x.message : '✗ ' + x.error, x.ok ? 'ok' : 'err', 7000); } }, 'Test'), ' ', h('button', { class: 'btn sm', onclick: () => integDialog(i, render) }, 'Edit'), ' ', h('button', { class: 'btn sm danger', onclick: async () => { if (confirm('Remove integration ' + i.name + '? Devices it imported stay until you delete them.')) { await del('/api/integrations/' + i.id); render(); } } }, 'Delete')))));
        body.append(h('div', { class: 'page-head' }, h('span', { class: 'sub' }, 'Controllers and cloud APIs TopoLight reads: access points, clients per radio and SSID, firmware, and Meraki MX WAN uplinks. Imported devices count towards the device limit like any other.'), h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => integDialog(null, render) }, '+ Integration')),
          h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Name'), h('th', null, 'Type'), h('th', null, 'Target'), h('th', null, 'Status'), h('th', { class: 'r' }, 'Devices'), h('th', { class: 'r' }, 'Clients'), h('th', null, 'Last run'), h('th', null, ''))), tb)),
          h('p', { class: 'small faint', style: 'margin-top:8px' }, 'UniFi: a local read-only user on the controller (UniFi OS consoles and the classic controller both work). Meraki: an API key from Organization → Settings → Dashboard API access (read-only is enough). Cisco WLC (AireOS/9800) and Aruba controllers need no integration — add them as SNMP devices and their APs are discovered from the controller MIB.'));
      } else if (tab === 'cluster') {
        const c = await get('/api/cluster');
        if (!c.available) { body.append(h('div', { class: 'empty' }, h('h3', null, 'Clustering unavailable'), c.reason)); return; }
        if (!c.enabled) {
          body.append(h('div', { class: 'panel pb', style: 'max-width:760px' }, h('h3', null, 'Run TopoLight on more than one server'),
            h('p', null, 'A cluster gives you two things: ', h('b', null, 'failover'), ' (with three or more full nodes a new leader takes over automatically within about 20 seconds; every full node keeps a complete copy of the data) and ', h('b', null, 'throughput'), ' (each node polls a share of the devices, and remote sites can run a lightweight collector that polls locally and forwards). One binary, one command to join, no external components.'),
            h('p', { class: 'small muted' }, 'Enabling creates the cluster certificate authority on this server and opens the node-to-node port (' + (c.addr || 'https://<this host>:8434') + ', mutual TLS). Nothing else changes until another node joins. Firewalls: nodes talk to each other on that port only.'),
            h('div', { class: 'actions' }, isAdmin ? h('button', { class: 'btn primary', onclick: async () => { try { await post('/api/cluster/enable'); toast('Cluster enabled — this node is the leader', 'ok'); render(); } catch (x) { toast(x.message, 'err'); } } }, 'Enable clustering on this server') : null)));
          return;
        }
        const st = c.status || {};
        const ms = st.member_status || {};
        const members = (c.members || []).slice().sort((a, b) => (a.id === st.leader_id ? -1 : b.id === st.leader_id ? 1 : a.name.localeCompare(b.name)));
        const full = members.filter(m => m.role === 'full').length;
        const head = h('div', { class: 'page-head' }, h('span', { class: 'sub' }, `This node: ${esc(c.name)} · ${st.state || '?'} · term ${st.term || 0} · ${full} full node${full === 1 ? '' : 's'} (${full >= 3 ? 'automatic failover' : full === 2 ? 'manual failover only — a third full node gives automatic failover' : 'no failover yet — add nodes'}) · CA ${(c.ca_fp || '').slice(0, 12)}…`), h('div', { class: 'spacer' }),
          isAdmin ? h('button', { class: 'btn primary', onclick: () => tokenDialog('full') }, '+ Join token (full node)') : null, ' ', isAdmin ? h('button', { class: 'btn', onclick: () => tokenDialog('collector') }, '+ Join token (collector)') : null);
        const tb = h('tbody');
        members.forEach(m => {
          const s = ms[m.id] || {};
          const isLeader = m.id === st.leader_id, self = m.id === c.node_id;
          const alive = self ? true : !!s.alive;
          tb.append(h('tr', null,
            h('td', null, h('span', { class: 'sdot ' + (alive ? 'up' : 'down') }), ' ', h('b', null, m.name), self ? h('span', { class: 'faint small' }, ' (this node)') : null, h('div', { class: 'small mono muted' }, m.id)),
            h('td', null, isLeader ? h('span', { class: 'badge up' }, 'leader') : m.role === 'collector' ? h('span', { class: 'badge unknown' }, 'collector') : h('span', { class: 'badge info' }, 'standby'), alive ? null : h('span', { class: 'badge down', style: 'margin-left:6px' }, 'unreachable')),
            h('td', { class: 'small mono' }, m.addr, h('div', { class: 'faint' }, m.console)),
            h('td', { class: 'num r' }, self ? (st.assigned || 0) : (s.assigned || 0)),
            h('td', { class: 'small' }, self ? '—' : (s.last_seen ? ago(s.last_seen) + (s.rtt_ms ? ' · ' + s.rtt_ms.toFixed(1) + ' ms' : '') : 'never')),
            h('td', { class: 'small' }, self || m.role === 'collector' ? '—' : (s.mirror_age !== undefined && s.last_seen ? (s.mirror_age < 60 ? 'in sync (' + Math.round(s.mirror_age) + ' s)' : h('span', { style: 'color:var(--major)' }, Math.round(s.mirror_age / 60) + ' min behind')) : '—'), s.queue ? h('div', { class: 'faint' }, s.queue + ' queued') : null),
            h('td', { class: 'small muted' }, s.version || (self ? state.status.version : '')),
            h('td', null, !self && isAdmin ? h('button', { class: 'btn sm danger', onclick: async () => { if (confirm('Remove ' + m.name + ' from the cluster? Reinstall it to join again.')) { await post('/api/cluster/members', { remove: m.id }); render(); } } }, 'Remove') : null)));
        });
        // site pinning
        const pins = c.site_pins || {};
        const pinRows = Object.entries(c.sites || {}).map(([sid, sname]) => { const sel = h('select', { class: 'btn', disabled: !isAdmin, onchange: async () => { pins[sid] = sel.value; await post('/api/cluster/members', { pins }); toast('Saved', 'ok'); } }, h('option', { value: '' }, 'any node (hashed)'), ...members.map(m => h('option', { value: m.id, selected: pins[sid] === m.id }, m.name + (m.role === 'collector' ? ' (collector)' : '')))); return h('tr', null, h('td', null, sname), h('td', null, sel)); });
        body.append(head,
          h('div', { class: 'panel tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Node'), h('th', null, 'Role'), h('th', null, 'Addresses'), h('th', { class: 'r' }, 'Polls'), h('th', null, 'Heartbeat'), h('th', null, 'Data copy'), h('th', null, 'Version'), h('th', null, ''))), tb)),
          h('div', { class: 'grid c2', style: 'margin-top:12px' },
            h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Pin sites to nodes'), h('span', { class: 'hint' }, 'a branch collector polls its own site; unpinned sites are spread by hash')), h('table', { class: 'tbl' }, h('tbody', null, ...pinRows))),
            h('div', { class: 'panel pb' }, h('h3', null, 'How it works'), h('p', { class: 'small muted' }, 'The leader runs the state engine, alerts, notifications, endpoint and configuration walks; every full node keeps a full copy of the data (state, history, logs, flows, configs) refreshed every 10 seconds, so a failover loses at most a few seconds of logs and up to 5 minutes of metrics. Standbys and collectors poll their share and forward samples, syslog, traps and flows to the leader. Point devices at any node — or at all of them. Open any node’s console; standbys proxy to the leader.'),
              h('p', { class: 'small muted' }, 'Failover needs a majority of full nodes (3 → survives 1 failure, 5 → survives 2). With two full nodes, promote manually: ', h('code', null, 'topolight -promote'), ' on the survivor. Rolling upgrade: upgrade standbys first, then the leader.'))));
        function tokenDialog(role) {
          post('/api/cluster/token', { role }).then(r => {
            const pre = h('pre', { class: 'snippet', style: 'white-space:pre-wrap;word-break:break-all' }, r.command);
            const m = modal('Join token (' + role + ', valid 24 hours, single use)', h('div', null, h('p', { class: 'small muted' }, 'Run this on the new server (Ubuntu/Debian/RHEL, as root). It installs TopoLight, joins this cluster and starts as a ' + (role === 'collector' ? 'collector — polls and forwards only, no data copy' : 'standby with a full data copy') + '.'), pre,
              h('p', { class: 'small muted' }, 'Already installed? ', h('span', { class: 'mono' }, r.manual)),
              h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => { navigator.clipboard.writeText(r.command).then(() => toast('Copied', 'ok')); } }, 'Copy command'), h('button', { class: 'btn primary', type: 'button', onclick: () => m.close() }, 'Done'))));
          }).catch(x => toast(x.message, 'err'));
        }
      } else if (tab === 'license') {
        const lic = await get('/api/license');
        const key = h('textarea', { rows: 4, placeholder: 'SNTL1-…', disabled: !isAdmin });
        const out = h('div', { class: 'small' });
        const caps = lic.caps;
        const inst = lic.instance || '';
        const copyInst = h('button', { class: 'btn sm', type: 'button', 'aria-label': 'Copy Instance ID', disabled: !inst, onclick: () => { navigator.clipboard.writeText(inst).then(() => toast('Instance ID copied', 'ok')); } }, 'Copy');
        body.append(h('div', { class: 'grid c2' },
          h('div', { class: 'panel pb' }, h('h3', null, cap(lic.tier) + ' edition'), h('p', { class: 'muted' }, lic.notice),
            h('dl', { class: 'kv' }, h('dt', null, 'Instance ID'), h('dd', null, h('code', { class: 'mono', style: 'font-size:14px;letter-spacing:.04em' }, inst || '— (no data directory)'), ' ', copyInst),
              lic.bound ? h('dt', null, 'Key bound to') : null, lic.bound ? h('dd', null, h('code', { class: 'mono' }, lic.bound)) : null,
              h('dt', null, 'Devices'), h('dd', null, caps.max_devices || 'unlimited'), h('dt', null, 'Sites'), h('dd', null, caps.max_sites || 'unlimited'), h('dt', null, 'Retention'), h('dd', null, caps.retention_days + ' days'), h('dt', null, 'Users'), h('dd', null, caps.max_users || 'unlimited'), h('dt', null, 'Telegram / webhook'), h('dd', null, caps.telegram ? 'yes' : 'no'), h('dt', null, 'Roles'), h('dd', null, caps.roles ? 'yes' : 'no'), h('dt', null, 'Export'), h('dd', null, caps.export ? 'yes' : 'no')),
            h('p', { class: 'small muted', style: 'margin-top:12px' }, 'Free: 25 devices · Pro: 500 devices, 3 sites, 6 months · Team: 1,500 devices, unlimited sites, 12 months, roles. Every feature is in every edition.'),
            h('p', { class: 'small muted' }, 'A licence key is issued for one Instance ID — enter the ID above at checkout. A cluster shares one Instance ID, so the key keeps working after failover. ',
              h('a', { href: 'https://whop.com/checkout/plan_rP8yCv7zVAAF6', target: '_blank', rel: 'noopener' }, 'Get Pro'), ' · ', h('a', { href: 'https://whop.com/checkout/plan_B4Z4t1MnPZvVJ', target: '_blank', rel: 'noopener' }, 'Get Team'), ' · ', h('a', { href: 'https://whop.com/topolight', target: '_blank', rel: 'noopener' }, 'Compare on Whop'), '.')),
          h('form', { class: 'panel pb form', onsubmit: async e => { e.preventDefault(); try { const r = await put('/api/license', { key: key.value }); out.textContent = r.license.notice + ` — ${r.monitored} monitored, ${r.unmonitored} not monitored.`; state.status = await get('/api/status'); refreshTop(); } catch (x) { out.textContent = x.message; } } },
            h('label', null, 'Licence key', key), h('div', { class: 'hint' }, 'Keys are verified offline with the issuer’s public key; nothing is sent anywhere. Stored in <data>/license.key. A key bound to another Instance ID is rejected here — ask for a re-issue if you moved servers.'), h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit', disabled: !isAdmin }, 'Apply')), out)));
      } else if (tab === 'settings') {
        const s = await get('/api/settings');
        const name = h('input', { value: s.instance_name }), url = h('input', { value: s.console_url || '', placeholder: 'https://nms.example.com' }), poll = h('input', { type: 'number', value: s.default_poll, min: 15, max: 3600 }), disc = h('input', { type: 'number', value: s.discovery_every, min: 0 }), topo = h('input', { type: 'number', value: s.topology_every, min: 5 }), bk = h('input', { type: 'number', value: s.backup_every_hours || 24, min: -1, max: 720 });
        body.append(h('form', { class: 'form panel pb', style: 'max-width:640px', onsubmit: async e => { e.preventDefault(); try { await put('/api/settings', { instance_name: name.value, console_url: url.value, default_poll: Number(poll.value), discovery_every: Number(disc.value), topology_every: Number(topo.value), backup_every_hours: Number(bk.value) }); toast('Saved', 'ok'); state.status = await get('/api/status'); refreshTop(); } catch (x) { toast(x.message, 'err'); } } },
          h('div', { class: 'row' }, h('label', null, 'Instance name', name), h('label', null, 'Console URL (for notification links)', url)),
          h('div', { class: 'row' }, h('label', null, 'Default poll interval (s)', poll), h('label', null, 'Discovery sweep every (min, 0 = off)', disc), h('label', null, 'Topology refresh every (min)', topo)),
          h('div', { class: 'row' }, h('label', null, 'Configuration backup every (hours; -1 = off)', bk)),
          h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit', disabled: !isAdmin }, 'Save'))));
      } else if (tab === 'about') {
        const s = await get('/api/status'); const c = s.collectors || {};
        const unknown = Object.entries(c.syslog_unknown_hosts || {});
        body.append(h('div', { class: 'grid c2' },
          h('div', { class: 'panel pb' }, h('h3', null, 'Collectors'), h('dl', { class: 'kv', style: 'margin-top:8px' },
            h('dt', null, 'ICMP'), h('dd', null, c.icmp ? 'on' : h('span', { style: 'color:var(--major)' }, c.icmp_error)), h('dt', null, 'Poll cycles'), h('dd', { class: 'num' }, c.poll_cycles + ' (' + c.poll_failures + ' failed)'),
            h('dt', null, 'Syslog'), h('dd', null, (c.syslog_addr || 'off') + ' · ' + c.syslog_received + ' received, ' + c.syslog_dropped + ' dropped'), h('dt', null, 'Traps'), h('dd', null, (c.trap_addr || 'off') + ' · v2c ' + c.trap_received + ' received, ' + c.trap_rejected + ' rejected · v3 ' + (c.trap_v3_received || 0) + ' received, ' + (c.trap_v3_rejected || 0) + ' rejected', c.trap_engine_id ? h('div', { class: 'small muted' }, 'v3 engine id for informs: ', h('span', { class: 'mono' }, c.trap_engine_id)) : null, c.trap_v3_last_error ? h('div', { class: 'small', style: 'color:var(--major)' }, 'last v3 error: ' + c.trap_v3_last_error) : null),
            h('dt', null, 'Flow'), h('dd', null, c.flow ? (c.flow_addr || 'off') + ' / ' + (c.sflow_addr || 'off') + ' · ' + (c.flow.datagrams || 0) + ' datagrams, ' + (c.flow.exporters || 0) + ' exporters · ' + fmtBytes(c.flow.disk_bytes || 0) : 'off'),
            h('dt', null, 'Endpoints'), h('dd', null, c.endpoints ? (c.endpoints.endpoints || 0) + ' MACs, ' + (c.endpoints.with_ip || 0) + ' with IP · ' + (c.endpoint_walks || 0) + ' table walks' : 'off'),
            h('dt', null, 'Metrics'), h('dd', null, c.series + ' series · ' + fmtBytes(c.tsdb_bytes)), h('dt', null, 'Log lines'), h('dd', { class: 'num' }, c.logs_count), h('dt', null, 'Notifications'), h('dd', null, c.notify_sent + ' sent, ' + c.notify_failed + ' failed'),
            h('dt', null, 'Uptime'), h('dd', null, fmtDur(s.uptime_s)), h('dt', null, 'Version'), h('dd', null, s.product + ' ' + s.version))),
          h('div', { class: 'panel pb' }, h('h3', null, 'Unknown log sources'), h('p', { class: 'small muted' }, 'Hosts that send syslog but are not in the inventory. Add them as devices to attach their logs.'), unknown.length ? h('table', { class: 'tbl' }, h('tbody', null, ...unknown.map(([ip, n]) => h('tr', null, h('td', { class: 'mono' }, ip), h('td', { class: 'num r' }, n))))) : h('div', { class: 'muted small' }, 'None.'),
            h('h3', { style: 'margin-top:16px' }, 'Export'), h('p', { class: 'small muted' }, 'Inventory, links and alerts as JSON.'), h('a', { class: 'btn sm', href: '/api/export.json', target: '_blank' }, 'Download export.json'))));
        const profs = await get('/api/profiles');
        body.append(h('div', { class: 'panel', style: 'margin-top:16px' }, h('div', { class: 'ph' }, h('h3', null, 'Vendor profiles'), h('span', { class: 'hint' }, profs.length + ' loaded · add your own as JSON files in <data>/profiles/ using the same fields')),
          h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'ID'), h('th', null, 'Vendor'), h('th', null, 'Matches sysObjectID'), h('th', null, 'Role'), h('th', null, 'CPU OID'), h('th', null, 'LLDP / CDP'), h('th', { class: 'r' }, 'Priority'))),
            h('tbody', null, ...profs.map(p => h('tr', null, h('td', { class: 'mono' }, p.id), h('td', null, p.vendor), h('td', { class: 'mono small' }, (p.match || []).join(', ') || (p.descr_match ? 'sysDescr ~ ' + p.descr_match : '—')), h('td', null, p.role || '—'), h('td', { class: 'mono small' }, p.cpu ? p.cpu.oid : '—'), h('td', null, (p.lldp ? 'LLDP' : '') + (p.lldp && p.cdp ? ' + ' : '') + (p.cdp ? 'CDP' : '')), h('td', { class: 'r num' }, p.priority)))))),
          h('div', { class: 'pb small muted' }, 'Example file: ', h('code', null, '{"id":"my-vendor","vendor":"Acme","match":["1.3.6.1.4.1.99999"],"role":"access","cpu":{"oid":"1.3.6.1.4.1.99999.1.1.0"},"mem_used":{"oid":"…"},"mem_free":{"oid":"…"},"temp":{"oid":"…","walk":true,"agg":"max"},"lldp":true,"priority":20}'))));
      }
    }
    function siteDialog(s, done) {
      const name = h('input', { value: s ? s.name : '', required: true }), sub = h('textarea', { rows: 4 }, s ? s.subnets.join('\n') : '');
      const credSel = h('select', null, h('option', { value: '' }, 'any credential'));
      const apo = h('input', { type: 'checkbox', checked: !!(s && s.add_ping_only) });
      const sshSel = h('select', null, h('option', { value: '' }, 'none — no configuration backups'));
      get('/api/creds').then(cs => cs.forEach(c => { if (c.kind === 'ssh') sshSel.append(h('option', { value: c.id, selected: s && s.ssh_cred_id === c.id }, c.name)); else credSel.append(h('option', { value: c.id, selected: s && s.cred_id === c.id }, c.name)); }));
      const err = h('div', { class: 'err' });
      const m = modal(s ? 'Edit site' : 'New site', h('form', { class: 'form', onsubmit: async e => {
        e.preventDefault();
        const body = { name: name.value.trim(), subnets: sub.value.split(/[\n,]/).map(x => x.trim()).filter(Boolean), cred_id: credSel.value, ssh_cred_id: sshSel.value, add_ping_only: apo.checked };
        try { if (s) await put('/api/sites/' + s.id, body); else await post('/api/sites', body); m.close(); done(); } catch (x) { err.textContent = x.message; }
      } }, h('label', null, 'Name', name), h('label', null, 'Ranges (CIDR, single IP or a-b; one per line)', sub), h('div', { class: 'row' }, h('label', null, 'SNMP credential', credSel), h('label', null, 'SSH credential (config backup)', sshSel)), h('label', { class: 'check' }, apo, ' Keep hosts that answer ping but no SNMP as ping-only devices (counts toward the device cap — on an office LAN this adds every PC)'), err, h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Save'))));
    }
    function credDialog(c, done) {
      const err = h('div', { class: 'err' });
      const f = credForm(c || { version: '3', auth_proto: 'sha', priv_proto: 'aes', user: 'topolight' }, async out => { try { if (c) await put('/api/creds/' + c.id, out); else await post('/api/creds', out); m.close(); done(); } catch (x) { err.textContent = x.message; } }, 'Save');
      const m = modal(c ? 'Edit credential' : 'New credential', h('div', null, f, err));
    }
    function integDialog(i, done) {
      const kind = h('select', null, h('option', { value: 'unifi', selected: !i || i.kind === 'unifi' }, 'UniFi Network (controller / UniFi OS)'), h('option', { value: 'meraki', selected: i && i.kind === 'meraki' }, 'Cisco Meraki (Dashboard API)'));
      const name = h('input', { value: i ? i.name : '', placeholder: 'e.g. HQ UniFi', maxlength: 60 });
      const url = h('input', { value: i ? i.url || '' : '', placeholder: 'https://unifi.example.com:8443' });
      const site = h('input', { value: i && i.kind !== 'meraki' ? i.site || '' : '', placeholder: 'default' });
      const org = h('input', { value: i && i.kind === 'meraki' ? i.site || '' : '', placeholder: 'blank = every organisation the key can see' });
      const user = h('input', { value: i ? i.username || '' : '', placeholder: 'topolight (read-only)', autocomplete: 'off' });
      const pass = h('input', { type: 'password', value: '', placeholder: i && i.password ? '(unchanged)' : '', autocomplete: 'new-password' });
      const key = h('input', { type: 'password', value: '', placeholder: i && i.api_key ? '(unchanged)' : 'Meraki API key', autocomplete: 'new-password' });
      const insecure = h('input', { type: 'checkbox', checked: i ? !!i.insecure : true });
      const every = h('input', { type: 'number', value: i ? i.every || 60 : 60, min: 30, max: 3600 });
      const siteSel = h('select', null, h('option', { value: '' }, 'first site'), ...state.sites.map(s => h('option', { value: s.id, selected: i && i.site_id === s.id }, s.name)));
      const enabled = h('input', { type: 'checkbox', checked: i ? !!i.enabled : true });
      const err = h('div', { class: 'err' });
      const uni = h('div', null, h('label', null, 'Controller URL', url), h('label', null, 'UniFi site (name in the URL, not the display name)', site), h('label', null, 'Username', user), h('label', null, 'Password', pass), h('label', { class: 'check' }, insecure, ' accept the controller’s self-signed certificate'));
      const mer = h('div', null, h('label', null, 'API key', key), h('label', null, 'Organisation id', org));
      function sw() { const u = kind.value === 'unifi'; uni.hidden = !u; mer.hidden = u; }
      kind.onchange = sw;
      const m = modal(i ? 'Edit integration' : 'New integration', h('form', { class: 'form', onsubmit: async e => {
        e.preventDefault(); err.textContent = '';
        const out = { kind: kind.value, name: name.value.trim(), url: url.value.trim(), site: (kind.value === 'meraki' ? org : site).value.trim(), username: user.value.trim(), password: pass.value, api_key: key.value.trim(), insecure: insecure.checked, every: Number(every.value), site_id: siteSel.value, enabled: enabled.checked };
        try { if (i) await put('/api/integrations/' + i.id, out); else await post('/api/integrations', out); m.close(); done(); } catch (x) { err.textContent = x.message; }
      } }, h('label', null, 'Type', kind), h('label', null, 'Name', name), uni, mer, h('div', { class: 'grid c2' }, h('label', null, 'Poll every (s)', every), h('label', null, 'Put imported devices in', siteSel)), h('label', { class: 'check' }, enabled, ' enabled'), err,
        h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Save'))));
      sw();
    }
    function testCred(c) {
      const ip = h('input', { placeholder: '10.20.1.1', required: true }); const out = h('div', { class: 'small', style: 'margin-top:8px' });
      const m = modal('Test ' + c.name, h('form', { class: 'form', onsubmit: async e => { e.preventDefault(); out.textContent = 'Testing…'; const r = await post('/api/creds/' + c.id + '/test', { ip: ip.value.trim() }); out.innerHTML = r.ok ? `<span class="ok">✓ ${esc(r.sys_name)}</span> · ${esc(r.vendor || '')} (${esc(r.profile)}) · ${r.ms} ms<br><span class="muted">${esc(r.sys_descr)}</span>` : `<span class="err">✗ ${esc(r.error)}</span> after ${r.ms} ms`; } }, h('label', null, 'Device IP', ip), h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Close'), h('button', { class: 'btn primary', type: 'submit' }, 'Test')), out));
    }
    function discoverDialog(s) {
      const stat = h('div', { class: 'muted small' }, 'Starting…'), prog = h('div', { class: 'progress' }, h('i', { style: 'width:0%' })), found = h('div', { class: 'found' });
      const m = modal('Discovering ' + s.name, h('div', null, prog, stat, found, h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => m.close() }, 'Close'))), () => clearInterval(t));
      post('/api/sites/' + s.id + '/discover').catch(x => { stat.textContent = x.message; });
      const seen = new Set();
      const t = setInterval(async () => {
        try {
          const p = await get('/api/sites/' + s.id + '/discovery');
          if (p.total) prog.firstChild.style.width = Math.round(p.scanned / p.total * 100) + '%';
          stat.textContent = `${p.scanned || 0}/${p.total || 0} scanned · ${p.answered || 0} answered ping · ${p.found || 0} SNMP devices · ${p.added || 0} new` + (p.skipped ? ` · ${p.skipped} over licence limit` : '');
          const devs = await get('/api/devices?site=' + s.id);
          devs.forEach(d => { if (!seen.has(d.id)) { seen.add(d.id); found.append(h('div', null, h('span', { class: 'sdot ' + (d.monitored ? d.status : 'unknown') }), h('b', null, d.name), h('span', { class: 'muted' }, d.ip + ' · ' + (d.vendor || '')))); } });
          if (!p.running && p.total !== undefined) { clearInterval(t); stat.textContent += ' — done.'; }
        } catch (x) { }
      }, 1500);
    }
    function tokenDialog(done) {
      const name = h('input', { placeholder: 'e.g. grafana, backup-script', required: true, maxlength: 60 });
      const role = h('select', null, ...['viewer', 'operator', 'admin'].map(r => h('option', { value: r }, r)));
      const err = h('div', { class: 'err' });
      const m = modal('New API token', h('form', { class: 'form', onsubmit: async e => {
        e.preventDefault();
        try {
          const r = await post('/api/tokens', { Name: name.value.trim(), Role: role.value });
          m.close();
          const pre = h('pre', { class: 'snippet', style: 'white-space:pre-wrap;word-break:break-all' }, r.secret);
          const m2 = modal('Token created — copy it now', h('div', null, h('p', { class: 'small muted' }, 'This is the only time the secret is shown. TopoLight stores a hash.'), pre, h('p', { class: 'small muted mono' }, r.example),
            h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => { navigator.clipboard.writeText(r.secret).then(() => toast('Copied', 'ok')); } }, 'Copy'), h('button', { class: 'btn primary', type: 'button', onclick: () => { m2.close(); done(); } }, 'Done'))));
        } catch (x) { err.textContent = x.message; }
      } }, h('div', { class: 'row' }, h('label', null, 'Name', name), h('label', null, 'Role', role)), err, h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Create'))));
    }
    function userDialog(done) {
      const name = h('input', { required: true }), pass = h('input', { type: 'password', required: true, minlength: 10 }), role = h('select', null, ...['admin', 'operator', 'viewer'].map(r => h('option', { value: r }, r)));
      const err = h('div', { class: 'err' });
      const m = modal('New user', h('form', { class: 'form', onsubmit: async e => { e.preventDefault(); try { await post('/api/users', { Name: name.value.trim(), Password: pass.value, Role: role.value }); m.close(); done(); } catch (x) { err.textContent = x.message; } } }, h('div', { class: 'row' }, h('label', null, 'Name', name), h('label', null, 'Role', role)), h('label', null, 'Password (10+)', pass), err, h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Create'))));
    }
    function passwordDialog(u) {
      const pass = h('input', { type: 'password', required: true, minlength: 10 }); const err = h('div', { class: 'err' });
      const m = modal('Password for ' + u.name, h('form', { class: 'form', onsubmit: async e => { e.preventDefault(); try { await put('/api/users/' + u.id + '/password', { password: pass.value }); m.close(); toast('Password changed', 'ok'); } catch (x) { err.textContent = x.message; } } }, h('label', null, 'New password', pass), err, h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Change'))));
    }
    function maintenanceDialogSite(done) {
      const name = h('input', { required: true, placeholder: 'e.g. Core upgrade' }), site = h('select', null, h('option', { value: '' }, 'All sites'), ...state.sites.map(s => h('option', { value: s.id }, s.name))), from = h('input', { type: 'datetime-local', required: true }), to = h('input', { type: 'datetime-local', required: true }), reason = h('input');
      const err = h('div', { class: 'err' });
      const m = modal('Maintenance window', h('form', { class: 'form', onsubmit: async e => { e.preventDefault(); try { await post('/api/maintenance', { name: name.value, site_id: site.value, from: new Date(from.value).toISOString(), to: new Date(to.value).toISOString(), reason: reason.value }); m.close(); done(); } catch (x) { err.textContent = x.message; } } }, h('div', { class: 'row' }, h('label', null, 'Name', name), h('label', null, 'Scope', site)), h('div', { class: 'row' }, h('label', null, 'From', from), h('label', null, 'To', to)), h('label', null, 'Reason', reason), err, h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Create'))));
    }
    main.append(h('div', { class: 'page-head' }, h('h1', null, 'Admin')), tabs, body);
    await render();
    return {};
  }

  boot();
})();
