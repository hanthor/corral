// Pool View: user-defined folders (ADR-0008) as a drag-and-drop tree, with
// backends as drop targets for a cross-backend move (ADR-0010).
//
// Two kinds of node live in this tree and they mean different things, so they
// look different and behave differently:
//
//   - A **pool** is a grouping. Dropping onto it reassigns membership and
//     touches no instance. It is instant and undoable by dragging back.
//   - A **backend** is a location. Dropping onto it proposes a *move*, which
//     stops the guest and copies its disk. It never acts on the drop — it
//     opens the preflight, which the server produced without touching
//     anything, and the operator reads it before anything happens.
//
// That asymmetry is the point. A drag is a cheap, exploratory gesture; a move
// is the most destructive thing Corral does to a running guest. The preflight
// is what lets the two coexist: you find out about the UEFI mismatch or the
// missing 20GiB by dragging, not by losing a VM.

// Injected by app.js so this module does not import the entry point back.
let ctx = {};
export function bindPools(helpers) { ctx = helpers; }

let pools = { folders: [], unfoldered: [] };
let destinations = [];
export const poolState = () => pools;

// loadPools refreshes both halves of the tree. Failures are soft: a pool tree
// that cannot load should degrade to "no pools yet", not blank the sidebar.
export async function loadPools() {
  try {
    pools = await ctx.api('/api/folders');
  } catch {
    pools = { folders: [], unfoldered: [] };
  }
  try {
    destinations = (await ctx.api('/api/move/destinations')).destinations || [];
  } catch {
    destinations = [];
  }
}

const depthOf = (path) => path.split('/').length - 1;
const leafOf = (path) => path.split('/').pop();

// ── the tree ──────────────────────────────────────────────────────

export function renderTreePools(tree) {
  const { treeRow, vmRow, icon, esc } = ctx;

  const header = treeRow({
    lvl: 0, icon: icon('folder'), label: 'Pools',
    sub: pools.folders.length ? `${pools.folders.length}` : 'none yet',
    onclick: () => {},
  });
  header.appendChild(newPoolButton());
  tree.appendChild(header);

  for (const folder of pools.folders) {
    const row = treeRow({
      lvl: Math.min(depthOf(folder.path) + 1, 3),
      icon: icon('folder'),
      label: leafOf(folder.path),
      sub: memberSummary(folder),
      onclick: () => {},
    });
    row.title = folder.path;
    dropTargetPool(row, folder.path);
    row.appendChild(poolActions(folder));
    tree.appendChild(row);

    for (const vm of folder.members || []) {
      const child = vmRow(vm, Math.min(depthOf(folder.path) + 2, 4));
      draggable(child, vm);
      tree.appendChild(child);
    }
    for (const missing of folder.missing || []) {
      const gone = treeRow({
        lvl: Math.min(depthOf(folder.path) + 2, 4),
        icon: icon('cube'), label: leafOf(missing), sub: 'not found', dot: 'off',
        onclick: () => {},
      });
      // Shown rather than hidden: a pool pointing at something gone is
      // information, and a partial fleet is normal in a multi-backend setup.
      gone.classList.add('muted');
      gone.title = `${missing} — in this pool but not in the current fleet`;
      tree.appendChild(gone);
    }
  }

  // Unassigned instances are the drag source that makes the tree usable at all
  // on a fresh install, where every pool is empty.
  const unassigned = pools.unfoldered || [];
  const loose = treeRow({
    lvl: 0, icon: icon('cube'), label: 'Unassigned',
    sub: unassigned.length ? `${unassigned.length}` : 'none',
    onclick: () => {},
  });
  dropTargetPool(loose, '');
  tree.appendChild(loose);
  for (const vm of unassigned) {
    const row = vmRow(vm, 1);
    draggable(row, vm);
    tree.appendChild(row);
  }

  renderBackendTargets(tree);
  void esc;
}

function memberSummary(folder) {
  const n = (folder.members || []).length;
  const missing = (folder.missing || []).length;
  const parts = [`${n} instance${n === 1 ? '' : 's'}`];
  if (missing) parts.push(`${missing} missing`);
  return parts.join(', ');
}

