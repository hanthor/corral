// Corral web UI — Proxmox-style dashboard for KubeVirt.
// Vanilla JS; noVNC + xterm.js vendored under static/vendor/ (offline-safe).

import { icon } from './icons.js';

const $ = (sel) => document.querySelector(sel);

let vms = [];
let cts = []; // Containers (#50) — pet pods, not KubeVirt VMs
let nodes = [];
let caps = { storageClass: '', canExpand: false, canSnapshot: false };
// Authenticated tailnet identity + privilege (see /api/whoami). Defaults to
// admin so the UI is fully enabled until told otherwise (single-user mode).
let me = { login: '', name: '', admin: true, enforced: false };
let availableNADs = [];
let selected = { type: 'dc' }; // {type:'dc'} | {type:'node',name} | {type:'vm',key}
let tab = 'summary';
let rfb = null;        // noVNC connection
let term = null;       // xterm instance
let ttyWS = null;      // serial console websocket

// ── API ───────────────────────────────────────────────────────────

async function api(path, opts = {}) {
  const r = await fetch(path, opts);
  if (!r.ok) {
    let msg = r.statusText;
    try { msg = (await r.json()).error || msg; } catch { /* not json */ }
    throw new Error(msg);
  }
  return r.headers.get('content-type')?.includes('json') ? r.json() : r.text();
}

function toast(msg) {
  document.querySelectorAll('.toast').forEach((t) => t.remove());
  const el = document.createElement('div');
  el.className = 'toast';
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 6000);
}

const esc = (s) => String(s ?? '').replace(/[&<>"']/g,
  (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const vmKey = (vm) => `${vm.peer || ''}/${vm.context || ''}/${vm.namespace}/${vm.name}`;
const vmURL = (vm, suffix = '') => `/api/vms/${vm.namespace}/${vm.name}${suffix}${vm.context ? `?context=${encodeURIComponent(vm.context)}` : ''}`;
const findVM = (key) => vms.find((v) => vmKey(v) === key);
const ctKey = (c) => `${c.namespace}/${c.name}`;
const findCT = (key) => cts.find((c) => ctKey(c) === key);

// Tag the tree/list is filtered to, or null for "show all".
let tagFilter = null;

// Add (on=true) or remove (on=false) a tag on a VM, then refresh.
async function setTag(vm, tag, on) {
  try { await post(vm, '/tags', { tag, on }); }
  catch (e) { toast(e.message); return; }
  setTimeout(() => refresh(), 500);
}

// Removable tag chips for a VM, plus an "+ add" affordance (bound by bindTags).
function tagChips(vm) {
  const tags = vm.tags || [];
  const chips = tags.map((t) =>
    `<span class="chip">${esc(t)}<button class="chip-x" data-untag="${esc(t)}" title="Remove tag">×</button></span>`).join('');
  return `${chips || '<span class="muted">none</span>'}
    <button class="btn sm" id="tag-add">${icon('plus')} Tag</button>`;
}

function bindTags(vm) {
  const el = $('#vm-tags');
  if (!el) return;
  el.querySelectorAll('[data-untag]').forEach((b) => {
    b.onclick = () => setTag(vm, b.dataset.untag, false);
  });
  const add = el.querySelector('#tag-add');
  if (add) add.onclick = () => {
    const t = prompt('Add tag (letters, digits, -_.):', '');
    if (t && t.trim()) setTag(vm, t.trim(), true);
  };
}

// Fingerprint of the last-rendered state. The 5s poll only re-renders when
// the data (or what's selected) actually changed — otherwise innerHTML
// replacement would reset scroll position and text selection on every tick.
let lastRenderFp = '';
// Whether the offline empty state is showing — rendered once, not on every
// failed 5s poll, so it doesn't clobber pages that work offline (Extensions).
let offlineShown = false;

// First-load failure page: no cluster reachable. The dashboard drives the
// KubeVirt backend, so tell the user how to connect one instead of showing
// a blank screen.
function renderOffline(msg) {
  $('#content').innerHTML = `
    <div class="empty-state">
      <div class="empty-icon">🤠</div>
      <h1>No cluster connected</h1>
      <p class="muted">Corral couldn't reach a Kubernetes cluster:
        <code>${esc(msg)}</code></p>
      <p>This dashboard drives the KubeVirt backend. To get going:</p>
      <ul>
        <li>Point <code>kubectl</code> at a cluster (<code>KUBECONFIG</code> or
          <code>~/.kube/config</code>), then hit Retry.</li>
        <li>Run <code>corral doctor --fix</code> to install anything the
          cluster is missing (KubeVirt, CDI, …).</li>
        <li>No cluster handy? Local QEMU VMs work from the CLI:
          <code>corral create myvm --image fedora</code></li>
      </ul>
      <button class="btn primary" id="offline-retry">Retry</button>
    </div>`;
  const b = $('#offline-retry');
  if (b) b.onclick = () => { offlineShown = false; refresh(true); };
}

async function refresh(force = false) {
  try {
    [vms, nodes] = await Promise.all([api('/api/vms'), api('/api/nodes')]);
  } catch (e) {
    // Never rendered anything yet → the cluster is unreachable on first load.
    // A blank page with a toast reads as "broken"; show setup guidance once
    // instead, and keep the static tree rows (Extensions works offline).
    if (!lastRenderFp && !offlineShown) {
      offlineShown = true;
      renderTree();
      renderOffline(e.message);
    } else if (lastRenderFp) {
      toast(`Refresh failed: ${e.message}`);
    }
    return;
  }
  offlineShown = false;
  try { cts = await api('/api/cts'); } catch { cts = []; } // best-effort — don't fail the whole refresh over CTs
  const fp = JSON.stringify([vms, cts, nodes, selected, tab]);
  if (!force && fp === lastRenderFp) return; // nothing changed — keep the DOM
  lastRenderFp = fp;

  // Re-render, preserving scroll positions across the DOM swap.
  const treeEl = $('#tree');
  const contentEl = $('#content');
  const treeScroll = treeEl ? treeEl.scrollTop : 0;
  const contentScroll = contentEl ? contentEl.scrollTop : 0;
  renderTree();
  // Don't clobber live consoles (or the multiview grid) on poll.
  if (tab !== 'console' && tab !== 'terminal' && selected.type !== 'multiview') renderContent();
  if (treeEl) treeEl.scrollTop = treeScroll;
  if (contentEl) contentEl.scrollTop = contentScroll;
}

async function loadCaps() {
  try { caps = await api('/api/capabilities'); } catch { /* keep defaults */ }
  // bootc/windows are optional plugins — hide their source options when absent.
  if (!caps.bootc) {
    document.querySelector('[name=sourceType] option[value=bootc]')?.remove();
  }
  await loadCreateContexts();
  // The Windows create flow is compiled into the web server (manifest gen in
  // pkg/kubevirt), so it's always available — no plugin install needed.
  try { availableNADs = await api('/api/nads'); } catch { availableNADs = []; }
}

async function loadCreateContexts() {
  const select = document.querySelector('#create-form [name=target]');
  if (!select) return;
  try {
    const result = await api('/api/contexts');
    const targets = result.contexts || [];
    select.innerHTML = targets.map((c) => `<option value="${esc(c.name)}" data-backend="${esc(c.backend)}" ${c.name === result.default ? 'selected' : ''}>${esc(c.name)} · ${esc(c.backend)}${c.context ? ` · ${esc(c.context)}` : ''}</option>`).join('');
  } catch {
    select.innerHTML = '<option value="cluster" data-backend="kubevirt">cluster · kubevirt</option>';
  }
  updateSourceFields();
}

// loadWhoami fetches the caller identity and flips the UI into read-only mode
// for non-admins (the server still enforces this; the UI just hides controls).
async function loadWhoami() {
  try { me = await api('/api/whoami'); } catch { return; }
  const el = $('#whoami');
  if (el) {
    const badge = me.admin ? '' : '<span class="ro-badge">read-only</span>';
    if (me.login) el.innerHTML = `<span class="who-name">${esc(me.name || me.login)}</span>${badge}`;
    else if (me.enforced) el.innerHTML = badge; // behind the gate but unidentified
    else el.textContent = '';
  }
  document.body.classList.toggle('read-only', !me.admin);
}

async function loadInstanceTypes() {
  let d;
  try { d = await api('/api/instancetypes'); } catch { return; }
  const fill = (sel, items, head) => {
    const el = document.querySelector(sel);
    if (!el) return;
    el.innerHTML = `<option value="">${head}</option>` +
      (items || []).map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
  };
  fill('[name=instancetype]', d.instancetypes, '— manual CPU/mem —');
  fill('[name=preference]', d.preferences, '(none)');
}

// ── Tree (sidebar) ────────────────────────────────────────────────
// Two views over the same VM list, mirroring PVE's Server/Folder view
// toggle (see docs/adr — namespace is the stable grouping axis for
// KubeVirt: unlike node, it doesn't change under live migration).

const TREE_VIEW_KEY = 'corral-tree-view';
let treeView = localStorage.getItem(TREE_VIEW_KEY) === 'folder' ? 'folder' : 'server';

function setTreeView(v) {
  treeView = v;
  localStorage.setItem(TREE_VIEW_KEY, v);
  renderTree();
}

function treeRow({ lvl, icon, label, sub, sel, onclick, dot }) {
  const div = document.createElement('div');
  div.className = `tree-item lvl-${lvl}${sel ? ' selected' : ''}`;
  div.innerHTML = `${dot ? `<span class="dot ${dot}"></span>` : ''}${icon} ${esc(label)}` +
    (sub ? ` <span class="muted">${esc(sub)}</span>` : '');
  div.onclick = () => { onclick(); closeDrawer(); };
  return div;
}

function treeViewToggle() {
  const div = document.createElement('div');
  div.className = 'tree-view-toggle';
  div.innerHTML = `
    <button type="button" class="btn sm${treeView === 'server' ? ' active' : ''}" data-view="server">Server View</button>
    <button type="button" class="btn sm${treeView === 'folder' ? ' active' : ''}" data-view="folder">Folder View</button>`;
  div.querySelectorAll('[data-view]').forEach((b) => {
    b.onclick = () => setTreeView(b.dataset.view);
  });
  return div;
}

function renderTree() {
  const tree = $('#tree');
  tree.replaceChildren();
  tree.appendChild(treeViewToggle());

  tree.appendChild(treeRow({
    lvl: 0, icon: icon('datacenter'), label: 'Datacenter',
    sel: selected.type === 'dc',
    onclick: () => select({ type: 'dc' }),
  }));

  tree.appendChild(treeRow({
    lvl: 0, icon: icon('health'), label: 'Cluster health',
    sel: selected.type === 'doctor',
    onclick: () => select({ type: 'doctor' }),
  }));

  tree.appendChild(treeRow({
    lvl: 0, icon: icon('extension'), label: 'Extensions',
    sel: selected.type === 'extensions',
    onclick: () => select({ type: 'extensions' }),
  }));

  tree.appendChild(treeRow({
    lvl: 0, icon: icon('cube'), label: 'Multiview',
    sub: 'live consoles',
    sel: selected.type === 'multiview',
    onclick: () => select({ type: 'multiview' }),
  }));

  tree.appendChild(treeRow({
    lvl: 0, icon: icon('cog'), label: 'Settings',
    sub: 'theme & branding',
    sel: selected.type === 'settings',
    onclick: () => select({ type: 'settings' }),
  }));

  if (treeView === 'folder') renderTreeFolders(tree);
  else renderTreeServer(tree);
}

// CTs sit in the same per-node/per-namespace groups as VMs, distinguished
// only by icon — matching real Proxmox, which puts VMs and CTs in one
// resource tree per node/pool rather than segregating them.
function ctRow(c, lvl) {
  return treeRow({
    lvl, icon: icon('container'), label: c.name,
    sub: c.namespace,
    dot: c.ready ? 'on' : c.phase === 'Stopped' ? 'off' : 'mid',
    sel: selected.type === 'ct' && selected.key === ctKey(c),
    onclick: () => select({ type: 'ct', key: ctKey(c) }),
  });
}

// Server View: Datacenter → Node → VMs/CTs, grouped by .node. Guests with
// no placed node (stopped, unscheduled) render as top-level orphans.
function renderTreeServer(tree) {
  const byNode = (nodeName) => vms.filter((v) => v.node === nodeName);
  const ctsByNode = (nodeName) => cts.filter((c) => c.node === nodeName);
  const placed = new Set();
  const ctPlaced = new Set();

  for (const n of nodes) {
    tree.appendChild(treeRow({
      lvl: 1, icon: icon('server'), label: n.name, sub: n.roles,
      dot: n.ready ? 'on' : 'off',
      sel: selected.type === 'node' && selected.name === n.name,
      onclick: () => select({ type: 'node', name: n.name }),
    }));
    for (const vm of byNode(n.name)) {
      placed.add(vmKey(vm));
      tree.appendChild(vmRow(vm, 2));
    }
    for (const c of ctsByNode(n.name)) {
      ctPlaced.add(ctKey(c));
      tree.appendChild(ctRow(c, 2));
    }
  }

  const orphans = vms.filter((v) => !placed.has(vmKey(v)));
  for (const vm of orphans) tree.appendChild(vmRow(vm, 1));
  const ctOrphans = cts.filter((c) => !ctPlaced.has(ctKey(c)));
  for (const c of ctOrphans) tree.appendChild(ctRow(c, 1));
}

// Folder View: Datacenter → Namespace → VMs/CTs (templates included — they're
// still VMs in their namespace, just labeled differently by vmRow). Namespace
// is stable across live migration, unlike node.
function renderTreeFolders(tree) {
  const byNS = new Map();
  for (const vm of vms) {
    const ns = vm.namespace || '(none)';
    if (!byNS.has(ns)) byNS.set(ns, { vms: [], cts: [] });
    byNS.get(ns).vms.push(vm);
  }
  for (const c of cts) {
    const ns = c.namespace || '(none)';
    if (!byNS.has(ns)) byNS.set(ns, { vms: [], cts: [] });
    byNS.get(ns).cts.push(c);
  }
  const namespaces = [...byNS.keys()].sort();

  for (const ns of namespaces) {
    const { vms: nsVMs, cts: nsCTs } = byNS.get(ns);
    const parts = [];
    if (nsVMs.length) parts.push(`${nsVMs.length} VM${nsVMs.length === 1 ? '' : 's'}`);
    if (nsCTs.length) parts.push(`${nsCTs.length} CT${nsCTs.length === 1 ? '' : 's'}`);
    tree.appendChild(treeRow({
      lvl: 1, icon: icon('folder'), label: ns, sub: parts.join(', '),
      sel: selected.type === 'namespace' && selected.name === ns,
      onclick: () => select({ type: 'namespace', name: ns }),
    }));
    for (const vm of nsVMs) tree.appendChild(vmRow(vm, 2));
    for (const c of nsCTs) tree.appendChild(ctRow(c, 2));
  }
}

function vmRow(vm, lvl) {
  return treeRow({
    lvl, icon: icon(vm.isTemplate ? 'template' : 'cube'), label: vm.name,
    sub: vm.isTemplate ? 'template' : vm.namespace,
    dot: vm.ready ? 'on' : (vm.running ? 'mid' : 'off'),
    sel: selected.type === 'vm' && selected.key === vmKey(vm),
    onclick: () => select({ type: 'vm', key: vmKey(vm) }),
  });
}

// markRendered records the just-rendered state so the next poll tick doesn't
// re-render (and reset scroll) for a change the user already saw.
function markRendered() {
  lastRenderFp = JSON.stringify([vms, cts, nodes, selected, tab]);
}

function select(sel) {
  disconnectConsoles();
  if (selected.type === 'multiview' && sel.type !== 'multiview') disconnectMultiview();
  selected = sel;
  tab = 'summary';
  renderTree();
  renderContent();
  markRendered();
}

// ── Content panel ─────────────────────────────────────────────────

function renderContent() {
  const main = $('#content');
  if (selected.type === 'vm') {
    const vm = findVM(selected.key);
    if (!vm) { selected = { type: 'dc' }; }
    else return renderVM(main, vm);
  }
  if (selected.type === 'ct') {
    const c = findCT(selected.key);
    if (!c) { selected = { type: 'dc' }; }
    else return renderCT(main, c);
  }
  if (selected.type === 'node') return renderNode(main, selected.name);
  if (selected.type === 'namespace') return renderNamespace(main, selected.name);
  if (selected.type === 'extensions') return renderExtensions(main);
  if (selected.type === 'doctor') return renderDoctor(main);
  if (selected.type === 'multiview') return renderMultiview(main);
  if (selected.type === 'settings') return renderSettings(main);
  return renderDatacenter(main);
}

// ── Multiview: a grid of live view-only consoles ──────────────────
// Watch 4–6 VMs at once — built for monitoring automated GUI testing.

let multiviewRFBs = [];

function disconnectMultiview() {
  for (const r of multiviewRFBs) { try { r.disconnect(); } catch { /* gone */ } }
  multiviewRFBs = [];
}

async function renderMultiview(main) {
  disconnectMultiview();
  const running = vms.filter((v) => v.running);
  const shown = running.slice(0, 6);
  main.innerHTML = `
    <div class="page-head"><h1>${icon('cube')} Multiview</h1>
      <span class="muted">${running.length} running VM${running.length === 1 ? '' : 's'}${running.length > 6 ? ' — showing first 6' : ''}</span>
    </div>
    ${shown.length ? `<div id="mv-grid" class="mv-grid" data-count="${shown.length}"></div>`
      : `<p class="console-msg">No running VMs. Start some and they appear here, live.</p>`}`;
  if (!shown.length) return;

  let RFB;
  try {
    ({ default: RFB } = await import(
      './vendor/novnc-rfb.esm.js'));
  } catch (e) {
    $('#mv-grid').innerHTML = `<p class="console-msg">noVNC failed to load: ${esc(e.message)}</p>`;
    return;
  }
  const grid = $('#mv-grid');
  for (const vm of shown) {
    const tile = document.createElement('div');
    tile.className = 'mv-tile';
    tile.innerHTML = `<div class="mv-title">${esc(vm.name)} <span class="muted">${esc(vm.namespace)}</span></div>
      <div class="mv-screen"></div>`;
    tile.querySelector('.mv-title').onclick = () => {
      disconnectMultiview();
      select({ type: 'vm', key: vmKey(vm) });
      tab = 'console';
      renderContent();
    };
    grid.appendChild(tile);
    try {
      const rfb = new RFB(tile.querySelector('.mv-screen'), wsURL('vnc', vm));
      rfb.viewOnly = false; // click tile canvas to focus and interact directly
      rfb.scaleViewport = true;
      tile.onclick = (e) => {
        if (!e.target.closest('.mv-title')) {
          rfb.focus();
        }
      };
      rfb.addEventListener('disconnect', () => {
        tile.querySelector('.mv-screen').innerHTML = `<p class="console-msg">disconnected</p>`;
      });
      multiviewRFBs.push(rfb);
    } catch {
      tile.querySelector('.mv-screen').innerHTML = `<p class="console-msg">connect failed</p>`;
    }
  }
}

async function renderDoctor(main) {
  main.innerHTML = `<div class="page-head"><h1>${icon('health')} Cluster health</h1>
    <button class="btn primary" id="doc-fix" hidden>Reconcile fixable</button></div>
    <p class="muted" style="margin-bottom:14px">What Corral's features need from the cluster.
      Fixable items are safe, config-only changes Corral can apply.</p>
    <div id="doc-list"><p class="muted">checking…</p></div>`;
  let checks;
  try { checks = await api('/api/doctor'); }
  catch (e) { $('#doc-list').innerHTML = `<p class="console-msg">${esc(e.message)}</p>`; return; }
  const fixBtn = $('#doc-fix');
  fixBtn.hidden = !checks.some((c) => !c.ok && c.fixable);
  fixBtn.onclick = async () => {
    fixBtn.disabled = true; fixBtn.textContent = 'Reconciling…';
    try { await api('/api/doctor/fix', { method: 'POST' }); toast('Reconciled'); }
    catch (e) { toast(e.message); }
    renderDoctor(main);
  };
  $('#doc-list').innerHTML = `<table><tbody>
    ${checks.map((c) => `<tr>
      <td style="width:1.5rem">${c.ok ? '<span class="dot on"></span>' : '<span class="dot off"></span>'}</td>
      <td><strong ${c.ok ? '' : 'class="doc-broken"'}>${esc(c.name)}</strong></td>
      <td class="${c.ok ? 'muted' : 'doc-broken'}">${esc(c.detail)}</td>
      <td>${!c.ok && c.fixable ? `<button class="btn sm" data-fix="${esc(c.name)}">Fix</button>` : ''}</td>
    </tr>`).join('')}
  </tbody></table>`;
  // Per-check fix buttons — scoped reconcile of just that item.
  $('#doc-list').querySelectorAll('[data-fix]').forEach((b) => {
    b.onclick = async () => {
      b.disabled = true; b.textContent = 'Fixing…';
      try {
        await api('/api/doctor/fix', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ check: b.dataset.fix }),
        });
        toast(`Fixed: ${b.dataset.fix}`);
      } catch (e) { toast(e.message); }
      renderDoctor(main);
    };
  });
}

