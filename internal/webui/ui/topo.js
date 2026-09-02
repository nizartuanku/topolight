/* TopoLight topology renderer — a hand-written perspective projection on a
   plain 2D canvas. No WebGL, no third-party code. Nodes carry x,y (ring
   layout from the server) and z (tier); the view can orbit (3D) or lie flat
   (2D). Status colours come from CSS variables so both themes work. */
(function () {
  'use strict';

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  const SHAPE = { core: 'square', distribution: 'square', access: 'square', router: 'circle', firewall: 'hex', ap: 'triangle', server: 'rect', other: 'circle' };
  const SIZE = { core: 13, distribution: 10, access: 8, router: 10, firewall: 10, ap: 6, server: 8, other: 7 };
  const TIER_H = 110; // vertical distance per tier (layout units)
  const RING_MIN = { 3: 60, 2: 60, 1: 90, 0: 120 }; // smallest disc per tier

  function shortLabel(name) { name = String(name || ''); const i = name.indexOf('.'); if (i > 0 && !/^\d+\.\d+\.\d+\.\d+$/.test(name)) name = name.slice(0, i); return name.length > 22 ? name.slice(0, 21) + '…' : name; }

  function Topo(canvas, opts) {
    this.canvas = canvas;
    this.ctx = canvas.getContext('2d');
    this.opts = Object.assign({ mode: '3d', overlay: 'status', onSelect: null, onHover: null, showGuess: false }, opts || {});
    this.nodes = [];
    this.links = [];
    this.byId = {};
    this.theta = 0.55; this.phi = 1.05; this.zoom = 1; this.panX = 0; this.panY = 0;
    this.auto = true; this.selected = null; this.hover = null;
    this.pulse = null; this.dpr = window.devicePixelRatio || 1;
    this.reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    this._bind();
    this._colors();
    this.resize();
    this._raf = requestAnimationFrame(this._loop.bind(this));
  }

  Topo.prototype._colors = function () {
    this.C = {
      up: cssVar('--ok'), degraded: cssVar('--major'), down: cssVar('--crit'), flapping: cssVar('--crit'), unreachable: cssVar('--unk'),
      unknown: cssVar('--unk'), maintenance: cssVar('--maint'), link: cssVar('--link'), grid: cssVar('--grid'), bg: cssVar('--surface'),
      text: cssVar('--text'), faint: cssVar('--faint'), accent: cssVar('--accent'), major: cssVar('--major'), minor: cssVar('--minor'), crit: cssVar('--crit'),
      mono: cssVar('--mono') || 'monospace', font: cssVar('--font') || 'sans-serif'
    };
  };

  Topo.prototype.setData = function (nodes, links) {
    const old = this.byId;
    this.nodes = nodes.map(n => {
      const o = old[n.id];
      const t = { id: n.id, name: n.name, role: n.role || 'other', status: n.status || 'unknown', x: n.X !== undefined ? n.X : n.x, y: n.Y !== undefined ? n.Y : n.y, z: n.Z !== undefined ? n.Z : n.z, external: !!n.external, cause: n.cause, alerts: n.alerts || 0, data: n };
      if (o && o.status !== t.status && !this.reduce) this.pulse = { id: t.id, t: 0.7, color: this.statusColor(t.status) };
      return t;
    });
    this.byId = {};
    this.nodes.forEach(n => { this.byId[n.id] = n; });
    this.links = links.filter(l => this.byId[l.a_device] && this.byId[l.b_device]);
    this.recenter();
  };

  Topo.prototype.recenter = function () {
    if (!this.nodes.length) return;
    let maxR = 1, zmin = 99, zmax = -99;
    const tiersSeen = {};
    this.nodes.forEach(n => { maxR = Math.max(maxR, Math.hypot(n.x, n.y)); zmin = Math.min(zmin, n.z || 0); zmax = Math.max(zmax, n.z || 0); tiersSeen[n.z || 0] = true; });
    // each disc is drawn a little wider than the outermost node on it
    const ringR = {};
    this.nodes.forEach(n => { const t = n.z || 0; ringR[t] = Math.max(ringR[t] || 0, Math.hypot(n.x, n.y) + 28); });
    Object.keys(ringR).forEach(t => { ringR[t] = Math.max(ringR[t], RING_MIN[t] || 40); maxR = Math.max(maxR, ringR[t]); });
    this.fit = maxR + 10; this.zmin = zmin; this.zmax = zmax; this.tiersSeen = tiersSeen; this.ringR = ringR;
  };

  Topo.prototype.updateNode = function (n) {
    const t = this.byId[n.id];
    if (!t) return;
    if (t.status !== n.status && !this.reduce) this.pulse = { id: t.id, t: 0.7, color: this.statusColor(n.status) };
    t.status = n.status; t.cause = n.cause; t.data = n; t.name = n.name; t.role = n.role || t.role;
  };

  Topo.prototype.updateLink = function (l) {
    const i = this.links.findIndex(x => x.id === l.id);
    if (i >= 0) this.links[i] = Object.assign(this.links[i], l);
  };

  Topo.prototype.statusColor = function (s) { return this.C[s] || this.C.unknown; };

  Topo.prototype.resize = function () {
    const r = this.canvas.getBoundingClientRect();
    this.dpr = window.devicePixelRatio || 1;
    this.canvas.width = Math.max(1, r.width * this.dpr);
    this.canvas.height = Math.max(1, r.height * this.dpr);
  };

  Topo.prototype.destroy = function () { cancelAnimationFrame(this._raf); this.dead = true; };

  // projection: rotate around the vertical axis by theta, tilt the discs by phi
  Topo.prototype.project = function (n) {
    const W = this.canvas.width, H = this.canvas.height;
    const flat = this.opts.mode === '2d';
    const tilt = flat ? 1 : Math.cos(this.phi);
    const lift = flat ? 0 : Math.sin(this.phi) * TIER_H;
    const zmid = ((this.zmin || 0) + (this.zmax || 0)) / 2;
    const fit = this.fit || 300;
    const wExt = 2 * fit, hExt = flat ? 2 * fit : 2 * fit * tilt + ((this.zmax || 0) - (this.zmin || 0)) * lift + 60;
    const scale = Math.min(W / wExt, H / hExt) * 0.9 * this.zoom;
    const cx = n.x * Math.cos(this.theta) - n.y * Math.sin(this.theta);
    const cy = n.x * Math.sin(this.theta) + n.y * Math.cos(this.theta);
    const px = W / 2 + this.panX * this.dpr + cx * scale;
    const py = H / 2 + this.panY * this.dpr + cy * scale * tilt - ((n.z || 0) - zmid) * lift * scale;
    const depth = flat ? 0 : cy;
    return [px, py, depth, scale];
  };

  Topo.prototype._loop = function () {
    if (this.dead) return;
    this._raf = requestAnimationFrame(this._loop.bind(this));
    if (this.auto && !this.reduce && this.opts.mode === '3d' && !this.drag) this.theta += 0.0012;
    this.draw();
  };

  Topo.prototype.draw = function () {
    const ctx = this.ctx, W = this.canvas.width, H = this.canvas.height, s = this.dpr;
    ctx.clearRect(0, 0, W, H);
    if (!this.nodes.length) return;
    // tier rings
    if (this.opts.mode === '3d') {
      const tilt = Math.cos(this.phi);
      [3, 2, 1, 0].forEach(t => {
        if (!this.tiersSeen || !this.tiersSeen[t]) return;
        const [cx, cy, , scale] = this.project({ x: 0, y: 0, z: t });
        const rr = (this.ringR[t] || RING_MIN[t]) * scale;
        ctx.beginPath();
        ctx.ellipse(cx, cy, rr, rr * tilt, 0, 0, Math.PI * 2);
        ctx.fillStyle = this.C.grid; ctx.globalAlpha = 0.18; ctx.fill(); ctx.globalAlpha = 1;
        ctx.strokeStyle = this.C.grid; ctx.lineWidth = 1 * s; ctx.stroke();
      });
    }
    // links (sorted back to front)
    const proj = {};
    this.nodes.forEach(n => { proj[n.id] = this.project(n); });
    const links = this.links.slice().sort((a, b) => (proj[a.a_device][2] + proj[a.b_device][2]) - (proj[b.a_device][2] + proj[b.b_device][2]));
    links.forEach(l => {
      if (!this.opts.showGuess && l.confidence < 0.8 && !l.manual) return;
      const [ax, ay] = proj[l.a_device], [bx, by] = proj[l.b_device];
      let color = this.C.link, width = 1;
      const speed = l.speed_mbps || 0;
      width = speed >= 40000 ? 3.5 : speed >= 10000 ? 2.5 : speed >= 1000 ? 1.6 : 1.1;
      if (this.opts.overlay === 'util') {
        const u = l.util_pct || 0;
        color = u > 85 ? this.C.major : u > 60 ? this.C.minor : this.C.link;
      }
      if (l.status === 'down') color = this.C.crit;
      const a = this.byId[l.a_device], b = this.byId[l.b_device];
      if (a.status === 'unreachable' || b.status === 'unreachable') color = this.C.unknown;
      if (l.stale) color = this.C.unknown;
      ctx.beginPath(); ctx.moveTo(ax, ay); ctx.lineTo(bx, by);
      ctx.strokeStyle = color; ctx.lineWidth = width * s;
      ctx.globalAlpha = (l.confidence < 0.8 || l.stale) ? 0.55 : 0.9;
      ctx.setLineDash((l.confidence < 0.8 || l.stale) ? [4 * s, 4 * s] : []);
      ctx.stroke(); ctx.setLineDash([]); ctx.globalAlpha = 1;
      if (this.selected && (l.a_device === this.selected || l.b_device === this.selected)) {
        ctx.beginPath(); ctx.moveTo(ax, ay); ctx.lineTo(bx, by); ctx.strokeStyle = this.C.accent; ctx.lineWidth = (width + 1.5) * s; ctx.globalAlpha = .5; ctx.stroke(); ctx.globalAlpha = 1;
      }
    });
    // nodes back to front
    const nodes = this.nodes.slice().sort((a, b) => proj[a.id][2] - proj[b.id][2]);
    const zoomLabel = this.zoom >= 1.3 || this.nodes.length <= 60;
    nodes.forEach(n => {
      const [x, y, , scale] = proj[n.id];
      const r = (SIZE[n.role] || 7) * s * Math.max(0.7, Math.min(1.6, this.zoom));
      let color = this.statusColor(n.status);
      if (n.external) color = this.C.unknown;
      if (this.opts.overlay === 'util' && n.data && n.data.cpu) {
        const c = n.data.cpu; color = c > 85 ? this.C.major : c > 60 ? this.C.minor : this.C.up;
      }
      ctx.fillStyle = color;
      ctx.globalAlpha = (n.status === 'unreachable' || n.external || (n.data && n.data.monitored === false)) ? 0.45 : 1;
      this._shape(ctx, SHAPE[n.role] || 'circle', x, y, r);
      ctx.fill();
      ctx.globalAlpha = 1;
      if (n.status === 'unreachable') { ctx.strokeStyle = this.C.unknown; ctx.lineWidth = 1 * s; ctx.setLineDash([2 * s, 2 * s]); this._shape(ctx, SHAPE[n.role] || 'circle', x, y, r + 2 * s); ctx.stroke(); ctx.setLineDash([]); }
      if (n.id === this.selected || n.id === this.hover) {
        ctx.strokeStyle = this.C.accent; ctx.lineWidth = 2 * s; this._shape(ctx, SHAPE[n.role] || 'circle', x, y, r + 4 * s); ctx.stroke();
      }
      if (n.alerts > 0 && n.status !== 'up') {
        ctx.fillStyle = this.C.crit; ctx.beginPath(); ctx.arc(x + r, y - r, 4 * s, 0, Math.PI * 2); ctx.fill();
      }
      if (zoomLabel || n.role === 'core' || n.role === 'distribution' || n.role === 'router' || n.role === 'firewall' || n.id === this.selected || n.id === this.hover) {
        ctx.fillStyle = this.C.text; ctx.font = `${11 * s}px ${this.C.mono}`; ctx.textAlign = 'center';
        const label = shortLabel(n.name); ctx.fillText(label, x, y - r - 6 * s);
      }
    });
    if (this.pulse) {
      const n = this.byId[this.pulse.id];
      if (n) {
        const [x, y] = proj[n.id];
        this.pulse.t -= 0.016;
        if (this.pulse.t <= 0) this.pulse = null;
        else {
          const rr = (12 + (0.7 - this.pulse.t) * 50) * s;
          ctx.beginPath(); ctx.ellipse(x, y, rr, rr * (this.opts.mode === '2d' ? 1 : 0.55), 0, 0, Math.PI * 2);
          ctx.strokeStyle = this.pulse.color; ctx.globalAlpha = this.pulse.t / 0.7; ctx.lineWidth = 2 * s; ctx.stroke(); ctx.globalAlpha = 1;
        }
      } else this.pulse = null;
    }
    this._proj = proj;
  };

  Topo.prototype._shape = function (ctx, shape, x, y, r) {
    ctx.beginPath();
    switch (shape) {
      case 'square': ctx.rect(x - r, y - r, r * 2, r * 2); break;
      case 'rect': ctx.rect(x - r * 1.3, y - r * 0.8, r * 2.6, r * 1.6); break;
      case 'hex': for (let i = 0; i < 6; i++) { const a = Math.PI / 3 * i - Math.PI / 6; ctx.lineTo(x + r * Math.cos(a), y + r * Math.sin(a)); } ctx.closePath(); break;
      case 'triangle': ctx.moveTo(x, y - r); ctx.lineTo(x + r, y + r); ctx.lineTo(x - r, y + r); ctx.closePath(); break;
      default: ctx.arc(x, y, r, 0, Math.PI * 2);
    }
  };

  Topo.prototype.nodeAt = function (clientX, clientY) {
    if (!this._proj) return null;
    const rect = this.canvas.getBoundingClientRect();
    const mx = (clientX - rect.left) * this.dpr, my = (clientY - rect.top) * this.dpr;
    let best = null, bestD = 16 * this.dpr;
    this.nodes.forEach(n => {
      const p = this._proj[n.id]; if (!p) return;
      const d = Math.hypot(p[0] - mx, p[1] - my);
      if (d < bestD) { best = n; bestD = d; }
    });
    return best;
  };

  Topo.prototype._bind = function () {
    const cv = this.canvas;
    let lx = 0, ly = 0, moved = false;
    cv.addEventListener('pointerdown', e => { this.drag = true; this.auto = false; moved = false; lx = e.clientX; ly = e.clientY; cv.setPointerCapture(e.pointerId); });
    cv.addEventListener('pointerup', e => {
      this.drag = false;
      if (!moved) { const n = this.nodeAt(e.clientX, e.clientY); this.selected = n ? n.id : null; if (this.opts.onSelect) this.opts.onSelect(n ? n.data : null); }
    });
    cv.addEventListener('pointermove', e => {
      if (this.drag) {
        const dx = e.clientX - lx, dy = e.clientY - ly;
        if (Math.abs(dx) + Math.abs(dy) > 2) moved = true;
        if (this.opts.mode === '3d' && !e.shiftKey) { this.theta -= dx * 0.006; this.phi = Math.max(0.25, Math.min(1.5, this.phi - dy * 0.006)); }
        else { this.panX += dx; this.panY += dy; }
        lx = e.clientX; ly = e.clientY;
      } else {
        const n = this.nodeAt(e.clientX, e.clientY);
        const id = n ? n.id : null;
        if (id !== this.hover) { this.hover = id; if (this.opts.onHover) this.opts.onHover(n ? n.data : null, e); }
        else if (n && this.opts.onHover) this.opts.onHover(n.data, e);
        cv.style.cursor = n ? 'pointer' : 'grab';
      }
    });
    cv.addEventListener('pointerleave', () => { this.hover = null; if (this.opts.onHover) this.opts.onHover(null); });
    cv.addEventListener('wheel', e => { e.preventDefault(); this.zoom = Math.max(0.4, Math.min(4, this.zoom * (e.deltaY > 0 ? 0.9 : 1.1))); }, { passive: false });
    cv.addEventListener('dblclick', () => { this.zoom = 1; this.panX = 0; this.panY = 0; this.theta = 0.55; this.phi = 1.05; });
  };

  window.TopoView = Topo;
})();