function renderBackendTargets(tree) {
  const { treeRow, icon } = ctx;
  if (!destinations.length) return;

  tree.appendChild(treeRow({
    lvl: 0, icon: icon('server'), label: 'Move to backend',
    sub: 'drop a VM here', onclick: () => {},
  }));

  for (const d of destinations) {
    const row = treeRow({
      lvl: 1, icon: icon('server'), label: d.backend,
      sub: d.can ? 'accepts moves' : 'unavailable',
      onclick: () => {},
    });
    if (!d.can) {
      // Greyed with the reason on hover: refusing after the drop would be
      // worse manners than not accepting it, and a target the operator cannot
      // use should say why rather than being a mystery.
      row.classList.add('disabled');
      row.title = d.reason || `${d.backend} cannot receive a moved instance`;
      tree.appendChild(row);
      continue;
    }
    row.title = `Drop a VM here to move it to ${d.backend} (the guest stops)`;
    dropTargetBackend(row, d.backend);
    tree.appendChild(row);
  }
}

// ── drag and drop ─────────────────────────────────────────────────

const DRAG_TYPE = 'application/x-corral-instance';

function draggable(row, vm) {
  row.draggable = true;
  row.addEventListener('dragstart', (e) => {
    e.dataTransfer.setData(DRAG_TYPE, vm.id);
    e.dataTransfer.setData('text/plain', vm.name);
    e.dataTransfer.effectAllowed = 'move';
    row.classList.add('dragging');
  });
  row.addEventListener('dragend', () => row.classList.remove('dragging'));
}

// dropZone wires the three events every target needs identically, so a target
// differs only in what it does with the ref.
function dropZone(row, onDrop) {
  row.addEventListener('dragover', (e) => {
    if (!e.dataTransfer.types.includes(DRAG_TYPE)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    row.classList.add('drop-target');
  });
  row.addEventListener('dragleave', () => row.classList.remove('drop-target'));
  row.addEventListener('drop', (e) => {
    row.classList.remove('drop-target');
    const ref = e.dataTransfer.getData(DRAG_TYPE);
    if (!ref) return;
    e.preventDefault();
    e.stopPropagation();
    onDrop(ref);
  });
}

// Dropping onto a pool is grouping only — no instance is touched, so it commits
// immediately rather than asking. Dragging it back undoes it.
function dropTargetPool(row, path) {
  dropZone(row, async (ref) => {
    try {
      if (path === '') {
        await ctx.api(`/api/folders/members?ref=${encodeURIComponent(ref)}`, { method: 'DELETE' });
        ctx.toast('Removed from its pool');
      } else {
        await ctx.api('/api/folders/members', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path, ref }),
        });
        ctx.toast(`Added to ${path}`);
      }
    } catch (e) {
      ctx.toast(e.message);
      return;
    }
    await loadPools();
    ctx.refresh(true);
  });
}

// Dropping onto a backend proposes a move. The drop itself only asks the server
// for a plan — safe to do on every stray drag.
function dropTargetBackend(row, backend) {
  dropZone(row, async (ref) => {
    let plan;
    try {
      plan = await ctx.api('/api/move/preflight', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ref, toBackend: backend }),
      });
    } catch (e) {
      ctx.toast(`Could not plan the move: ${e.message}`);
      return;
    }
    showMoveDialog(ref, backend, plan);
  });
}

// ── the confirmation ──────────────────────────────────────────────