async function renderExtensions(main) {
  main.innerHTML = `<div class="page-head"><h1>${icon('extension')} Extensions</h1></div>
    <p class="muted" style="margin-bottom:14px">Optional plugins from the Corral marketplace.
      Installed plugins add <code>corral &lt;name&gt;</code> commands.</p>
    <div id="ext-list"><p class="muted">loading…</p></div>`;
  let list;
  try { list = await api('/api/plugins'); }
  catch (e) { $('#ext-list').innerHTML = `<p class="console-msg">${esc(e.message)}</p>`; return; }
  if (!list.length) { $('#ext-list').innerHTML = `<p class="muted">No extensions available.</p>`; return; }
  $('#ext-list').innerHTML = `<div class="ext-grid">${list.map((p) => `
    <div class="ext-card">
      <div class="ext-head">${icon('extension')} <strong>${esc(p.name)}</strong>
        <span class="muted">${esc(p.version || '')}</span>
        ${p.installed ? '<span class="pill on">installed</span>' : ''}</div>
      <div class="ext-desc">${esc(p.description || '')}</div>
      <div class="muted">${esc(p.source || '')}${p.publisher ? ` · ${esc(p.publisher)}` : ''}${p.license ? ` · ${esc(p.license)}` : ''}</div>
      ${p.supportedBackends?.length ? `<div class="muted">Backends: ${p.supportedBackends.map(esc).join(', ')}</div>` : '<div class="muted">Backends: not declared</div>'}
      ${p.permissions?.length ? `<div class="muted">Permissions: ${p.permissions.map(esc).join(', ')}</div>` : ''}
      <div class="ext-actions">
        ${p.installed
          ? `<button class="btn sm danger" data-ext-rm="${esc(p.name)}">Remove</button>`
          : (p.inStore ? `<button class="btn sm primary" data-ext-add="${esc(p.name)}" data-source="${esc(p.source || '')}" data-permissions="${esc((p.permissions || []).join(', '))}">Install</button>` : '')}
        ${p.homepage ? `<a class="btn sm" href="${esc(p.homepage)}" target="_blank" rel="noopener">Homepage</a>` : ''}
      </div>
    </div>`).join('')}</div>`;
  main.querySelectorAll('[data-ext-add]').forEach((b) => {
    b.onclick = async () => {
      const permissions = b.dataset.permissions;
      if (permissions && !confirm(`Install ${b.dataset.extAdd} and grant its declared permissions?\n\n${permissions}`)) return;
      b.disabled = true; b.textContent = 'Installing…';
      const source = b.dataset.source ? `?source=${encodeURIComponent(b.dataset.source)}` : '';
      try { await api(`/api/plugins/${b.dataset.extAdd}/install${source}`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ acceptPermissions: !!permissions }),
      }); toast('Installed'); }
      catch (e) { toast(e.message); }
      renderExtensions(main);
    };
  });
  main.querySelectorAll('[data-ext-rm]').forEach((b) => {
    b.onclick = async () => {
      try { await api(`/api/plugins/${b.dataset.extRm}`, { method: 'DELETE' }); toast('Removed'); }
      catch (e) { toast(e.message); }
      renderExtensions(main);
    };
  });
}

// ── Settings: theme & branding ────────────────────────────────────
// Live-preview settings form: changes apply to the page immediately;
// Save persists to config.yaml via PUT /api/theme.

let themePreview = null; // <style> element for live preview

async function renderSettings(main) {
  let theme;
  try { theme = await api('/api/theme'); }
  catch (e) { main.innerHTML = `<p class="console-msg">${esc(e.message)}</p>`; return; }

  const presets = [
    ['#f0883e', 'Orange'],
    ['#3b82f6', 'Blue'],
    ['#22c55e', 'Green'],
    ['#a855f7', 'Purple'],
    ['#ef4444', 'Red'],
    ['#f59e0b', 'Amber'],
  ];

  main.innerHTML = `
    <div class="page-head"><h1>${icon('cog')} Settings</h1></div>

    <h2 class="section">${icon('cpu')} Accent colour</h2>
    <div class="hw-edit" style="align-items:center">
      <label>Colour <input type="color" id="set-accent" value="${esc(theme.accent)}"></label>
      <div style="display:flex;gap:8px;flex-wrap:wrap">
        ${presets.map(([hex, label]) =>
          `<button class="btn sm preset" data-hex="${esc(hex)}" style="${hex === theme.accent ? 'border-color:' + esc(hex) + ';outline:1px solid ' + esc(hex) : ''}">${esc(label)}</button>`).join('')}
      </div>
    </div>

    <h2 class="section">Branding</h2>
    <div class="hw-edit">
      <label>Title <input id="set-brand-title" value="${esc(theme.brand_title)}" placeholder="Corral"></label>
      <label>Emoji <input id="set-brand-emoji" value="${esc(theme.brand_emoji)}" placeholder="🤠" maxlength="4" style="width:80px"></label>
      <label>Subtitle <input id="set-brand-subtitle" value="${esc(theme.brand_subtitle)}" placeholder="Virtual Environment" style="width:200px"></label>
    </div>

    <h2 class="section">Custom CSS</h2>
    <textarea id="set-css" rows="10" style="width:100%;background:var(--bg);color:var(--text);border:1px solid var(--border);border-radius:var(--radius);padding:10px;font-family:monospace;font-size:.85rem" placeholder="/* Custom CSS injected into every page */">${esc(theme.custom_css)}</textarea>

    <div class="dialog-actions" style="justify-content:flex-start;margin-top:16px">
      <button class="btn primary" id="set-save">${icon('download')} Save</button>
      <button class="btn" id="set-reset-css" ${theme.custom_css ? '' : 'disabled'}>Reset CSS</button>
      <span class="muted" style="margin-left:12px;font-size:.82rem" id="set-status"></span>
    </div>`;

  // Live preview: update CSS custom properties and branding on every change.
  const accent = $('#set-accent');
  const title = $('#set-brand-title');
  const emoji = $('#set-brand-emoji');
  const subtitle = $('#set-brand-subtitle');
  const css = $('#set-css');

  const updatePreview = () => {
    document.documentElement.style.setProperty('--accent', accent.value);
    // Accent-2: slightly darkened variant for hover states.
    const hex = accent.value.replace('#', '');
    if (hex.length === 6) {
      const dim = (c) => {
        let v = parseInt(c, 16);
        v = Math.round(v * 0.875);
        return v.toString(16).padStart(2, '0');
      };
      document.documentElement.style.setProperty('--accent-2', '#' + dim(hex.slice(0, 2)) + dim(hex.slice(2, 4)) + dim(hex.slice(4, 6)));
    }
    // Branding.
    const brand = document.querySelector('.brand');
    if (brand) {
      brand.innerHTML = `${emoji.value ? emoji.value + ' ' : ''}<strong>${esc(title.value || 'Corral')}</strong> <span>${esc(subtitle.value || '')}</span>`;
    }
    // Custom CSS preview.
    if (!themePreview) {
      themePreview = document.createElement('style');
      themePreview.id = 'corral-theme-preview';
      document.head.appendChild(themePreview);
    }
    themePreview.textContent = css.value;
  };

  // Preset swatches.
  main.querySelectorAll('.preset').forEach((b) => {
    b.onclick = () => {
      accent.value = b.dataset.hex;
      updatePreview();
      // Highlight the active preset.
      main.querySelectorAll('.preset').forEach((p) => p.style.outline = '');
      b.style.outline = `1px solid ${b.dataset.hex}`;
      b.style.borderColor = b.dataset.hex;
    };
  });

  accent.oninput = updatePreview;
  title.oninput = updatePreview;
  emoji.oninput = updatePreview;
  subtitle.oninput = updatePreview;
  css.oninput = updatePreview;

  // Save.
  $('#set-save').onclick = async () => {
    const btn = $('#set-save');
    const st = $('#set-status');
    btn.disabled = true;
    st.textContent = 'Saving…';
    try {
      await api('/api/theme', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          accent: accent.value,
          brand_title: title.value,
          brand_emoji: emoji.value,
          brand_subtitle: subtitle.value,
          custom_css: css.value,
        }),
      });
      st.textContent = '✓ Saved';
      st.style.color = 'var(--green)';
      // Remove the preview style — the server will inject the real one on reload.
      if (themePreview) { themePreview.remove(); themePreview = null; }
    } catch (e) {
      st.textContent = `✗ ${e.message}`;
      st.style.color = 'var(--red)';
    }
    btn.disabled = false;
    setTimeout(() => { st.textContent = ''; st.style.color = ''; }, 4000);
  };

  // Reset custom CSS.
  $('#set-reset-css').onclick = () => {
    css.value = '';
    updatePreview();
  };
  css.oninput = () => {
    $('#set-reset-css').disabled = !css.value.trim();
  };

  // Apply the current theme's custom CSS as live preview too.
  updatePreview();
}

