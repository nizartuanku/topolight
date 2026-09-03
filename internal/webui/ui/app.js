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
    const pages = { overview: pageOverview, topology: pageTopology, alerts: pageAlerts, devices: pageDevices, device: pageDevice, logs: pageLogs, admin: pageAdmin };
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
      ...[['overview', 'Overview'], ['topology', 'Topology'], ['alerts', 'Alerts'], ['devices', 'Devices'], ['logs', 'Logs'], ['admin', 'Admin']].map(([p, label]) => {
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
      if (state._g) { const m = { o: 'overview', t: 'topology', a: 'alerts', d: 'devices', l: 'logs', s: 'admin' }[e.key]; if (m) nav('#/' + m); state._g = false; }
      if (e.key === '?') { toast('Keys: / search · g o/t/a/d/l/s go to page · in alerts: j/k move, a ack, r resolve, Enter open', 'ok', 8000); }
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
      { label: 'Go to Logs', k: 'g l', run: () => nav('#/logs') }, { label: 'Admin', k: 'g s', run: () => nav('#/admin') },
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
    const ver = h('select', { onchange: () => toggle() }, h('option', { value: '3', selected: c.version === '3' }, 'SNMP v3 (recommended)'), h('option', { value: '2c', selected: c.version === '2c' }, 'SNMP v2c'));
    const v2 = h('div', { class: 'row' }, h('label', null, 'Community (read-only)', h('input', { id: 'cc', value: c.community || '', autocomplete: 'off' })));
    const v3 = h('div', null,
      h('div', { class: 'row' }, h('label', null, 'User', h('input', { id: 'cu', value: c.user || '' })),
        h('label', null, 'Auth protocol', h('select', { id: 'ca' }, ...['sha', 'sha256', 'md5', ''].map(p => h('option', { value: p, selected: (c.auth_proto || 'sha') === p }, p || 'none')))),
        h('label', null, 'Auth password', h('input', { id: 'cap', type: 'password', value: c.auth_pass || '', autocomplete: 'new-password' }))),
      h('div', { class: 'row' }, h('label', null, 'Privacy protocol', h('select', { id: 'cp' }, ...['aes', 'des', ''].map(p => h('option', { value: p, selected: (c.priv_proto || 'aes') === p }, p || 'none')))),
        h('label', null, 'Privacy password', h('input', { id: 'cpp', type: 'password', value: c.priv_pass || '', autocomplete: 'new-password' }))));
    function toggle() { const is3 = ver.value === '3'; v2.classList.toggle('hidden', is3); v3.classList.toggle('hidden', !is3); }
    f.append(h('div', { class: 'row' }, h('label', null, 'Name', h('input', { id: 'cn', value: c.name || '', required: true })), h('label', null, 'Version', ver)), v2, v3,
      h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit' }, submitLabel || 'Save')));
    toggle();
    f.onsubmit = e => {
      e.preventDefault();
      const out = { name: $('#cn', f).value.trim(), version: ver.value, community: $('#cc', f).value, user: $('#cu', f).value.trim(), auth_proto: $('#ca', f).value, auth_pass: $('#cap', f).value, priv_proto: $('#cp', f).value, priv_pass: $('#cpp', f).value };
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
    const W = canvas.width, H = canvas.height, padL = 46 * dpr, padB = 18 * dpr, padT = 8 * dpr, padR = 8 * dpr;
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
    const err = h('div', { class: 'err' });
    const m = modal('Add a device', h('form', { class: 'form', onsubmit: async e => {
      e.preventDefault();
      try { const r = await post('/api/devices', { IP: ip.value.trim(), SiteID: site.value, Name: name.value.trim() }); toast(r.warning ? r.warning : 'Added ' + r.device.name, r.warning ? 'major' : 'ok'); m.close(); done && done(); }
      catch (x) { err.textContent = x.message; }
    } }, h('div', { class: 'row' }, h('label', null, 'IP address', ip), h('label', null, 'Site', site)), h('label', null, 'Name', name), err, h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Add'))));
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
        dv.cause ? h('span', { class: 'badge unreachable' }, 'upstream ' + (d.cause_name || '') + ' down') : null, dv.monitored ? null : h('span', { class: 'badge minor' }, dv.notes || 'not monitored'),
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
      [['interfaces', 'Interfaces (' + d.interfaces.length + ')'], ['links', 'Links & neighbours'], ['alerts', 'Alerts & events'], ['snippets', 'Config snippets']].forEach(([k, l]) => tabs.append(h('button', { class: k === tab ? 'on' : '', onclick: () => { tab = k; location.hash = '#/device/' + id + '/' + k; renderBody(); } }, l)));
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
        if (!ifs.length) body.append(h('div', { class: 'empty' }, 'No interfaces yet — the inventory walk runs on the first poll.'));
      } else if (tab === 'links') {
        body.append(h('div', { class: 'grid c2' },
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Links (' + d.links.length + ')')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Local'), h('th', null, 'Remote'), h('th', null, 'Confidence'), h('th', null, 'Sources'), h('th', null, 'Util'))), h('tbody', null, ...d.links.map(l => { const mine = l.a_device === id; const other = mine ? l.b_device : l.a_device; return h('tr', null, h('td', { class: 'mono' }, h('span', { class: 'sdot ' + (l.status || 'up') }), ' ', mine ? l.a_if : l.b_if), h('td', null, l.external ? h('span', { class: 'muted' }, l.external_name) : h('a', { href: '#/device/' + other }, d.names[other] || other), ' ', h('span', { class: 'mono faint' }, mine ? l.b_if : l.a_if)), h('td', { class: 'num' }, l.confidence.toFixed(2), l.stale ? h('span', { class: 'badge unknown', style: 'margin-left:6px' }, 'stale') : null), h('td', { class: 'small muted' }, (l.sources || []).join(', ')), h('td', { class: 'num small' }, (l.util_pct || 0).toFixed(0) + '%')); })))),
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Raw neighbour observations (' + d.neighbors.length + ')')), h('table', { class: 'tbl' }, h('thead', null, h('tr', null, h('th', null, 'Local port'), h('th', null, 'Remote'), h('th', null, 'Remote port'), h('th', null, 'Via'))), h('tbody', null, ...d.neighbors.map(n => h('tr', null, h('td', { class: 'mono' }, n.local_if), h('td', null, n.remote_name || n.remote_mac || n.remote_ip, h('div', { class: 'small faint mono' }, [n.remote_mac, n.remote_ip].filter(Boolean).join(' · '))), h('td', { class: 'mono small' }, n.remote_port), h('td', { class: 'small muted' }, n.source + ' · ' + ago(n.seen_at)))))))));
      } else if (tab === 'alerts') {
        const evs = await get('/api/events?device=' + id + '&limit=100');
        body.append(h('div', { class: 'grid c2' },
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Open alerts')), h('div', { class: 'list', style: 'padding:6px' }, ...(d.alerts.length ? d.alerts.map(a => alertItem(a, {}, () => nav('#/alerts/' + a.id))) : [h('div', { class: 'empty' }, 'No open alerts')]))),
          h('div', { class: 'panel' }, h('div', { class: 'ph' }, h('h3', null, 'Events'), h('div', { class: 'spacer' }), h('a', { class: 'btn sm', href: '#/logs/' + id }, 'Syslog & traps')), h('div', { class: 'list', style: 'padding:6px' }, ...(evs.length ? evs.map(e => h('div', { class: 'item ' + (e.severity || 'info') }, h('span', { class: 'bar' }), h('div', null, h('div', { class: 't' }, h('span', null, dateTime(e.ts)), h('span', null, '· ' + e.source), h('span', null, '· ' + e.kind)), h('div', { class: 'd', style: 'color:var(--text)' }, e.message)))) : [h('div', { class: 'empty' }, 'No events yet')])))));
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
      const poll = h('input', { type: 'number', value: dv.poll_every, min: 15, max: 3600 }), mon = h('input', { type: 'checkbox', checked: dv.monitored });
      const site = h('select', null, ...state.sites.map(s => h('option', { value: s.id, selected: s.id === dv.site_id }, s.name)));
      const loc = h('input', { value: dv.location || '' }), notes = h('textarea', { rows: 2 }, dv.notes || '');
      const err = h('div', { class: 'err' });
      const m = modal('Edit ' + dv.name, h('form', { class: 'form', onsubmit: async e => {
        e.preventDefault();
        try { await put('/api/devices/' + id, { name: name.value, role: role.value, domain: domain.value, poll_every: Number(poll.value), monitored: mon.checked, site_id: site.value, location: loc.value, notes: notes.value }); m.close(); d = await get('/api/devices/' + id); renderHead(); renderTiles(); }
        catch (x) { err.textContent = x.message; }
      } }, h('div', { class: 'row' }, h('label', null, 'Name', name), h('label', null, 'Role', role), h('label', null, 'Domain', domain)), h('div', { class: 'row' }, h('label', null, 'Poll every (s)', poll), h('label', null, 'Site', site), h('label', null, 'Location', loc)), h('label', { class: 'check' }, mon, ' Monitored'), h('label', null, 'Notes', notes), err,
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

  // ---------- admin ----------
  async function pageAdmin(main, params) {
    let tab = params[0] || 'sites';
    const tabs = h('div', { class: 'tabs' });
    const body = h('div');
    const TABS = [['sites', 'Sites'], ['creds', 'Credentials'], ['notify', 'Notifications'], ['rules', 'Alert rules'], ['maintenance', 'Maintenance'], ['users', 'Users'], ['license', 'Licence'], ['settings', 'Settings'], ['about', 'System']];
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
        creds.forEach(c => tb.append(h('tr', null, h('td', null, h('b', null, c.name)), h('td', null, 'v' + c.version), h('td', { class: 'small muted' }, c.version === '3' ? `${c.user} · ${c.auth_proto || 'noauth'}/${c.priv_proto || 'nopriv'}` : 'community ••••'), h('td', null, h('button', { class: 'btn sm', onclick: () => testCred(c) }, 'Test'), ' ', h('button', { class: 'btn sm', onclick: () => credDialog(c, render) }, 'Edit'), ' ', isAdmin ? h('button', { class: 'btn sm danger', onclick: async () => { await del('/api/creds/' + c.id); render(); } }, 'Delete') : null))));
        body.append(h('div', { class: 'page-head' }, h('span', { class: 'sub' }, 'Read-only SNMP credentials. Discovery tries the site’s credential first, then the others.'), h('div', { class: 'spacer' }), h('button', { class: 'btn primary', onclick: () => credDialog(null, render) }, '+ Credential')),
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
      } else if (tab === 'license') {
        const lic = await get('/api/license');
        const key = h('textarea', { rows: 4, placeholder: 'SNTL1-…', disabled: !isAdmin });
        const out = h('div', { class: 'small' });
        const caps = lic.caps;
        body.append(h('div', { class: 'grid c2' },
          h('div', { class: 'panel pb' }, h('h3', null, cap(lic.tier) + ' edition'), h('p', { class: 'muted' }, lic.notice),
            h('dl', { class: 'kv' }, h('dt', null, 'Devices'), h('dd', null, caps.max_devices || 'unlimited'), h('dt', null, 'Sites'), h('dd', null, caps.max_sites || 'unlimited'), h('dt', null, 'Retention'), h('dd', null, caps.retention_days + ' days'), h('dt', null, 'Users'), h('dd', null, caps.max_users || 'unlimited'), h('dt', null, 'Telegram / webhook'), h('dd', null, caps.telegram ? 'yes' : 'no'), h('dt', null, 'Roles'), h('dd', null, caps.roles ? 'yes' : 'no'), h('dt', null, 'Export'), h('dd', null, caps.export ? 'yes' : 'no')),
            h('p', { class: 'small muted', style: 'margin-top:12px' }, 'Free: 25 devices · Pro: 500 devices, 3 sites, 6 months · Team: 1,500 devices, unlimited sites, 12 months, roles. ', h('a', { href: 'https://whop.com/topolight', target: '_blank', rel: 'noopener' }, 'Get a licence on Whop'), '.')),
          h('form', { class: 'panel pb form', onsubmit: async e => { e.preventDefault(); try { const r = await put('/api/license', { key: key.value }); out.textContent = r.license.notice + ` — ${r.monitored} monitored, ${r.unmonitored} not monitored.`; state.status = await get('/api/status'); refreshTop(); } catch (x) { out.textContent = x.message; } } },
            h('label', null, 'Licence key', key), h('div', { class: 'hint' }, 'Keys are verified offline with the issuer’s public key; nothing is sent anywhere. Stored in <data>/license.key.'), h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit', disabled: !isAdmin }, 'Apply')), out)));
      } else if (tab === 'settings') {
        const s = await get('/api/settings');
        const name = h('input', { value: s.instance_name }), url = h('input', { value: s.console_url || '', placeholder: 'https://nms.example.com' }), poll = h('input', { type: 'number', value: s.default_poll, min: 15, max: 3600 }), disc = h('input', { type: 'number', value: s.discovery_every, min: 0 }), topo = h('input', { type: 'number', value: s.topology_every, min: 5 });
        body.append(h('form', { class: 'form panel pb', style: 'max-width:640px', onsubmit: async e => { e.preventDefault(); try { await put('/api/settings', { instance_name: name.value, console_url: url.value, default_poll: Number(poll.value), discovery_every: Number(disc.value), topology_every: Number(topo.value) }); toast('Saved', 'ok'); state.status = await get('/api/status'); refreshTop(); } catch (x) { toast(x.message, 'err'); } } },
          h('div', { class: 'row' }, h('label', null, 'Instance name', name), h('label', null, 'Console URL (for notification links)', url)),
          h('div', { class: 'row' }, h('label', null, 'Default poll interval (s)', poll), h('label', null, 'Discovery sweep every (min, 0 = off)', disc), h('label', null, 'Topology refresh every (min)', topo)),
          h('div', { class: 'actions' }, h('button', { class: 'btn primary', type: 'submit', disabled: !isAdmin }, 'Save'))));
      } else if (tab === 'about') {
        const s = await get('/api/status'); const c = s.collectors || {};
        const unknown = Object.entries(c.syslog_unknown_hosts || {});
        body.append(h('div', { class: 'grid c2' },
          h('div', { class: 'panel pb' }, h('h3', null, 'Collectors'), h('dl', { class: 'kv', style: 'margin-top:8px' },
            h('dt', null, 'ICMP'), h('dd', null, c.icmp ? 'on' : h('span', { style: 'color:var(--major)' }, c.icmp_error)), h('dt', null, 'Poll cycles'), h('dd', { class: 'num' }, c.poll_cycles + ' (' + c.poll_failures + ' failed)'),
            h('dt', null, 'Syslog'), h('dd', null, (c.syslog_addr || 'off') + ' · ' + c.syslog_received + ' received, ' + c.syslog_dropped + ' dropped'), h('dt', null, 'Traps'), h('dd', null, (c.trap_addr || 'off') + ' · ' + c.trap_received + ' received, ' + c.trap_rejected + ' rejected'),
            h('dt', null, 'Metrics'), h('dd', null, c.series + ' series · ' + fmtBytes(c.tsdb_bytes)), h('dt', null, 'Log lines'), h('dd', { class: 'num' }, c.logs_count), h('dt', null, 'Notifications'), h('dd', null, c.notify_sent + ' sent, ' + c.notify_failed + ' failed'),
            h('dt', null, 'Uptime'), h('dd', null, fmtDur(s.uptime_s)), h('dt', null, 'Version'), h('dd', null, s.product + ' ' + s.version))),
          h('div', { class: 'panel pb' }, h('h3', null, 'Unknown log sources'), h('p', { class: 'small muted' }, 'Hosts that send syslog but are not in the inventory. Add them as devices to attach their logs.'), unknown.length ? h('table', { class: 'tbl' }, h('tbody', null, ...unknown.map(([ip, n]) => h('tr', null, h('td', { class: 'mono' }, ip), h('td', { class: 'num r' }, n))))) : h('div', { class: 'muted small' }, 'None.'),
            h('h3', { style: 'margin-top:16px' }, 'Export'), h('p', { class: 'small muted' }, 'Inventory, links and alerts as JSON (Pro/Team).'), h('a', { class: 'btn sm', href: '/api/export.json', target: '_blank' }, 'Download export.json'))));
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
      get('/api/creds').then(cs => cs.forEach(c => credSel.append(h('option', { value: c.id, selected: s && s.cred_id === c.id }, c.name))));
      const err = h('div', { class: 'err' });
      const m = modal(s ? 'Edit site' : 'New site', h('form', { class: 'form', onsubmit: async e => {
        e.preventDefault();
        const body = { name: name.value.trim(), subnets: sub.value.split(/[\n,]/).map(x => x.trim()).filter(Boolean), cred_id: credSel.value };
        try { if (s) await put('/api/sites/' + s.id, body); else await post('/api/sites', body); m.close(); done(); } catch (x) { err.textContent = x.message; }
      } }, h('label', null, 'Name', name), h('label', null, 'Ranges (CIDR, single IP or a-b; one per line)', sub), h('label', null, 'Credential', credSel), err, h('div', { class: 'actions' }, h('button', { class: 'btn', type: 'button', onclick: () => m.close() }, 'Cancel'), h('button', { class: 'btn primary', type: 'submit' }, 'Save'))));
    }
    function credDialog(c, done) {
      const err = h('div', { class: 'err' });
      const f = credForm(c || { version: '3', auth_proto: 'sha', priv_proto: 'aes', user: 'topolight' }, async out => { try { if (c) await put('/api/creds/' + c.id, out); else await post('/api/creds', out); m.close(); done(); } catch (x) { err.textContent = x.message; } }, 'Save');
      const m = modal(c ? 'Edit credential' : 'New credential', h('div', null, f, err));
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