// showMoveDialog renders the preflight in full. Everything the server said is
// shown, including the warnings on a refused plan — fixing the refusal does not
// make the IP change go away, and an operator who reads this once should have
// read all of it.
export function showMoveDialog(ref, backend, plan) {
  const { esc } = ctx;
  document.querySelectorAll('.move-dialog').forEach((d) => d.remove());

  const list = (items, cls) => (items || []).length
    ? `<ul class="${cls}">${items.map((i) => `<li>${esc(typeof i === 'string' ? i : i.reason)}${
        i && i.remedy ? `<div class="muted">${esc(i.remedy)}</div>` : ''}</li>`).join('')}</ul>`
    : '';

  const steps = (plan.steps || []).map(
    (s, i) => `<li><b>${i + 1}. ${esc(s.name)}</b> <span class="muted">${esc(s.detail || '')}</span></li>`,
  ).join('');

  const el = document.createElement('div');
  el.className = 'modal move-dialog';
  el.innerHTML = `
    <div class="modal-card">
      <h2>Move ${esc(plan.source?.name || '')} to ${esc(backend)}</h2>
      <p class="lede">This is not a live migration. The guest stops for the whole
        move and comes back with a new MAC address and almost certainly a new IP.</p>
      ${plan.ok ? '' : '<h3 class="bad">This move cannot run</h3>' + list(plan.refusals, 'refusals')}
      <h3>Plan</h3><ol class="steps">${steps}</ol>
      ${(plan.warnings || []).length ? `<h3>Warnings</h3>${list(plan.warnings, 'warnings')}` : ''}
      ${(plan.dropped || []).length ? `<h3>Not carried over</h3>${list(plan.dropped, 'dropped')}` : ''}
      <label class="check"><input type="checkbox" id="move-delete-source">
        Delete the source afterwards
        <span class="muted">(off by default — a stopped source is recoverable)</span></label>
      <div class="modal-actions">
        <button class="btn" id="move-cancel">Cancel</button>
        <button class="btn primary" id="move-go" ${plan.ok ? '' : 'disabled'}>Move</button>
      </div>
    </div>`;
  document.body.appendChild(el);

  const close = () => el.remove();
  el.querySelector('#move-cancel').onclick = close;
  el.onclick = (e) => { if (e.target === el) close(); };

  el.querySelector('#move-go').onclick = async () => {
    const go = el.querySelector('#move-go');
    go.disabled = true;
    go.textContent = 'Moving…';
    try {
      const result = await ctx.api('/api/move', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ref, toBackend: backend,
          deleteSource: el.querySelector('#move-delete-source').checked,
        }),
      });
      close();
      ctx.toast(`Moved to ${backend}/${result.destination?.name || ''} — created stopped`);
    } catch (e) {
      go.disabled = false;
      go.textContent = 'Move';
      ctx.toast(`Move failed: ${e.message}`);
      return;
    }
    await loadPools();
    ctx.refresh(true);
  };
}

// ── pool management ───────────────────────────────────────────────

function newPoolButton() {
  const b = document.createElement('button');
  b.className = 'btn xs tree-inline';
  b.textContent = '+';
  b.title = 'New pool (use a/b for a nested one)';
  b.onclick = async (e) => {
    e.stopPropagation();
    const path = prompt('Pool path (nest with /, e.g. prod/web):');
    if (!path) return;
    try {
      await ctx.api('/api/folders', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path }),
      });
    } catch (err) {
      ctx.toast(err.message);
      return;
    }
    await loadPools();
    ctx.refresh(true);
  };
  return b;
}

// poolActions is the bulk-action menu: the reason pools exist. Each action fans
// out server-side and reports per member, so a heterogeneous pool where one
// backend refuses does not look like a total failure.
function poolActions(folder) {
  const wrap = document.createElement('span');
  wrap.className = 'tree-inline pool-actions';
  for (const [action, label, title] of [
    ['start', '▶', 'Start every instance in this pool'],
    ['stop', '■', 'Stop every instance in this pool'],
    ['restart', '⟳', 'Restart every instance in this pool'],
  ]) {
    const b = document.createElement('button');
    b.className = 'btn xs';
    b.textContent = label;
    b.title = title;
    b.onclick = async (e) => {
      e.stopPropagation();
      if (!confirm(`${title}? (${(folder.members || []).length} instances)`)) return;
      let result;
      try {
        result = await ctx.api('/api/folders/action', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: folder.path, action }),
        });
      } catch (err) {
        ctx.toast(err.message);
        return;
      }
      ctx.toast(summariseOutcomes(action, result.members || []));
      ctx.refresh(true);
    };
    wrap.appendChild(b);
  }
  return wrap;
}

// summariseOutcomes reports skipped separately from failed. In a pool spanning
// backends, "3 skipped" (already stopped, or unsupported here) is a normal
// outcome and reading it as 3 errors would train an operator to ignore the
// toast.
export function summariseOutcomes(action, results) {
  let ok = 0; let skipped = 0; const failed = [];
  for (const r of results) {
    if (r.skipped) skipped++;
    else if (r.ok) ok++;
    else failed.push(r.name);
  }
  const parts = [`${action}: ${ok} ok`];
  if (skipped) parts.push(`${skipped} skipped`);
  if (failed.length) parts.push(`${failed.length} failed (${failed.slice(0, 3).join(', ')})`);
  return parts.join(', ');
}