function renderDatacenter(main) {
  const running = vms.filter((v) => v.ready).length;
  const ready = nodes.filter((n) => n.ready).length;
  const allTags = [...new Set(vms.flatMap((v) => v.tags || []))].sort();
  if (tagFilter && !allTags.includes(tagFilter)) tagFilter = null; // tag vanished
  const shown = tagFilter ? vms.filter((v) => (v.tags || []).includes(tagFilter)) : vms;
  main.innerHTML = `
    <div class="page-head"><h1>Datacenter</h1></div>
    <div class="cards">
      <div class="card"><div class="num">${vms.length}</div><div class="label">virtual machines</div></div>
      <div class="card"><div class="num">${running}</div><div class="label">running</div></div>
      <div class="card"><div class="num">${ready}/${nodes.length}</div><div class="label">nodes ready</div></div>
    </div>
    ${allTags.length ? `<div class="tagbar">
      <span class="muted">Filter by tag:</span>
      <button class="chip filter ${tagFilter ? '' : 'active'}" data-tagfilter="">all</button>
      ${allTags.map((t) => `<button class="chip filter ${tagFilter === t ? 'active' : ''}" data-tagfilter="${esc(t)}">${esc(t)}</button>`).join('')}
    </div>` : ''}
    ${vmTable(shown)}
    <h2 class="section">${icon('disk')} Image library
      <button class="btn" id="dc-import">${icon('download')} Import image</button>
      <button class="btn" id="dc-upload">${icon('download')} Upload ISO</button>
      <input type="file" id="dc-upload-file" accept=".iso,.img,.qcow2,.raw" hidden>
    </h2>
    <div id="dc-images"><p class="muted">loading…</p></div>
    <h2 class="section">${icon('template')} Templates</h2>
    ${templateTable(vms.filter((v) => v.isTemplate))}`;
  bindVMTable(main);
  bindTemplateTable(main);
  main.querySelectorAll('[data-tagfilter]').forEach((b) => {
    b.onclick = () => { tagFilter = b.dataset.tagfilter || null; renderDatacenter(main); markRendered(); };
  });
  $('#dc-import').onclick = importImage;
  $('#dc-upload').onclick = () => $('#dc-upload-file').click();
  $('#dc-upload-file').onchange = (e) => {
    const file = e.target.files[0];
    e.target.value = ''; // allow re-selecting the same file next time
    if (file) uploadImage(file);
  };
  loadImages();
}

async function loadImages() {
  const el = $('#dc-images');
  if (!el) return;
  let dvs;
  try { dvs = await api('/api/datavolumes'); }
  catch (e) { el.innerHTML = `<p class="muted">${esc(e.message)}</p>`; return; }
  if (!dvs.length) { el.innerHTML = `<p class="muted">No imported images. Use “Import image” for an ISO/qcow2 URL.</p>`; return; }
  el.innerHTML = `<table><thead><tr>
      <th>Name</th><th>Namespace</th><th>Size</th><th>Status</th><th>Source</th><th></th>
    </tr></thead><tbody>
    ${dvs.map((d) => `<tr>
      <td>${esc(d.name)}</td><td>${esc(d.namespace)}</td><td>${esc(d.size || '—')}</td>
      <td>${esc(d.phase || '—')}${d.progress && d.phase !== 'Succeeded' ? ` (${esc(d.progress)})` : ''}</td>
      <td class="muted" style="max-width:280px;overflow:hidden;text-overflow:ellipsis">${esc(d.source || '')}</td>
      <td><button class="btn sm danger" data-deldv="${esc(d.namespace)}/${esc(d.name)}">${icon('trash')}</button></td>
    </tr>`).join('')}
    </tbody></table>`;
  el.querySelectorAll('[data-deldv]').forEach((b) => {
    b.onclick = async () => {
      const [ns, name] = b.dataset.deldv.split('/');
      if (!confirm(`Delete image ${name}?`)) return;
      try { await api(`/api/datavolumes/${ns}/${name}`, { method: 'DELETE' }); toast('Deleted'); }
      catch (e) { toast(e.message); }
      setTimeout(loadImages, 500);
    };
  });
}

async function importImage() {
  const url = prompt('Image URL (ISO / qcow2 / raw, http[s]):', '');
  if (!url) return;
  const guess = (url.split('/').pop() || 'image').replace(/[^a-z0-9-]/gi, '-').toLowerCase().slice(0, 40);
  const name = prompt('Name for this image:', guess);
  if (!name) return;
  const size = prompt('Disk size for the import (e.g. 10Gi):', '10Gi') || '10Gi';
  try {
    await api('/api/datavolumes', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, namespace: '', url, size }),
    });
    toast('Import started');
    setTimeout(loadImages, 800);
  } catch (e) { toast(e.message); }
}

// Uploads a local file straight to a new DataVolume via the CDI upload
// proxy (server-side: pkg/kubevirt.UploadDataVolume, which shells out to
// `virtctl image-upload` rather than reimplementing CDI's upload protocol).
async function uploadImage(file) {
  const guess = file.name.replace(/\.[^.]+$/, '').replace(/[^a-z0-9-]/gi, '-').toLowerCase().slice(0, 40) || 'image';
  const name = prompt('Name for this image:', guess);
  if (!name) return;
  const sizeGuess = Math.ceil(file.size / (1024 ** 3)) + 1; // pad a bit over the raw file size
  const size = prompt('DataVolume size (e.g. 10Gi) — must fit the uploaded file:', `${sizeGuess}Gi`) || `${sizeGuess}Gi`;

  const qs = new URLSearchParams({ name, namespace: '', size });
  let res;
  try {
    res = await api(`/api/datavolumes/upload?${qs}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/octet-stream' },
      body: file,
    });
  } catch (e) { toast(`Upload failed: ${e.message}`); return; }

  if (res.task) {
    watchBuild(res.task, name, {
      titleRun: `Uploading ${file.name}…`,
      titleDone: `✅ ${name} uploaded`,
      titleFail: `❌ Upload failed`,
      onDone: loadImages,
    });
  }
}

function renderNode(main, name) {
  const n = nodes.find((x) => x.name === name);
  const nodeVMs = vms.filter((v) => v.node === name);
  const nodeCTs = cts.filter((c) => c.node === name);
  main.innerHTML = `
    <div class="page-head">
      <h1>${icon('server')} ${esc(name)}</h1>
      <span class="pill ${n?.ready ? 'on' : 'off'}">${n?.ready ? 'ready' : 'not ready'}</span>
    </div>
    <dl class="props">
      <dt>Roles</dt><dd>${esc(n?.roles || '—')}</dd>
      <dt>Kubelet</dt><dd>${esc(n?.kubelet || '—')}</dd>
      <dt>Architecture</dt><dd>${esc(n?.arch || '—')}</dd>
      <dt>VMs</dt><dd>${nodeVMs.length}</dd>
      <dt>CTs</dt><dd>${nodeCTs.length}</dd>
    </dl>
    <h2 style="font-size:1rem;margin:18px 0 8px">Virtual machines</h2>
    ${vmTable(nodeVMs)}
    ${nodeCTs.length ? `<h2 style="font-size:1rem;margin:18px 0 8px">Containers</h2>${ctTable(nodeCTs)}` : ''}`;
  bindVMTable(main);
  bindCTTable(main);
}

// Folder View's namespace detail — same shape as renderNode, grouped by
// namespace instead of node.
function renderNamespace(main, name) {
  const nsVMs = vms.filter((v) => (v.namespace || '(none)') === name);
  const nsCTs = cts.filter((c) => (c.namespace || '(none)') === name);
  main.innerHTML = `
    <div class="page-head">
      <h1>${icon('folder')} ${esc(name)}</h1>
    </div>
    <dl class="props">
      <dt>VMs</dt><dd>${nsVMs.length}</dd>
      <dt>CTs</dt><dd>${nsCTs.length}</dd>
    </dl>
    <h2 style="font-size:1rem;margin:18px 0 8px">Virtual machines</h2>
    ${vmTable(nsVMs)}
    ${nsCTs.length ? `<h2 style="font-size:1rem;margin:18px 0 8px">Containers</h2>${ctTable(nsCTs)}` : ''}`;
  bindVMTable(main);
  bindCTTable(main);
}

function ctTable(list) {
  if (!list.length) return '';
  return `<table><thead><tr>
      <th>Name</th><th>Status</th><th>Node</th><th>Namespace</th><th>CPU</th><th>Mem</th><th>Privileged</th>
    </tr></thead><tbody>
    ${list.map((c) => `<tr data-ctkey="${esc(ctKey(c))}">
      <td>${esc(c.name)}</td>
      <td><span class="dot ${c.ready ? 'on' : c.phase === 'Stopped' ? 'off' : 'mid'}"></span> ${esc(c.phase)}</td>
      <td>${esc(c.node || '—')}</td><td>${esc(c.namespace)}</td>
      <td>${c.cpu || '—'}</td><td>${esc(c.mem || '—')}</td><td>${c.privileged ? 'yes' : 'no'}</td>
    </tr>`).join('')}
    </tbody></table>`;
}

function bindCTTable(root) {
  root.querySelectorAll('tr[data-ctkey]').forEach((tr) => {
    tr.onclick = () => select({ type: 'ct', key: tr.dataset.ctkey });
  });
}

// Container (CT) detail view (#50) — simpler than a VM's: no VNC/hardware/
// snapshots, just Summary + Terminal (exec, not a serial console — see
// ttyBridge's VM-vs-CT dispatch) and start/stop/delete.
const CT_TABS = [['summary', 'Summary'], ['hardware', 'Hardware'], ['terminal', 'Terminal']];
let ctTab = 'summary';

function renderCT(main, c) {
  const running = c.phase === 'Running';
  main.innerHTML = `
    <div class="page-head">
      <h1>${icon('cube')} ${esc(c.name)}</h1>
      <span class="pill ${c.ready ? 'on' : 'off'}">${esc(c.phase)}</span>
      <div class="toolbar">
        <button class="btn" data-ctact="start" ${running ? 'disabled' : ''}>${icon('play')} Start</button>
        <button class="btn" data-ctact="stop" ${running ? '' : 'disabled'}>${icon('stop')} Stop</button>
        <button class="btn danger" data-ctact="delete">${icon('trash')} Delete</button>
      </div>
    </div>
    <div class="tabs">
      ${CT_TABS.map(([id, label]) =>
        `<div class="tab ${ctTab === id ? 'active' : ''}" data-cttab="${id}">${label}</div>`).join('')}
    </div>
    <div id="ct-tab-body"></div>`;

  main.querySelectorAll('[data-ctact]').forEach((b) => {
    b.onclick = () => ctAction(c, b.dataset.ctact);
  });
  main.querySelectorAll('[data-cttab]').forEach((t) => {
    t.onclick = () => { disconnectConsoles(); ctTab = t.dataset.cttab; renderCT(main, c); markRendered(); };
  });

  const body = $('#ct-tab-body');
  if (ctTab === 'summary') {
    body.innerHTML = `<dl class="props">
      <dt>Status</dt><dd>${esc(c.phase)}</dd>
      <dt>Namespace</dt><dd>${esc(c.namespace)}</dd>
      <dt>Image</dt><dd><code>${esc(c.image || '—')}</code></dd>
      <dt>vCPUs</dt><dd>${c.cpu || '—'}</dd>
      <dt>Memory</dt><dd>${esc(c.mem || '—')}</dd>
      <dt>Privileged</dt><dd>${c.privileged ? 'yes' : 'no'}</dd>
      <dt>Storage</dt><dd>${c.privileged
        ? `<code>/</code> — persistent full rootfs (${esc(c.name)}-data PVC, distrobox-style: package installs and dotfiles survive Stop/Start)`
        : `<code>/data</code> only (${esc(c.name)}-data PVC) — everything else resets on Stop/Start`}</dd>
    </dl>`;
  } else if (ctTab === 'terminal') {
    connectTTY({ namespace: c.namespace, name: c.name, running }, body);
  } else if (ctTab === 'hardware') {
    body.innerHTML = `<div class="hw-edit">
      <label>vCPUs <input id="ct-hw-cpu" type="number" value="${c.cpu || 1}" min="1"></label>
      <label>Memory <input id="ct-hw-mem" value="${esc(c.mem || '512Mi')}"></label>
      <button class="btn primary" id="ct-hw-apply">Apply</button>
    </div>`;
    $('#ct-hw-apply').onclick = async () => {
      const cpu = parseInt($('#ct-hw-cpu').value, 10) || 0;
      const mem = $('#ct-hw-mem').value.trim();
      try {
        await api(`/api/cts/${c.namespace}/${c.name}/scale`, {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ cpu, mem }),
        });
        toast('Scaled — stop/start to apply if the pod rejects in-place resize');
        refresh(true);
      } catch (e) { toast(e.message); }
    };
  }
}

async function ctAction(c, act) {
  if (act === 'delete') {
    if (!confirm(`Delete container ${c.name}? This removes its data volume too.`)) return;
    try {
      await api(`/api/cts/${c.namespace}/${c.name}`, { method: 'DELETE' });
      toast('Deleted');
      select({ type: 'dc' });
    } catch (e) { toast(e.message); }
    return refresh(true);
  }
  try {
    await api(`/api/cts/${c.namespace}/${c.name}/${act}`, { method: 'POST' });
    toast(act === 'start' ? 'Starting…' : 'Stopped');
  } catch (e) { toast(e.message); }
  setTimeout(() => refresh(true), 600);
}

function vmTable(list) {
  if (!list.length) return `<p class="console-msg">No virtual machines.</p>`;
  return `<div class="bulkbar" hidden>
      <span class="bulkbar-count">0 selected</span>
      <button class="btn sm" data-bulk="start">${icon('play')} Start</button>
      <button class="btn sm" data-bulk="stop">${icon('stop')} Stop</button>
      <button class="btn sm" data-bulk="restart">${icon('restart')} Restart</button>
    </div>
    <table><thead><tr>
      <th class="check"><input type="checkbox" class="vm-check-all" title="Select all"></th>
      <th>Name</th><th>Status</th><th>Node</th><th>Namespace</th><th>CPU</th><th>Mem</th><th>IP</th>
    </tr></thead><tbody>
    ${list.map((v) => `<tr data-key="${esc(vmKey(v))}">
      <td class="check"><input type="checkbox" class="vm-check" value="${esc(vmKey(v))}"></td>
      <td>${esc(v.name)}${(v.tags || []).map((t) => `<span class="chip mini">${esc(t)}</span>`).join('')}</td>
      <td><span class="dot ${v.ready ? 'on' : v.running ? 'mid' : 'off'}"></span> ${esc(v.status)}</td>
      <td>${esc(v.node || '—')}</td><td>${esc(v.namespace)}</td>
      <td>${v.cpu}</td><td>${esc(v.mem)}</td><td>${esc(v.ip || '—')}</td>
    </tr>`).join('')}
    </tbody></table>`;
}

function bindVMTable(root) {
  // Row click opens the VM — except clicks landing in the checkbox cell.
  root.querySelectorAll('tr[data-key]').forEach((tr) => {
    tr.onclick = (e) => {
      if (e.target.closest('.check')) return;
      select({ type: 'vm', key: tr.dataset.key });
    };
  });

  const checks = [...root.querySelectorAll('.vm-check')];
  const all = root.querySelector('.vm-check-all');
  const bar = root.querySelector('.bulkbar');
  if (!checks.length || !bar) return;

  const selectedKeys = () => checks.filter((c) => c.checked).map((c) => c.value);
  const update = () => {
    const n = selectedKeys().length;
    bar.hidden = n === 0;
    bar.querySelector('.bulkbar-count').textContent = `${n} selected`;
    if (all) {
      all.checked = n > 0 && n === checks.length;
      all.indeterminate = n > 0 && n < checks.length;
    }
  };

  checks.forEach((c) => {
    c.onclick = (e) => e.stopPropagation();
    c.onchange = update;
  });
  if (all) {
    all.onclick = (e) => e.stopPropagation();
    all.onchange = () => { checks.forEach((c) => { c.checked = all.checked; }); update(); };
  }

  bar.querySelectorAll('[data-bulk]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      const act = b.dataset.bulk;
      const sel = selectedKeys().map(findVM).filter(Boolean);
      if (!sel.length) return;
      const verb = act === 'start' ? 'Start' : act === 'stop' ? 'Stop' : 'Restart';
      if (!confirm(`${verb} ${sel.length} VM${sel.length === 1 ? '' : 's'}?`)) return;
      let ok = 0;
      let fail = 0;
      await Promise.all(sel.map(async (vm) => {
        try { await api(vmURL(vm, `/${act}`), { method: 'POST' }); ok += 1; }
        catch { fail += 1; }
      }));
      toast(`${verb}: ${ok} ok${fail ? `, ${fail} failed` : ''}`);
      setTimeout(() => refresh(), 800);
    };
  });

  update();
}

// Templates section of the Datacenter/library view (#49) — VMs already
// marked via the "Make template" action (POST .../template) surfaced as a
// managed list, per the issue's "surface those templates here" ask. Reuses
// the same mark-template endpoint to unmark/remove from here.
function templateTable(list) {
  if (!list.length) return `<p class="muted">No templates. Mark a VM as a template from its detail page.</p>`;
  return `<table><thead><tr><th>Name</th><th>Namespace</th><th>CPU</th><th>Mem</th><th></th></tr></thead><tbody>
    ${list.map((v) => `<tr data-key="${esc(vmKey(v))}">
      <td>${esc(v.name)}</td><td>${esc(v.namespace)}</td><td>${v.cpu}</td><td>${esc(v.mem)}</td>
      <td><button class="btn sm danger" data-untemplate="${esc(vmKey(v))}">Unmark</button></td>
    </tr>`).join('')}
    </tbody></table>`;
}

function bindTemplateTable(root) {
  root.querySelectorAll('[data-untemplate]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      const vm = findVM(b.dataset.untemplate);
      if (!vm) return;
      try { await post(vm, '/template', { on: false }); toast('Template mark removed'); }
      catch (err) { toast(err.message); }
      setTimeout(refresh, 600);
    };
  });
  root.querySelectorAll('tr[data-key]').forEach((tr) => {
    tr.onclick = (e) => {
      if (e.target.closest('button')) return;
      select({ type: 'vm', key: tr.dataset.key });
    };
  });
}

// ── VM view ───────────────────────────────────────────────────────

const TABS = [
  ['summary', 'Summary'],
  ['console', 'Console'],
  ['terminal', 'Terminal'],
  ['rdp', 'RDP'],
  ['hardware', 'Hardware'],
  ['options', 'Options'],
  ['snapshots', 'Snapshots'],
  ['events', 'Events'],
  ['yaml', 'YAML'],
];

function renderVM(main, vm) {
  // Local QEMU VMs (#91): lifecycle, VNC console (the ws bridge dials the
  // local VNC port), and info — cluster concepts (migrate, snapshots, pause,
  // templates…) don't exist on this backend.
  const isLocal = vm.backend === 'qemu';
  const isKubeVirt = vm.backend === 'kubevirt';
  const capability = vm.capabilities || {};
  const tabs = TABS.filter(([id]) => {
    if (id === 'summary' || id === 'yaml') return true;
    if (id === 'console') return !!capability.vnc;
    if (id === 'terminal') return !!capability.tty;
    if (id === 'rdp') return !!capability.rdp;
    if (id === 'snapshots') return !!capability.snapshots;
    return isKubeVirt;
  });
  if (!tabs.some(([id]) => id === tab)) tab = 'summary';
  main.innerHTML = `
    <div class="page-head">
      <h1>${icon('cube')} ${esc(vm.name)}</h1>
      <span class="pill ${vm.ready ? 'on' : 'off'}">${esc(vm.status)}</span>
      ${isLocal ? '<span class="pill">local · qemu</span>' : ''}
      <div class="toolbar">
        ${capability.start ? `<button class="btn" data-act="start" ${vm.running ? 'disabled' : ''}>${icon('play')} Start</button>` : ''}
        ${capability.stop ? `<button class="btn" data-act="stop" ${vm.running ? '' : 'disabled'}>${icon('stop')} Stop</button>` : ''}
        ${capability.start && capability.stop ? `<button class="btn" data-act="restart" ${vm.running ? '' : 'disabled'}>${icon('restart')} Restart</button>` : ''}
        ${isKubeVirt ? `
        <button class="btn" data-act="pause" ${vm.ready ? '' : 'disabled'}>${icon('pause')} Pause</button>
        <button class="btn" data-act="unpause">${icon('play')} Resume</button>
        <button class="btn" data-act="migrate" ${vm.ready && vm.liveMigratable ? '' : 'disabled'}
          title="${vm.liveMigratable ? 'Live-migrate to another node' : 'Not live-migratable (persistent RWO disk)'}">${icon('migrate')} Migrate</button>
        <button class="btn" data-act="clone">${icon('clone')} Clone</button>
        <button class="btn" data-act="template" title="${vm.isTemplate ? 'Remove template mark' : 'Mark as a golden template to clone from'}">
          ${icon('template')} ${vm.isTemplate ? 'Unmark template' : 'Make template'}</button>
        <button class="btn" data-act="export" ${vm.running ? 'disabled' : ''}
          title="${vm.running ? 'Stop the VM to export its disk' : 'Download a disk backup'}">${icon('download')} Export</button>
        ${vm.bootc && caps.bootc ? `<button class="btn" data-act="upgrade"
          title="Rebuild this bootc VM's disk from the latest image and restart">${icon('restart')} Upgrade</button>` : ''}` : ''}
        ${capability.delete ? `<button class="btn danger" data-act="delete">${icon('trash')} Delete</button>` : ''}
      </div>
    </div>
    <div class="tabs">
      ${tabs.map(([id, label]) =>
        `<div class="tab ${tab === id ? 'active' : ''}" data-tab="${id}">${label}</div>`).join('')}
    </div>
    <div id="tab-body"></div>`;

  main.querySelectorAll('[data-act]').forEach((b) => {
    b.onclick = () => vmAction(vm, b.dataset.act);
  });
  main.querySelectorAll('[data-tab]').forEach((t) => {
    t.onclick = () => { disconnectConsoles(); tab = t.dataset.tab; renderVM(main, vm); markRendered(); };
  });

  renderTab(vm);
}

function renderTab(vm) {
  const body = $('#tab-body');
  const capability = vm.capabilities || {};
  switch (tab) {
    case 'summary':
      if (vm.backend !== 'kubevirt') {
        // Non-KubeVirt summary: do not issue cluster-only API calls.
        body.innerHTML = `<dl class="props">
          <dt>Status</dt><dd>${esc(vm.status)}</dd>
          <dt>Backend</dt><dd>${esc(vm.backend)}</dd>
          <dt>Context</dt><dd>${esc(vm.context || 'local')}</dd>
          <dt>vCPUs</dt><dd>${vm.cpu}</dd>
          <dt>Memory</dt><dd>${esc(vm.mem)}</dd>
          <dt>Disk</dt><dd>${esc(vm.disk || '—')}</dd>
          <dt>Tailscale IP</dt><dd>${esc(vm.ip || '—')}</dd>
          <dt>VNC</dt><dd>${vm.vnc ? `<code>vnc://${esc(vm.ip || 'host')}:${esc(vm.vnc)}</code>` : '—'}</dd>
          <dt>SSH</dt><dd><code>corral ssh ${esc(vm.name)}</code></dd>
        </dl>`;
        break;
      }
      body.innerHTML = `<dl class="props">
        <dt>Status</dt><dd>${esc(vm.status)}</dd>
        <dt>Tags</dt><dd id="vm-tags">${tagChips(vm)}</dd>
        <dt>Namespace</dt><dd>${esc(vm.namespace)}</dd>
        <dt>Node</dt><dd>${esc(vm.node || '—')}</dd>
        <dt>vCPUs</dt><dd>${vm.cpu}</dd>
        <dt>Memory</dt><dd>${esc(vm.mem)}</dd>
        <dt>Live usage</dt><dd id="vm-usage">${vm.running ? '…' : '—'}</dd>
        <dt>Pod IP</dt><dd>${esc(vm.ip || '—')}</dd>
        <dt>Live-migratable</dt><dd>${vm.liveMigratable ? 'yes' : 'no'}</dd>
        <dt>Guest agent</dt><dd>${vm.agentConnected ? 'connected' : 'not connected'}</dd>
        <dt>Tailnet proxy</dt><dd>${esc(vm.vnc || 'off')}</dd>
        <dt>SSH</dt><dd><code>corral ssh ${esc(vm.name)}</code></dd>
        <dt>RDP</dt><dd id="vm-rdp">${vm.running ? 'checking…' : '—'}</dd>
      </dl>
      <div id="cpu-graph-box" class="panel-section">
        <h2 class="section">${icon('cpu')} CPU usage</h2>
        <div id="cpu-graph"><p class="muted">${vm.running ? 'loading…' : 'VM is stopped'}</p></div>
      </div>
      <div id="guest-info"></div>
      <div id="powersched-box" class="panel-section"><p class="muted">loading schedule…</p></div>`;
      if (vm.running && capability.metrics) loadMetrics(vm);
      if (vm.running) loadCPUGraph(vm);
      if (vm.running && capability.rdp) checkRDP(vm);
      if (vm.agentConnected) loadGuestInfo(vm);
      renderPowerSchedule(vm);
      bindTags(vm);
      break;
    case 'console': connectVNC(vm, body); break;
    case 'terminal': connectTTY(vm, body); break;
    case 'rdp': connectRDP(vm, body); break;
    case 'hardware': renderHardware(vm, body); break;
    case 'options': renderOptions(vm, body); break;
    case 'snapshots': renderSnapshots(vm, body); break;
    case 'events': renderEvents(vm, body); break;
    case 'yaml':
      body.innerHTML = `<pre class="yaml">loading…</pre>`;
      api(vmURL(vm))
        .then((j) => { body.querySelector('pre').textContent = JSON.stringify(j, null, 2); })
        .catch((e) => { body.querySelector('pre').textContent = e.message; });
      break;
  }
}

async function vmAction(vm, act) {
  if (act === 'delete') {
    if (!confirm(`Delete ${vm.name} and its disks?`)) return;
    try {
      let target = vmURL(vm);
      if (vm.backend === 'libvirt') target += `${target.includes('?') ? '&' : '?'}destroyStorage=true`;
      await api(target, { method: 'DELETE' });
      select({ type: 'dc' });
    } catch (e) { toast(e.message); }
    return refresh();
  }
  if (act === 'migrate') return migrateVM(vm);
  if (act === 'clone') return cloneVM(vm);
  if (act === 'template') {
    try { await post(vm, '/template', { on: !vm.isTemplate }); toast(vm.isTemplate ? 'Template mark removed' : 'Marked as template'); }
    catch (e) { toast(e.message); }
    return setTimeout(refresh, 600);
  }
  if (act === 'export') return exportVM(vm);
  if (act === 'upgrade') {
    const ref = prompt('Rebuild from which bootc image?\nLeave blank to pull the latest of the current image, or enter a new image to switch.', '');
    if (ref === null) return; // cancelled
    try {
      const res = await api(vmURL(vm, '/bootc/rebuild'), {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ image: ref.trim() }),
      });
      if (res.task) watchBuild(res.task, vm.name);
    } catch (e) { toast(e.message); }
    return;
  }
  try {
    await api(vmURL(vm, `/${act}`), { method: 'POST' });
  } catch (e) { toast(e.message); }
  setTimeout(refresh, 800);
}

async function post(vm, path, body) {
  return api(vmURL(vm, path), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
}

async function migrateVM(vm) {
  const others = nodes.filter((n) => n.ready && n.name !== vm.node).map((n) => n.name);
  if (!others.length) { toast('No other ready node to migrate to.'); return; }
  const target = await pickNode(vm, others);
  if (target === null) return; // cancelled
  let res;
  try {
    res = await post(vm, '/migrate', { targetNode: target });
  } catch (e) { toast(e.message); return; }
  if (res && res.task) {
    watchBuild(res.task, vm.name, {
      titleRun: `Migrating ${vm.name}${target ? ` → ${target}` : ''}…`,
      titleDone: `✅ ${vm.name} migrated`,
      titleFail: `❌ Migration failed`,
    });
  }
}

// exportVM lets the user pick a disk-backup format, then downloads it. qcow2
// (compact, compressed, portable) is the default; raw.gz is the no-qemu-img
// fallback the server always supports.
async function exportVM(vm) {
  const fmt = await pickExportFormat(vm);
  if (!fmt) return; // cancelled
  toast('Preparing backup… the download will start when ready.');
  const q = fmt === 'qcow2' ? '?format=qcow2' : '';
  const base = vmURL(vm, '/export');
  window.location.href = base + (base.includes('?') ? `&${q.slice(1)}` : q);
}

function pickExportFormat(vm) {
  return new Promise((resolve) => {
    const dlg = document.createElement('dialog');
    dlg.className = 'pick-dialog';
    dlg.innerHTML = `
      <h3>Export ${esc(vm.name)}</h3>
      <p class="muted">Download a backup of the VM's disk.</p>
      <label>Format
        <select id="pick-fmt">
          <option value="qcow2">qcow2 — compact, compressed, portable (recommended)</option>
          <option value="raw">raw.gz — gzipped raw image (always available)</option>
        </select>
      </label>
      <p class="muted" style="font-size:.78rem">qcow2 needs qemu-img on the server; if it's unavailable the download falls back with a clear error and raw.gz still works.</p>
      <div class="pick-actions">
        <button class="btn" value="cancel">Cancel</button>
        <button class="btn primary" id="pick-go">${icon('download')} Download</button>
      </div>`;
    document.body.appendChild(dlg);
    const finish = (val) => { dlg.close(); dlg.remove(); resolve(val); };
    dlg.querySelector('[value="cancel"]').onclick = () => finish(null);
    dlg.querySelector('#pick-go').onclick = () => finish(dlg.querySelector('#pick-fmt').value);
    dlg.addEventListener('cancel', () => finish(null));
    dlg.showModal();
  });
}

// pickNode shows a small modal with a target-node dropdown (eligible nodes
// only). Resolves to the chosen node name, '' for "let the scheduler choose",
// or null if cancelled.
function pickNode(vm, eligible) {
  return new Promise((resolve) => {
    const dlg = document.createElement('dialog');
    dlg.className = 'pick-dialog';
    dlg.innerHTML = `
      <h3>Migrate ${esc(vm.name)}</h3>
      <p class="muted">Currently on <strong>${esc(vm.node || '—')}</strong>. Pick a target node.</p>
      <label>Target node
        <select id="pick-node">
          <option value="">Auto — let the scheduler choose</option>
          ${eligible.map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join('')}
        </select>
      </label>
      <div class="pick-actions">
        <button class="btn" value="cancel">Cancel</button>
        <button class="btn primary" id="pick-go">${icon('migrate')} Migrate</button>
      </div>`;
    document.body.appendChild(dlg);
    const finish = (val) => { dlg.close(); dlg.remove(); resolve(val); };
    dlg.querySelector('[value="cancel"]').onclick = () => finish(null);
    dlg.querySelector('#pick-go').onclick = () => finish(dlg.querySelector('#pick-node').value);
    dlg.addEventListener('cancel', () => finish(null)); // Esc key
    dlg.showModal();
  });
}

async function cloneVM(vm) {
  const target = prompt(`Clone ${vm.name} to a new VM named:`, `${vm.name}-clone`);
  if (!target) return;
  try {
    await post(vm, '/clone', { target: target.trim() });
    toast(`Cloning ${vm.name} → ${target}…`);
  } catch (e) { toast(e.message); }
  setTimeout(refresh, 1500);
}

async function loadGuestInfo(vm) {
  let info;
  try { info = await api(vmURL(vm, '/guestinfo')); }
  catch { return; /* agent dropped */ }
  const os = info.os || {};
  const fss = Array.isArray(info.filesystems) ? info.filesystems : (info.filesystems?.items || []);
  const el = $('#guest-info');
  if (!el) return;
  el.innerHTML = `<h2 class="section">Guest agent</h2>
    <dl class="props">
      <dt>OS</dt><dd>${esc(os.prettyName || os.name || '—')}</dd>
      <dt>Kernel</dt><dd>${esc(os.kernelRelease || '—')}</dd>
      <dt>Hostname</dt><dd>${esc(os.hostname || '—')}</dd>
      ${fss.map((f) => `<dt>FS ${esc(f.mountPoint || f.name || '?')}</dt>
        <dd>${esc(f.fileSystemType || '')} ${f.usedBytes != null ? `· ${gib(f.usedBytes)}/${gib(f.totalBytes)} GiB` : ''}</dd>`).join('')}
    </dl>`;
}

const gib = (b) => (Number(b || 0) / 1073741824).toFixed(1);

// Probe the VM for an open RDP port (Windows native, or Linux via
// gnome-remote-desktop/xrdp) and surface how to connect.
async function checkRDP(vm) {
  let r;
  try { r = await api(vmURL(vm, '/rdp')); } catch { r = { open: false }; }
  const el = $('#vm-rdp');
  if (!el) return; // user navigated away
  el.innerHTML = r.open
    ? `available — <code>virtctl port-forward vm/${esc(vm.name)} 3389:3389 -n ${esc(vm.namespace)}</code> then point your RDP client at localhost:3389`
    : '—';
}

async function loadMetrics(vm) {
  try {
    const m = await api(vmURL(vm, '/metrics'));
    const el = $('#vm-usage');
    if (el) el.textContent = (m.cpu || m.mem) ? `${m.cpu || '?'} CPU · ${m.mem || '?'} mem` : 'no metrics yet';
  } catch { /* metrics-server may be absent */ }
}

// ── CPU sparkline (RRD-style history) ─────────────────────────────
// The server samples per-VM CPU into a bounded ring buffer; we poll the
// retained window and draw a sparkline. The poller self-cancels once the
// graph element leaves the DOM (tab/VM switch via disconnectConsoles).
let cpuGraphTimer = null;

function stopCPUGraph() {
  if (cpuGraphTimer) { clearInterval(cpuGraphTimer); cpuGraphTimer = null; }
}

async function loadCPUGraph(vm) {
  stopCPUGraph();
  const draw = async () => {
    const box = $('#cpu-graph');
    if (!box) { stopCPUGraph(); return; } // navigated away
    let hist;
    try { hist = await api(vmURL(vm, '/metrics/history')); }
    catch { box.innerHTML = `<p class="muted">CPU history unavailable</p>`; return; }
    box.innerHTML = cpuSparkline(hist, vm);
  };
  await draw();
  cpuGraphTimer = setInterval(draw, 15000);
}

function cpuSparkline(hist, vm) {
  if (!hist || !hist.length) {
    return `<p class="muted">No CPU samples yet — install <strong>metrics-server</strong>
      (see <em>Cluster health</em>) and give it a moment to collect data.</p>`;
  }
  const cap = (vm.cpu || 1) * 1000;            // allocated millicores
  const peak = Math.max(...hist.map((s) => s.cpu));
  const top = Math.max(cap, peak) || 1;        // y-axis ceiling
  const W = 600;
  const H = 80;
  const n = hist.length;
  const xc = (i) => (n <= 1 ? 0 : (i / (n - 1)) * W);
  const yc = (c) => H - (c / top) * H;
  const line = hist.map((s, i) => `${xc(i).toFixed(1)},${yc(s.cpu).toFixed(1)}`).join(' ');
  const area = `0,${H} ${line} ${W},${H}`;
  const last = hist[n - 1].cpu;
  const pct = ((last / cap) * 100).toFixed(0);
  const capLine = cap <= top ? `<line class="spark-cap" x1="0" y1="${yc(cap).toFixed(1)}" x2="${W}" y2="${yc(cap).toFixed(1)}" />` : '';
  const mins = Math.max(1, Math.round((n * 15) / 60));
  return `<svg class="spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img" aria-label="CPU usage sparkline">
      <polygon class="spark-area" points="${area}" />
      <polyline class="spark-line" points="${line}" />
      ${capLine}
    </svg>
    <div class="muted spark-legend">now <strong>${last}m</strong> (${pct}% of ${vm.cpu} vCPU)
      · peak ${peak}m · last ~${mins}m</div>`;
}

// Autostart/shutdown windows (schedule plugin): two cron boundaries that flip
// the VM on/off. Built into the web server (pkg/cronops) — no plugin needed.
async function renderPowerSchedule(vm) {
  const box = $('#powersched-box');
  if (!box) return;
  let s = {};
  try { s = await api(vmURL(vm, '/powerschedule')); } catch { /* form */ }
  const has = s && (s.start || s.stop);
  box.innerHTML = `
    <h2 class="section">${icon('play')} Autostart / shutdown windows</h2>
    <div class="hw-edit">
      <label>Start (cron) <input id="pwr-start" placeholder="0 9 * * 1-5" value="${esc((s && s.start) || '')}" style="width:9rem"></label>
      <label>Stop (cron) <input id="pwr-stop" placeholder="0 18 * * 1-5" value="${esc((s && s.stop) || '')}" style="width:9rem"></label>
      <button class="btn primary" id="pwr-save">Save</button>
      ${has ? '<button class="btn sm danger" id="pwr-clear">Clear</button>' : ''}
    </div>
    <p class="muted">5-field cron in the cluster's timezone. e.g. start <code>0 9 * * 1-5</code>, stop <code>0 18 * * 1-5</code> = weekdays 9–6. Leave a field blank to skip that boundary.</p>`;
  $('#pwr-save').onclick = async () => {
    try {
      await api(vmURL(vm, '/powerschedule'), {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ start: $('#pwr-start').value.trim(), stop: $('#pwr-stop').value.trim() }),
      });
      toast('Schedule saved');
    } catch (e) { toast(e.message); }
    renderPowerSchedule(vm);
  };
  const clr = $('#pwr-clear');
  if (clr) clr.onclick = async () => {
    try { await api(vmURL(vm, '/powerschedule'), { method: 'DELETE' }); toast('Schedule cleared'); }
    catch (e) { toast(e.message); }
    renderPowerSchedule(vm);
  };
}

async function renderEvents(vm, body) {
  body.innerHTML = `<p class="muted">loading…</p>`;
  let evs;
  try { evs = await api(vmURL(vm, '/events')); }
  catch (e) { body.innerHTML = `<p class="console-msg">${esc(e.message)}</p>`; return; }
  if (!evs.length) { body.innerHTML = `<p class="muted">No recent events.</p>`; return; }
  body.innerHTML = `<table><thead><tr>
      <th>Time</th><th>Type</th><th>Reason</th><th>Object</th><th>Message</th>
    </tr></thead><tbody>
    ${evs.map((e) => `<tr class="${e.type === 'Warning' ? 'ev-warn' : ''}">
      <td>${esc(e.time || '')}</td><td>${esc(e.type)}</td><td>${esc(e.reason)}</td>
      <td>${esc(e.object)}</td><td>${esc(e.message)}</td></tr>`).join('')}
    </tbody></table>`;
}

async function renderHardware(vm, body) {
  body.innerHTML = `<pre class="yaml">loading…</pre>`;
  let j;
  try { j = await api(vmURL(vm)); }
  catch (e) { body.innerHTML = `<p class="console-msg">${esc(e.message)}</p>`; return; }

  const spec = j.spec?.template?.spec ?? {};
  const cpu = spec.domain?.cpu ?? {};
  const vcpus = (cpu.sockets || 1) * (cpu.cores || 1) * (cpu.threads || 1);
  const mem = spec.domain?.memory?.guest ?? '';
  const disks = spec.domain?.devices?.disks ?? [];
  const volumes = Object.fromEntries((spec.volumes ?? []).map((v) => [v.name, v]));
  const pvcOf = (name) => volumes[name]?.persistentVolumeClaim?.claimName;
  const volDesc = (name) => {
    const v = volumes[name] || {};
    if (v.persistentVolumeClaim) return `PVC ${v.persistentVolumeClaim.claimName}`;
    if (v.containerDisk) return `containerDisk ${v.containerDisk.image}`;
    if (v.cloudInitNoCloud) return 'cloud-init';
    return Object.keys(v).filter((k) => k !== 'name').join(',') || '?';
  };
  const liveNote = vm.liveMigratable
    ? 'applies live (hotplug)'
    : 'VM will restart to apply';

  body.innerHTML = `
    <h2 class="section">${icon('cpu')} Processor &amp; memory</h2>
    <div class="hw-edit">
      <label>vCPUs <input id="hw-cpu" type="number" min="1" max="64" value="${vcpus}"></label>
      <label>Memory <input id="hw-mem" value="${esc(mem)}"></label>
      <button class="btn primary" id="hw-apply">Apply</button>
      <span class="muted">${esc(liveNote)}</span>
    </div>

    <h2 class="section">${icon('disk')} Storage
      <button class="btn" id="hw-adddisk">${icon('plus')} Add disk</button>
    </h2>
    <table><thead><tr><th>Disk</th><th>Type</th><th>Backing</th><th></th></tr></thead><tbody>
      ${disks.map((d) => {
        const pvc = pvcOf(d.name);
        return `<tr>
          <td>${esc(d.name)}</td>
          <td>${esc(d.cdrom ? 'cdrom' : 'disk')} (${esc(d.disk?.bus || d.cdrom?.bus || '—')})</td>
          <td>${esc(volDesc(d.name))}</td>
          <td>${pvc ? `<button class="btn sm" data-expand="${esc(pvc)}" ${caps.canExpand ? '' : 'disabled'}
              title="${caps.canExpand ? 'Grow this disk' : 'Storage class does not support expansion'}">${icon('expand')} Expand</button>` : ''}
            ${pvc && d.name.includes('-hp-') ? `<button class="btn sm danger" data-rmvol="${esc(d.name)}">Detach</button>` : ''}
          </td>
        </tr>`;
      }).join('')}
    </tbody></table>

    <h2 class="section">${icon('server')} Network
      ${availableNADs.length ? `<button class="btn" id="hw-addnic">${icon('plus')} Add NIC</button>` : ''}
    </h2>
    ${networkTable(spec, vm)}

    <h2 class="section">${icon('cpu')} GPU / PCI passthrough</h2>
    <div id="hw-gpus"><p class="muted">loading…</p></div>

    <h2 class="section">${icon('info')} Firmware</h2>
    <dl class="props">
      <dt>Boot</dt><dd>${spec.domain?.firmware?.kernelBoot ? 'kernel boot (bootc)' : 'BIOS'}</dd>
      <dt>Node selector</dt><dd>${Object.keys(spec.nodeSelector ?? {}).length
        ? `<code>${esc(JSON.stringify(spec.nodeSelector))}</code>`
        : '<span class="muted">(any node)</span>'}</dd>
    </dl>`;

  renderGPUs(vm);

  $('#hw-apply').onclick = async () => {
    const newCpu = parseInt($('#hw-cpu').value, 10);
    const newMem = $('#hw-mem').value.trim();
    const payload = {};
    if (newCpu && newCpu !== vcpus) payload.cpu = newCpu;
    if (newMem && newMem !== mem) payload.mem = newMem;
    if (!payload.cpu && !payload.mem) { toast('No changes'); return; }
    $('#hw-apply').disabled = true;
    try { await post(vm, '/scale', payload); toast('Applied'); }
    catch (e) { toast(e.message); }
    setTimeout(refresh, 800);
  };
  $('#hw-adddisk').onclick = async () => {
    const size = prompt('New disk size (e.g. 10Gi):', '10Gi');
    if (!size) return;
    try { await post(vm, '/volumes', { size: size.trim() }); toast('Disk added'); }
    catch (e) { toast(e.message); }
    setTimeout(() => renderHardware(vm, body), 800);
  };
  body.querySelectorAll('[data-expand]').forEach((b) => {
    b.onclick = async () => {
      const size = prompt(`Grow ${b.dataset.expand} to:`, '');
      if (!size) return;
      try { await post(vm, '/expand', { pvc: b.dataset.expand, size: size.trim() }); toast('Expanding…'); }
      catch (e) { toast(e.message); }
    };
  });
  body.querySelectorAll('[data-rmvol]').forEach((b) => {
    b.onclick = async () => {
      if (!confirm(`Detach ${b.dataset.rmvol}?`)) return;
      try {
        await api(vmURL(vm, `/volumes/${b.dataset.rmvol}`), { method: 'DELETE' });
        toast('Detached');
      } catch (e) { toast(e.message); }
      setTimeout(() => renderHardware(vm, body), 800);
    };
  });
  const addNic = $('#hw-addnic');
  if (addNic) addNic.onclick = async () => {
    const nad = prompt(`Attach a NIC on which network?\nAvailable: ${availableNADs.join(', ')}`, availableNADs[0] || '');
    if (!nad) return;
    try { await post(vm, '/nics', { nad: nad.trim() }); toast('NIC added'); }
    catch (e) { toast(e.message); }
    setTimeout(() => renderHardware(vm, body), 800);
  };
}

// Options tab (#48): fields that map honestly onto the KubeVirt VM spec —
// start-on-boot (applies immediately, no VM restart), and boot
// order/firmware/machine type (all read once at VMI startup, so they show
// an "applies on next boot" badge instead of pretending to hot-apply).
// Fields with no honest KubeVirt equivalent (PVE's "swap", BIOS-only knobs
// KubeVirt doesn't expose) are simply absent, not stubbed.
async function renderOptions(vm, body) {
  body.innerHTML = `<pre class="yaml">loading…</pre>`;
  let j;
  try { j = await api(vmURL(vm)); }
  catch (e) { body.innerHTML = `<p class="console-msg">${esc(e.message)}</p>`; return; }

  const spec = j.spec ?? {};
  const tspec = spec.template?.spec ?? {};
  const domain = tspec.domain ?? {};
  const runStrategy = spec.runStrategy || (spec.running ? 'Always' : 'Manual');
  const isBootc = !!domain.firmware?.kernelBoot;
  const firmware = domain.firmware?.bootloader?.efi ? 'uefi'
    : domain.firmware?.bootloader?.bios ? 'bios' : '';
  const machineType = domain.machine?.type || '';

  const disks = domain.devices?.disks ?? [];
  const ifaces = domain.devices?.interfaces ?? [];
  const bootDevices = [
    ...disks.map((d) => ({ name: d.name, kind: 'disk', order: d.bootOrder })),
    ...ifaces.map((i) => ({ name: i.name, kind: 'nic', order: i.bootOrder })),
  ];

  const restartBadge = `<span class="pill mid" title="Read once at VM startup — takes effect the next time the VM (re)starts">applies on next boot</span>`;
  const liveBadge = `<span class="pill on" title="A controller-level setting, not part of the VM's boot template — takes effect immediately, no restart needed">applies immediately</span>`;

  body.innerHTML = `
    <h2 class="section">${icon('play')} Start on boot ${liveBadge}</h2>
    <div class="hw-edit">
      <label>Behavior
        <select id="opt-runstrategy">
          <option value="Always" ${runStrategy === 'Always' ? 'selected' : ''}>Always — auto-starts, stays running</option>
          <option value="Manual" ${runStrategy === 'Manual' ? 'selected' : ''}>Manual — start/stop is up to you</option>
        </select>
      </label>
      <button class="btn primary" id="opt-apply-runstrategy">Apply</button>
    </div>

    <h2 class="section">${icon('info')} Firmware &amp; machine type ${restartBadge}</h2>
    ${isBootc
      ? `<p class="muted">This VM boots a bootc-built disk via UEFI firmware set at build time — not editable here.</p>`
      : `<div class="hw-edit">
          <label>Firmware
            <select id="opt-firmware">
              <option value="bios" ${firmware === 'bios' || firmware === '' ? 'selected' : ''}>BIOS</option>
              <option value="uefi" ${firmware === 'uefi' ? 'selected' : ''}>UEFI (OVMF)</option>
            </select>
          </label>
          <label>Machine type <input id="opt-machine" value="${esc(machineType)}" placeholder="q35"></label>
          <button class="btn primary" id="opt-apply-firmware">Apply</button>
        </div>`}

    <h2 class="section">${icon('expand')} Boot order ${restartBadge}</h2>
    ${bootDevices.length
      ? `<table><thead><tr><th>Device</th><th>Type</th><th>Order</th></tr></thead><tbody>
          ${bootDevices.map((d) => `<tr>
            <td>${esc(d.name)}</td><td>${d.kind}</td>
            <td><input type="number" min="1" class="opt-bootorder" data-device="${esc(d.name)}"
              value="${d.order ?? ''}" style="width:4em"></td>
          </tr>`).join('')}
        </tbody></table>
        <div class="hw-edit"><button class="btn primary" id="opt-apply-bootorder">Apply</button>
          <span class="muted">Leave blank for devices with no explicit order.</span></div>`
      : `<p class="muted">No disks or interfaces to order.</p>`}

    <h2 class="section">${icon('terminal')} Guest agent</h2>
    <dl class="props">
      <dt>Status</dt><dd>${vm.agentConnected ? 'connected' : 'not connected'}</dd>
    </dl>
    ${vm.agentConnected ? '' : `<p class="muted">No server-side toggle — qemu-guest-agent runs inside the guest.
      Add it via cloud-init (<code>packages: [qemu-guest-agent]</code>,
      <code>runcmd: [systemctl enable --now qemu-guest-agent]</code>) or bake it into a bootc image.</p>`}`;

  $('#opt-apply-runstrategy').onclick = async () => {
    const v = $('#opt-runstrategy').value;
    if (v === runStrategy) { toast('No changes'); return; }
    try { await post(vm, '/options', { runStrategy: v }); toast('Applied'); }
    catch (e) { toast(e.message); }
    setTimeout(() => renderOptions(vm, body), 800);
  };

  const applyFirmware = $('#opt-apply-firmware');
  if (applyFirmware) applyFirmware.onclick = async () => {
    const newFirmware = $('#opt-firmware').value;
    const newMachine = $('#opt-machine').value.trim();
    const payload = {};
    if (newFirmware !== firmware) payload.firmware = newFirmware;
    if (newMachine !== machineType) payload.machineType = newMachine;
    if (!payload.firmware && !payload.machineType) { toast('No changes'); return; }
    try { await post(vm, '/options', payload); toast('Applied — restart the VM for it to take effect'); }
    catch (e) { toast(e.message); }
    setTimeout(() => renderOptions(vm, body), 800);
  };

  const applyBootOrder = $('#opt-apply-bootorder');
  if (applyBootOrder) applyBootOrder.onclick = async () => {
    const bootOrder = {};
    body.querySelectorAll('.opt-bootorder').forEach((inp) => {
      const n = parseInt(inp.value, 10);
      if (n) bootOrder[inp.dataset.device] = n;
    });
    if (!Object.keys(bootOrder).length) { toast('No boot order set'); return; }
    try { await post(vm, '/options', { bootOrder }); toast('Applied — restart the VM for it to take effect'); }
    catch (e) { toast(e.message); }
    setTimeout(() => renderOptions(vm, body), 800);
  };
}

function networkTable(spec, vm) {
  const ifaces = spec.domain?.devices?.interfaces ?? [];
  const nets = Object.fromEntries((spec.networks ?? []).map((n) => [n.name, n]));
  const binding = (i) => ['masquerade', 'bridge', 'sriov', 'macvtap', 'slirp'].find((b) => i[b]) || '?';
  const netOf = (name) => {
    const n = nets[name] || {};
    if (n.pod) return 'pod network';
    if (n.multus) return `multus: ${n.multus.networkName}`;
    return Object.keys(n).filter((k) => k !== 'name')[0] || '—';
  };
  if (!ifaces.length) return `<p class="muted">No interfaces.</p>`;
  return `<table><thead><tr><th>Name</th><th>Binding</th><th>Network</th><th>IP</th></tr></thead><tbody>
    ${ifaces.map((i) => `<tr>
      <td>${esc(i.name)}</td><td>${esc(binding(i))}</td><td>${esc(netOf(i.name))}</td>
      <td>${esc(i.name === 'default' ? (vm.ip || '—') : '—')}</td></tr>`).join('')}
    </tbody></table>
    <p class="muted" style="font-size:.78rem;margin-top:6px">Secondary NIC hotplug needs Multus (not installed on this cluster).</p>`;
}

// GPU/PCI passthrough section of the Hardware tab (gpu plugin): list attached
// devices with detach, and attach from the cluster's permitted devices.
async function renderGPUs(vm) {
  const box = $('#hw-gpus');
  if (!box) return;
  let permitted = [], attached = [];
  try { permitted = await api('/api/gpus'); } catch { /* none */ }
  try { attached = await api(vmURL(vm, '/gpus')); } catch { /* none */ }

  const rows = attached.length
    ? `<table><thead><tr><th>Name</th><th>Device</th><th></th></tr></thead><tbody>
        ${attached.map((g) => `<tr><td>${esc(g.name)}</td><td><code>${esc(g.deviceName)}</code></td>
          <td><button class="btn sm danger" data-rmgpu="${esc(g.name)}">Detach</button></td></tr>`).join('')}
       </tbody></table>`
    : `<p class="muted">No GPUs attached.</p>`;

  const picker = permitted.length
    ? `<div class="hw-edit" style="margin-top:8px">
        <label>Device <select id="gpu-dev">${permitted.map((d) =>
          `<option value="${esc(d.resourceName)}">${esc(d.resourceName)} (${esc(d.type)})</option>`).join('')}</select></label>
        <button class="btn" id="gpu-attach">${icon('plus')} Attach</button>
        <span class="muted">applies on next boot</span>
      </div>`
    : `<p class="muted" style="font-size:.78rem">No passthrough devices permitted yet. An admin enables them once with
       <code>corral gpu enable --vendor &lt;vid:did&gt; --resource &lt;vendor/name&gt;</code>.</p>`;
  box.innerHTML = rows + picker;

  const attach = $('#gpu-attach');
  if (attach) attach.onclick = async () => {
    try {
      await post(vm, '/gpus', { device: $('#gpu-dev').value });
      toast('GPU attached (next boot)');
    } catch (e) { toast(e.message); }
    renderGPUs(vm);
  };
  box.querySelectorAll('[data-rmgpu]').forEach((b) => {
    b.onclick = async () => {
      try {
        await api(vmURL(vm, `/gpus/${b.dataset.rmgpu}`), { method: 'DELETE' });
        toast('GPU detached');
      } catch (e) { toast(e.message); }
      renderGPUs(vm);
    };
  });
}

async function renderSnapshots(vm, body) {
  if (!caps.canSnapshot) {
    body.innerHTML = `<p class="console-msg">Snapshots need a snapshot-capable StorageClass
      (no VolumeSnapshotClass found in this cluster).</p>`;
    return;
  }
  // Scheduling is built into the web server (pkg/cronops); it needs the same
  // snapshot-capable storage as one-off snapshots, which canSnapshot gates.
  const hasSnapsched = caps.canSnapshot;
  body.innerHTML = `
    <div class="toolbar" style="margin-bottom:12px">
      <button class="btn primary" id="snap-new">${icon('camera')} Take snapshot</button>
    </div>
    ${hasSnapsched ? `<div id="snapsched-box" class="panel-section"><p class="muted">loading schedule…</p></div>` : ''}
    <table><thead><tr><th>Name</th><th>Ready</th><th>Created</th><th></th></tr></thead>
    <tbody id="snap-rows"><tr><td colspan="4" class="muted">loading…</td></tr></tbody></table>`;

  $('#snap-new').onclick = async () => {
    try { await post(vm, '/snapshots', {}); toast('Snapshot started'); }
    catch (e) { toast(e.message); }
    setTimeout(() => renderSnapshots(vm, body), 800);
  };

  if (hasSnapsched) renderSnapSchedule(vm);

  let snaps = [];
  try { snaps = await api(vmURL(vm, '/snapshots')); }
  catch (e) { $('#snap-rows').innerHTML = `<tr><td colspan="4">${esc(e.message)}</td></tr>`; return; }

  $('#snap-rows').innerHTML = snaps.length
    ? snaps.map((s) => `<tr>
        <td>${esc(s.name)}</td>
        <td>${s.ready ? '✓' : '…'}</td>
        <td>${esc(s.created || '')}</td>
        <td>
          <button class="btn sm" data-restore="${esc(s.name)}" title="VM must be stopped">Restore</button>
          <button class="btn sm danger" data-delsnap="${esc(s.name)}">Delete</button>
        </td></tr>`).join('')
    : `<tr><td colspan="4" class="muted">No snapshots yet.</td></tr>`;

  body.querySelectorAll('[data-restore]').forEach((b) => {
    b.onclick = async () => {
      if (!confirm(`Restore ${vm.name} from ${b.dataset.restore}? The VM must be stopped first.`)) return;
      try { await post(vm, `/snapshots/${b.dataset.restore}/restore`, {}); toast('Restoring…'); }
      catch (e) { toast(e.message); }
    };
  });
  body.querySelectorAll('[data-delsnap]').forEach((b) => {
    b.onclick = async () => {
      try {
        await api(vmURL(vm, `/snapshots/${b.dataset.delsnap}`), { method: 'DELETE' });
        toast('Deleted');
      } catch (e) { toast(e.message); }
      setTimeout(() => renderSnapshots(vm, body), 500);
    };
  });
}

// Scheduled snapshots (snapsched plugin) — list/add/remove the CronJob.
async function renderSnapSchedule(vm) {
  const box = $('#snapsched-box');
  if (!box) return;
  let sched = {};
  try { sched = await api(vmURL(vm, '/snapschedule')); }
  catch { /* show the add form */ }

  if (sched && sched.schedule) {
    box.innerHTML = `
      <h2 class="section">${icon('camera')} Snapshot schedule</h2>
      <dl class="props">
        <dt>Cron</dt><dd><code>${esc(sched.schedule)}</code></dd>
        <dt>Last run</dt><dd>${esc(sched.lastRun || '— (not yet)')}</dd>
      </dl>
      <button class="btn sm danger" id="snapsched-rm">Remove schedule</button>
      <span class="muted">Existing snapshots are kept.</span>`;
    $('#snapsched-rm').onclick = async () => {
      try { await api(vmURL(vm, '/snapschedule'), { method: 'DELETE' }); toast('Schedule removed'); }
      catch (e) { toast(e.message); }
      renderSnapSchedule(vm);
    };
    return;
  }

  box.innerHTML = `
    <h2 class="section">${icon('camera')} Snapshot schedule</h2>
    <div class="hw-edit">
      <label>Every
        <select id="snapsched-every">
          <option value="6h">6 hours</option>
          <option value="30m">30 minutes</option>
          <option value="1h">1 hour</option>
          <option value="12h">12 hours</option>
          <option value="24h">daily</option>
        </select>
      </label>
      <label>Keep <input id="snapsched-keep" type="number" min="1" value="12" style="width:5rem"></label>
      <button class="btn primary" id="snapsched-add">Schedule</button>
    </div>
    <p class="muted">A CronJob snapshots the VM each tick and prunes beyond “keep”.</p>`;
  $('#snapsched-add').onclick = async () => {
    const every = $('#snapsched-every').value;
    const keep = parseInt($('#snapsched-keep').value, 10) || 12;
    try {
      await api(vmURL(vm, '/snapschedule'), {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ every, keep }),
      });
      toast('Schedule created');
    } catch (e) { toast(e.message); }
    renderSnapSchedule(vm);
  };
}

// ── Consoles ──────────────────────────────────────────────────────

const wsURL = (kind, vm) => {
  const base = `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/${kind}/${vm.namespace}/${vm.name}`;
  return base + (vm.context ? `?context=${encodeURIComponent(vm.context)}` : '');
};

// toggleFullscreen puts el into (or out of) browser fullscreen.
function toggleFullscreen(el) {
  if (document.fullscreenElement) document.exitFullscreen();
  else el.requestFullscreen?.();
}
// Element fullscreen fires no window resize, but noVNC's scaling and xterm's
// fit addon both key off it — give them one.
document.addEventListener('fullscreenchange', () => window.dispatchEvent(new Event('resize')));

async function connectVNC(vm, body) {
  if (!vm.running) {
    body.innerHTML = `<p class="console-msg">VM is not running — start it to open the console.</p>`;
    return;
  }
  body.innerHTML = `
    <div class="toolbar console-bar">
      <button class="btn sm" id="vnc-fullscreen" title="Fullscreen (Esc to leave)">${icon('expand')} Fullscreen</button>
      <label class="console-opt"><input type="checkbox" id="vnc-scale" checked> Scale to fit (local)</label>
      <label class="console-opt" title="Ask the guest to change its resolution to match the window (needs guest support)">
        <input type="checkbox" id="vnc-resize"> Remote resize</label>
    </div>
    <div id="vnc-screen"><p class="console-msg">Connecting…</p></div>`;
  try {
    const { default: RFB } = await import(
      './vendor/novnc-rfb.esm.js');
    const screen = $('#vnc-screen');
    screen.replaceChildren();
    rfb = new RFB(screen, wsURL('vnc', vm));
    rfb.scaleViewport = true;  // noVNC local scaling — fits any window size
    rfb.resizeSession = false; // remote resize is opt-in (guest must support it)

    $('#vnc-fullscreen').onclick = () => toggleFullscreen(screen);
    $('#vnc-scale').onchange = (e) => { if (rfb) rfb.scaleViewport = e.target.checked; };
    $('#vnc-resize').onchange = (e) => {
      if (!rfb) return;
      rfb.resizeSession = e.target.checked;
      if (e.target.checked) {
        // The two modes fight each other; remote resize wins when enabled.
        $('#vnc-scale').checked = false;
        rfb.scaleViewport = false;
      }
    };
    rfb.addEventListener('disconnect', () => {
      if (tab === 'console') {
        screen.innerHTML = `<p class="console-msg">Console disconnected.<br>
          <button class="btn sm" id="vnc-reconnect" style="margin-top:10px">${icon('play')} Reconnect</button></p>`;
        const b = $('#vnc-reconnect');
        if (b) b.onclick = () => connectVNC(vm, body);
      }
    });
  } catch (e) {
    body.innerHTML = `<p class="console-msg">VNC failed: ${esc(e.message)}</p>`;
  }
}

function connectTTY(vm, body) {
  if (!vm.running) {
    body.innerHTML = `<p class="console-msg">VM is not running — start it to open the serial console.</p>`;
    return;
  }
  body.innerHTML = `
    <div class="toolbar console-bar">
      <button class="btn sm" id="tty-fullscreen" title="Fullscreen (Esc to leave)">${icon('expand')} Fullscreen</button>
    </div>
    <div id="tty-screen"></div>
    <p class="hint" style="color:var(--muted);font-size:.78rem">Serial console via virtctl — hit Enter if blank.</p>`;
  $('#tty-fullscreen').onclick = () => toggleFullscreen($('#tty-screen'));

  term = new Terminal({ fontSize: 14, theme: { background: '#000000' }, cursorBlink: true });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open($('#tty-screen'));
  fit.fit();
  const onResize = () => fit.fit();
  window.addEventListener('resize', onResize);

  ttyWS = new WebSocket(wsURL('tty', vm));
  ttyWS.binaryType = 'arraybuffer';
  const enc = new TextEncoder();
  ttyWS.onmessage = (e) => term.write(new Uint8Array(e.data));
  ttyWS.onclose = () => term?.write('\r\n\x1b[33m[disconnected]\x1b[0m\r\n');
  term.onData((d) => {
    if (ttyWS?.readyState === WebSocket.OPEN) ttyWS.send(enc.encode(d));
  });
  term.focus();
}

function disconnectConsoles() {
  stopCPUGraph();
  try { rfb?.disconnect(); } catch { /* already gone */ }
  rfb = null;
  try { ttyWS?.close(); } catch { /* already gone */ }
  ttyWS = null;
  try { term?.dispose(); } catch { /* already gone */ }
  term = null;
}

// ── Create dialog ─────────────────────────────────────────────────

$('#btn-create').onclick = () => {
  loadCatalog();
  // Reset to the catalog (the simple path) and update field visibility
  const srcType = document.querySelector('[name=sourceType]');
  if (srcType) srcType.value = 'catalog';
  const nsInput = document.querySelector('#create-form [name=namespace]');
  if (nsInput && !nsInput.value) nsInput.value = caps.defaultNamespace || '';
  updateSourceFields();
  wizardReset();
  $('#create-dialog').showModal();
};
$('#btn-cancel').onclick = () => $('#create-dialog').close();

// ── Create CT dialog (#50) — a separate button + form, not a toggle on the
// VM dialog, per PVE's own two-button Create VM / Create CT pattern. ──────

$('#btn-create-ct').onclick = () => $('#ct-create-dialog').showModal();
$('#ct-btn-cancel').onclick = () => $('#ct-create-dialog').close();

$('#ct-create-form').onsubmit = async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const body = {
    name: f.get('name'),
    image: f.get('image'),
    namespace: f.get('namespace') || '',
    storageClass: f.get('storageClass') || '',
    cpu: parseInt(f.get('cpu'), 10) || 1,
    mem: f.get('mem') || '512Mi',
    disk: f.get('disk') || '5Gi',
    privileged: !!f.get('privileged'),
  };
  try {
    await api('/api/cts', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    $('#ct-create-dialog').close();
    e.target.reset();
    toast(`Container ${body.name} created`);
    refresh(true);
  } catch (err) {
    toast(`Create failed: ${err.message}`);
  }
};

// ── Simple wizard: OS cards → name + size → create ────────────────

const WIZ_SIZES = {
  s: { cpu: 1, mem: '2G', disk: '15G' },
  m: { cpu: 2, mem: '4G', disk: '20G' },
  l: { cpu: 4, mem: '8G', disk: '40G' },
};
let wizImages = [];   // catalog entries + bootc entries (kind: 'bootc')
let wizSelected = null;
let wizFilter = 'all';

// A distro logo from simple-icons, falling back to a letter badge.
// (The badge swap is wired up post-render — see wizBindLogoFallbacks.)
function wizLogo(entry) {
  const letter = esc((entry.name || '?')[0].toUpperCase());
  if (!entry.logo) return `<span class="wiz-badge">${letter}</span>`;
  return `<img class="wiz-logo" data-letter="${letter}" src="https://cdn.simpleicons.org/${esc(entry.logo)}" alt="">`;
}

function wizBindLogoFallbacks(root) {
  root.querySelectorAll('img.wiz-logo').forEach((img) => {
    img.onerror = () => {
      const span = document.createElement('span');
      span.className = 'wiz-badge';
      span.textContent = img.dataset.letter || '?';
      img.replaceWith(span);
    };
  });
}

async function wizardLoad() {
  let imgs = [];
  try { imgs = await api('/api/images'); } catch { /* cards stay empty */ }
  wizImages = imgs.map((i) => ({ ...i, kind: i.url ? 'import' : i.iso ? 'iso' : 'server' }));
  if (caps.bootc) {
    $('#wiz-filters [data-filter="bootc"]').hidden = false;
    let bimgs = [];
    try { bimgs = await api('/api/images?type=bootc'); } catch { /* none */ }
    wizImages = wizImages.concat(bimgs.map((b) => ({ ...b, variant: 'bootc', kind: 'bootc' })));
  }
  wizardRenderCards();
}

function wizardRenderCards() {
  const grid = $('#wiz-cards');
  const shown = wizImages.filter((i) =>
    wizFilter === 'all' || i.variant === wizFilter);
  const cards = shown.map((i, n) => `
    <div class="wiz-card" data-wiz="${n}">
      ${i.custom ? `<button class="chip-x wiz-rmsrc" data-rmsrc="${esc(i.name)}" title="Remove custom source">×</button>` : ''}
      ${wizLogo(i)}
      <strong>${esc(i.name)}</strong>
      <span class="muted">${esc(i.description || '')}</span>
      ${i.custom ? '<span class="pill">custom</span>'
        : i.variant === 'bootc' ? '<span class="pill mid">bootc</span>'
        : i.iso ? '<span class="pill">installer</span>' : ''}
    </div>`).join('');
  // An always-present "add your own" card.
  const addCard = `<div class="wiz-card wiz-add" id="wiz-add-src">
      <span class="wiz-add-plus">${icon('plus')}</span>
      <strong>Add source</strong>
      <span class="muted">your own image or ISO URL</span>
    </div>`;
  grid.innerHTML = cards + addCard;
  wizBindLogoFallbacks(grid);
  grid.querySelectorAll('[data-wiz]').forEach((card) => {
    card.onclick = (e) => {
      if (e.target.closest('.wiz-rmsrc')) return; // handled below
      wizSelected = shown[parseInt(card.dataset.wiz, 10)];
      $('#wiz-selected').innerHTML = `${wizLogo(wizSelected)} <strong>${esc(wizSelected.name)}</strong>
        <span class="muted">${esc(wizSelected.description || '')}</span>`;
      wizBindLogoFallbacks($('#wiz-selected'));
      $('#wiz-step-1').hidden = true;
      $('#wiz-step-2').hidden = false;
      $('#wiz-back').hidden = false;
      $('#wiz-create').hidden = false;
      $('#wiz-name').focus();
    };
  });
  grid.querySelectorAll('[data-rmsrc]').forEach((b) => {
    b.onclick = async (e) => {
      e.stopPropagation();
      if (!confirm(`Remove custom source "${b.dataset.rmsrc}"?`)) return;
      try { await api(`/api/sources/${encodeURIComponent(b.dataset.rmsrc)}`, { method: 'DELETE' }); }
      catch (err) { toast(err.message); return; }
      wizardLoad();
    };
  });
  $('#wiz-add-src').onclick = addCustomSource;
}

// addCustomSource prompts for a name/kind/URI, persists it (ConfigMap), and
// reloads the wizard so the new card appears alongside the catalog.
async function addCustomSource() {
  const src = await sourceDialog();
  if (!src) return;
  try {
    await api('/api/sources', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(src),
    });
  } catch (e) { toast(e.message); return; }
  toast(`Added source "${src.name}"`);
  wizardLoad();
}

function sourceDialog() {
  return new Promise((resolve) => {
    const dlg = document.createElement('dialog');
    dlg.className = 'pick-dialog';
    dlg.innerHTML = `
      <h3>Add a custom source</h3>
      <label>Name <input id="src-name" placeholder="my-image" autofocus></label>
      <label>Kind
        <select id="src-kind">
          <option value="containerDisk">Container image (boots directly)</option>
          <option value="url">Disk image URL (qcow2/raw, imported)</option>
          <option value="iso">Installer ISO URL</option>
        </select>
      </label>
      <label id="src-uri-l">URI <input id="src-uri" placeholder="ghcr.io/me/image:tag"></label>
      <label>Description <input id="src-desc" placeholder="optional"></label>
      <div class="pick-actions">
        <button class="btn" value="cancel">Cancel</button>
        <button class="btn primary" id="src-go">${icon('plus')} Add</button>
      </div>`;
    document.body.appendChild(dlg);
    const finish = (val) => { dlg.close(); dlg.remove(); resolve(val); };
    const ph = { containerDisk: 'ghcr.io/me/image:tag', url: 'https://…/disk.qcow2', iso: 'https://…/installer.iso' };
    dlg.querySelector('#src-kind').onchange = (e) => { dlg.querySelector('#src-uri').placeholder = ph[e.target.value]; };
    dlg.querySelector('[value="cancel"]').onclick = () => finish(null);
    dlg.querySelector('#src-go').onclick = () => {
      const name = dlg.querySelector('#src-name').value.trim();
      const uri = dlg.querySelector('#src-uri').value.trim();
      if (!name || !uri) { toast('Name and URI are required'); return; }
      finish({
        name, uri,
        kind: dlg.querySelector('#src-kind').value,
        description: dlg.querySelector('#src-desc').value.trim(),
      });
    };
    dlg.addEventListener('cancel', () => finish(null));
    dlg.showModal();
  });
}

function wizardReset() {
  wizSelected = null;
  $('#wizard-simple').hidden = false;
  $('#create-form').hidden = true;
  $('#wiz-step-1').hidden = false;
  $('#wiz-step-2').hidden = true;
  $('#wiz-back').hidden = true;
  $('#wiz-create').hidden = true;
  wizardLoad();
}

$('#wiz-filters').querySelectorAll('.wiz-chip').forEach((chip) => {
  chip.onclick = () => {
    wizFilter = chip.dataset.filter;
    $('#wiz-filters').querySelectorAll('.wiz-chip').forEach((c) => c.classList.toggle('active', c === chip));
    wizardRenderCards();
  };
});
$('#wiz-sizes').querySelectorAll('.wiz-size').forEach((sz) => {
  sz.onclick = () => $('#wiz-sizes').querySelectorAll('.wiz-size')
    .forEach((c) => c.classList.toggle('selected', c === sz));
});
$('#wiz-back').onclick = () => {
  $('#wiz-step-1').hidden = false;
  $('#wiz-step-2').hidden = true;
  $('#wiz-back').hidden = true;
  $('#wiz-create').hidden = true;
};
$('#wiz-cancel').onclick = () => $('#create-dialog').close();
$('#wiz-advanced').onclick = () => {
  $('#wizard-simple').hidden = true;
  $('#create-form').hidden = false;
};
$('#btn-simple').onclick = () => {
  $('#create-form').hidden = true;
  $('#wizard-simple').hidden = false;
};

$('#wiz-create').onclick = async () => {
  const name = $('#wiz-name').value.trim();
  if (!name || !wizSelected) { toast('Pick a name'); return; }
  const size = WIZ_SIZES[$('#wiz-sizes .selected')?.dataset.size || 'm'];
  const body = {
    name,
    namespace: caps.defaultNamespace || '',
    cpu: size.cpu, mem: size.mem, disk: size.disk,
    sshKey: $('#wiz-sshkey').value.trim(),
  };
  if (wizSelected.kind === 'bootc') body.bootc = wizSelected.image;
  else body.image = wizSelected.name;
  $('#wiz-create').disabled = true;
  try {
    const res = await api('/api/vms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    $('#create-dialog').close();
    $('#wiz-name').value = '';
    if (res.task) watchBuild(res.task, name);
    refresh();
  } catch (err) {
    toast(`Create failed: ${err.message}`);
  }
  $('#wiz-create').disabled = false;
};
$('#btn-build-close').onclick = () => $('#build-dialog').close();

const SOURCE_HINTS = {
  containerDisk: 'quay.io/containerdisks/fedora:42',
  import: 'https://cloud-images.example/jammy.qcow2',
  iso: 'https://example.com/installer.iso',
  bootc: 'quay.io/centos-bootc/centos-bootc:stream9',
  windows: 'https://example.com/Win11_x64.iso (or pick a preset ↓)',
  pvc: 'existing-pvc-name',
};

async function loadCatalog() {
  let imgs;
  try { imgs = await api('/api/images'); } catch { imgs = []; }
  const sel = document.querySelector('[name=catalogImage]');
  // Catalog entries boot three ways; flag the slower ones so there are no surprises.
  const kindTag = (i) => (i.url ? ' [imports via CDI]' : i.iso ? ' [installer ISO]' : '');
  if (sel) sel.innerHTML = imgs.map((i) => `<option value="${esc(i.name)}">${esc(i.name)} — ${esc(i.description)}${kindTag(i)}</option>`).join('');

  // Bootc image suggestions (datalist on the source field) when the plugin is on.
  if (caps.bootc) {
    let bimgs;
    try { bimgs = await api('/api/images?type=bootc'); } catch { bimgs = []; }
    const dl = $('#bootc-catalog');
    if (dl) dl.innerHTML = bimgs.map((b) => `<option value="${esc(b.image)}">${esc(b.name)} — ${esc(b.description)}</option>`).join('');
  }
}

const CREATE_HINTS = {
  iso: 'The installer ISO boots with a blank disk — finish the install in the Console tab.',
  bootc: 'The SSH key is baked into the built disk. Bootc builds take a few minutes — a live build log opens after submit.',
  windows: 'UEFI + TPM + Hyper-V tuned, with the virtio-win driver CD-ROM attached. After boot, in Setup click “Load driver” → the virtio CD-ROM. Provide a Windows installer ISO URL.',
  pvc: 'Boots an existing disk as-is; cloud-init does not run again.',
};
const DEFAULT_HINT = 'Cloud-init VMs get your SSH key and Tailscale auth key (if configured) automatically.';

// isLocalTarget: the create form is aimed at this host's QEMU, not the
// cluster (#91 Phase 3). Local supports installer ISO / qcow2 sources only
// (no cloud-init, no cluster fields).
function selectedCreateBackend() {
  const select = document.querySelector('#create-form [name=target]');
  return select?.selectedOptions[0]?.dataset.backend || (select?.value === 'local' ? 'qemu' : 'kubevirt');
}

function isLocalTarget() {
  return selectedCreateBackend() === 'qemu';
}

function updateSourceFields() {
  const backend = selectedCreateBackend();
  const local = backend === 'qemu';
  const fileBacked = backend === 'qemu' || backend === 'libvirt';
  const kube = backend === 'kubevirt';
  const srcSel = document.querySelector('[name=sourceType]');
  // File-backed hypervisors accept ISO/qcow2; Incus accepts image aliases;
  // the full source catalog is available only to KubeVirt.
  for (const opt of srcSel.options) {
    opt.hidden = fileBacked
      ? opt.value !== 'iso' && opt.value !== 'import'
      : backend === 'incus' && opt.value !== 'containerDisk' && opt.value !== 'import';
  }
  if (fileBacked && srcSel.value !== 'iso' && srcSel.value !== 'import') srcSel.value = 'iso';
  if (backend === 'incus' && srcSel.value !== 'containerDisk' && srcSel.value !== 'import') srcSel.value = 'containerDisk';
  // Cluster-only fields disappear for a local VM.
  for (const sel of ['#create-form [name=namespace]', '#create-form [name=node]',
    '#create-form [name=instancetype]', '#create-form [name=preference]',
    '#create-form [name=cloudInit]', '#sshkey-field input']) {
    const el = document.querySelector(sel);
    if (el) el.closest('label').hidden = !kube;
  }

  const type = srcSel.value;
  $('#catalog-field').hidden = type !== 'catalog';
  $('#source-field').hidden = type === 'catalog' || type === 'pvc';
  $('#pvc-source-field').hidden = type !== 'pvc';
  $('#create-hint').textContent = fileBacked
    ? `${backend} VM. Source can be a path on the Corral web host or a URL (downloaded once, then cached). Cloud-init is not yet unified for this backend.`
    : backend === 'incus'
      ? 'Enter an Incus image alias such as images:ubuntu/24.04. Corral creates a virtual-machine instance on this remote.'
      : (CREATE_HINTS[type] || DEFAULT_HINT);
  if (fileBacked) {
    const srcInput = document.querySelector('[name=source]');
    if (srcInput) {
      srcInput.placeholder = type === 'iso'
        ? '/path/on/this/host.iso or https://…/installer.iso'
        : '/path/on/this/host.qcow2 or https://…/image.qcow2';
      srcInput.removeAttribute('list');
    }
    return;
  }
  const src = document.querySelector('[name=source]');
  if (src) {
    src.placeholder = SOURCE_HINTS[type] || '';
    // Bootc and Windows get catalog suggestions; other types are free-form.
    if (type === 'bootc') src.setAttribute('list', 'bootc-catalog');
    else if (type === 'windows') src.setAttribute('list', 'windows-iso-catalog');
    else src.removeAttribute('list');
  }
  if (type === 'pvc') loadPVCSources();
}

// Populates the "Existing PVC" dropdown from the DataVolume/ISO library
// (#49) — includes uploaded ISOs and imported images, not just a free-text
// PVC name. Ready-only (Succeeded) entries, since anything still importing
// isn't bootable yet.
async function loadPVCSources() {
  const sel = document.querySelector('[name=pvcSource]');
  if (!sel) return;
  let dvs;
  try { dvs = await api('/api/datavolumes'); } catch { dvs = []; }
  const ready = dvs.filter((d) => d.phase === 'Succeeded');
  sel.innerHTML = ready.length
    ? ready.map((d) => `<option value="${esc(d.name)}">${esc(d.name)} (${esc(d.size || '?')}, ${esc(d.namespace)})</option>`).join('')
    : `<option value="">— no ready images in the library, use Import/Upload first —</option>`;
}

document.querySelector('[name=sourceType]').onchange = updateSourceFields;
document.querySelector('#create-form [name=target]').onchange = updateSourceFields;

$('#create-form').onsubmit = async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const body = {
    name: f.get('name'),
    target: f.get('target'),
    namespace: f.get('namespace'),
    node: f.get('node'),
    cpu: parseInt(f.get('cpu'), 10) || 2,
    mem: f.get('mem'),
    disk: f.get('disk'),
    cloudInit: f.get('cloudInit') || '',
    instancetype: f.get('instancetype') || '',
    preference: f.get('preference') || '',
  };
  const src = f.get('source');
  const type = f.get('sourceType');
  if (selectedCreateBackend() === 'qemu') {
    // Local QEMU VM (#91 Phase 3): lean payload, ISO or qcow2 only.
    const local = { name: body.name, target: body.target, cpu: body.cpu, mem: body.mem, disk: body.disk };
    if (type === 'import') local.import = src; else local.iso = src;
    try {
      const res = await api('/api/vms', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(local),
      });
      $('#create-dialog').close();
      e.target.reset();
      if (res.status === 'downloading') toast(`Downloading image for ${local.name} — progress in the Tasks panel`);
      refresh();
    } catch (err) {
      toast(`Create failed: ${err.message}`);
    }
    return;
  }
  body.sshKey = f.get('sshKey') || ''; // key or GitHub username, any source type
  if (type === 'catalog') body.image = f.get('catalogImage');
  else if (type === 'containerDisk') body.containerDisk = src;
  else if (type === 'import') body.import = src;
  else if (type === 'iso') body.iso = src;
  else if (type === 'bootc') body.bootc = src;
  else if (type === 'windows') { body.windows = true; body.iso = src; }
  else body.pvc = f.get('pvcSource') || src; // pvc: prefer the library dropdown, fall back to free text

  try {
    const res = await api('/api/vms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    $('#create-dialog').close();
    e.target.reset();
    if (res.task) watchBuild(res.task, body.name);
    refresh();
  } catch (err) {
    toast(`Create failed: ${err.message}`);
  }
};

// Poll a bootc build task, streaming its log into the build dialog.
function watchBuild(taskID, vmName, opts = {}) {
  const dlg = $('#build-dialog');
  const log = $('#build-log');
  const title = $('#build-title');
  const titleRun = opts.titleRun || `Building bootc disk for ${vmName}…`;
  const titleDone = opts.titleDone || `✅ ${vmName} ready`;
  const titleFail = opts.titleFail || `❌ Build failed`;
  title.textContent = titleRun;
  log.textContent = '';
  dlg.showModal();

  const timer = setInterval(async () => {
    let t;
    try { t = await api(`/api/tasks/${taskID}`); } catch { return; }
    log.textContent = t.log;
    log.scrollTop = log.scrollHeight;
    if (t.status === 'done') {
      clearInterval(timer);
      title.textContent = titleDone;
      refresh();
      if (opts.onDone) opts.onDone();
    } else if (t.status === 'error') {
      clearInterval(timer);
      title.textContent = titleFail;
      log.textContent += `\n${t.error}`;
    }
  }, 2000);
}

// ── Task panel (Proxmox-style activity log) ────────────────────────
// First Alpine.js island — see docs/adr/0004-web-ui-alpinejs-no-build.md.
// The poll loop stays a plain setInterval (Alpine is for render, not
// fetching); only the DOM sync (row templating, collapse toggle) moved to
// x-data/x-for/x-show, replacing the old innerHTML-string templating.
//
// Registered via Alpine.data() inside an alpine:init listener, not a bare
// `window.taskPanel = ...` assignment — Alpine (a deferred classic script)
// can start scanning the DOM before this module script finishes running,
// so a plain global isn't reliably defined in time. alpine:init only fires
// when Alpine.start() actually runs (after all deferred/module scripts have
// executed), so listening for it is timing-safe regardless of script order.
document.addEventListener('alpine:init', () => {
  Alpine.data('taskPanel', () => ({
    collapsed: true,
    tasks: [],
    summary: '',
    _lastFp: '',

    start() {
      this.refresh();
      setInterval(() => this.refresh(), 5000);
    },

    async refresh() {
      let log;
      try { log = await api('/api/tasklog'); } catch { return; }
      const fp = JSON.stringify(log);
      if (fp === this._lastFp) return; // unchanged — don't reset panel scroll
      this._lastFp = fp;
      this.tasks = log;
      const running = log.filter((t) => t.status === 'running').length;
      const errors = log.filter((t) => t.status === 'error').length;
      this.summary = log.length
        ? `${running ? `${running} running · ` : ''}${errors ? `${errors} failed · ` : ''}${log.length} total`
        : '';
    },
  }));
});

// ── Mobile drawer ─────────────────────────────────────────────────

$('#btn-menu').onclick = () => $('#tree').classList.toggle('open');
function closeDrawer() { $('#tree').classList.remove('open'); }

// ── Boot ──────────────────────────────────────────────────────────

$('#btn-menu').innerHTML = icon('menu');
$('#btn-create').innerHTML = `${icon('plus')} Create VM`;

loadWhoami();
loadCaps();
loadInstanceTypes();
refresh();
setInterval(refresh, 5000);
// Task panel polling now lives in the taskPanel() Alpine component (x-init="start()").

// ── RDP console (IronRDP via RDCleanPath) ────────────────────────

let rdpSession = null;

function connectRDP(vm, body) {
  if (!vm.running) {
    body.innerHTML = '<p class="console-msg">VM is stopped. Start it to connect via RDP.</p>';
    return;
  }

  body.innerHTML = `
    <div class="console-bar">
      <label class="console-opt" style="margin-right:12px">
        <input type="checkbox" id="rdp-scale" checked> Scale to fit
      </label>
      <button class="btn sm" id="rdp-fullscreen">${icon('expand')} Fullscreen</button>
      <button class="btn sm" id="rdp-ctrlaltdel">Ctrl+Alt+Del</button>
    </div>
    <div id="rdp-screen" style="background:#000;border:1px solid var(--border);border-radius:var(--radius);min-height:400px;display:flex;align-items:center;justify-content:center">
      <p class="console-msg">Initializing RDP…</p>
    </div>`;

  const screen = $('#rdp-screen');
  const wsURL = `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/rdp/${vm.namespace}/${vm.name}?rdcleanpath=1`;

  // Prompt for credentials.
  const username = prompt('RDP username:', '');
  if (username === null) { screen.innerHTML = '<p class="console-msg">RDP cancelled.</p>'; return; }
  const password = prompt('RDP password:', '');
  if (password === null) { screen.innerHTML = '<p class="console-msg">RDP cancelled.</p>'; return; }

  screen.innerHTML = '';

  // IronRDP uses the UserInteraction API.
  const ironRDP = document.createElement('div');
  ironRDP.id = 'rdp-iron';
  screen.appendChild(ironRDP);

  // Fullscreen toggles the container.
  $('#rdp-fullscreen').onclick = () => {
    if (document.fullscreenElement) {
      document.exitFullscreen();
    } else {
      screen.requestFullscreen();
    }
  };

  // Ctrl+Alt+Del — IronRDP exposes this via the session.
  $('#rdp-ctrlaltdel').onclick = () => {
    if (rdpSession) rdpSession.ctrlAltDel();
  };

  // Initialize IronRDP via the ConfigBuilder API.
  // The ConfigBuilder is exposed globally by iron-remote-desktop.js.
  const cfg = new window.IronRemoteDesktop.ConfigBuilder()
    .withUsername(username)
    .withPassword(password)
    .withDestination(vm.name)
    .withProxyAddress(wsURL)
    .withAuthToken('corral')
    .build();

  const ui = new window.IronRemoteDesktop.UserInteraction(ironRDP);
  ui.setVisibility(true);
  ui.connect(cfg).then((info) => {
    rdpSession = ui;
    info.run(); // start the RDP session loop
  }).catch((e) => {
    screen.innerHTML = `<p class="console-msg">RDP connection failed: ${esc(e.message || String(e))}</p>`;
  });
}
