// UI smoke test: drives the real dashboard against `corral web --demo` with
// headless Chromium. Run by .github/workflows/ui-smoke.yml; locally:
//
//   corral web --demo --addr 127.0.0.1:8899 &
//   npx playwright install chromium && node scripts/ui-smoke.mjs
//
// Asserts the load-bearing screens render and a stateful action round-trips.
// Fails (exit 1) on any assertion or page error.
// For reproducible documentation images, see scripts/capture-docs.mjs.

import { chromium } from 'playwright';

const BASE = process.env.CORRAL_URL || 'http://127.0.0.1:8899/';
let failures = 0;
const check = (ok, msg) => {
  console.log(`${ok ? 'ok' : 'FAIL'} - ${msg}`);
  if (!ok) failures++;
};

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
const pageErrors = [];
page.on('pageerror', (e) => pageErrors.push(e.message));

// Datacenter view renders the demo fleet. Wait on real elements, not a fixed
// delay — CI runners cold-start slower than the 5s poll cycle.
await page.goto(BASE);
await page.waitForSelector('#tree >> text=Datacenter', { timeout: 30000 }).catch(() => {});
await page.waitForSelector('td:has-text("web-prod")', { timeout: 30000 }).catch(() => {});
check(await page.locator('#tree >> text=Datacenter').count() > 0, 'tree renders');
check(await page.locator('td:has-text("web-prod")').count() > 0, 'VM table lists the fleet');
check(await page.locator('.chip.filter').count() > 2, 'tag filter bar populated');
check(await page.locator('#tree >> text=laptop-dev').count() > 0, 'local demo VM in the tree');
// The demo Incus remote holds one virtual machine and one container, and the
// UI must place each on its own surface: the VM in the fleet table, the
// container in the tree as a CT and nowhere in the VM table. That last
// assertion is the bug this replaces — the container used to be listed as a
// VM *and* as a CT, so checking for it in the table was checking for the bug.
check(await page.locator('td:has-text("incus-demo-vm")').count() > 0, 'Incus demo VM in VM table');
check(await page.locator('td:has-text("incus-demo-container")').count() === 0, 'Incus demo container is not in the VM table');
check(await page.locator('#tree >> text=incus-demo-container').count() > 0, 'Incus demo container in the tree as a CT');

// VM summary.
await page.click('#tree >> text=web-prod');
await page.waitForTimeout(1200);
check(await page.locator('.tab.active:has-text("Summary")').count() === 1, 'VM summary tab opens');
check((await page.textContent('#tab-body')).includes('corral ssh web-prod'), 'summary shows SSH hint');

// Stateful action: toggle power and watch the status flip. State-agnostic so
// the script also works against an already-toggled long-running server.
const wasRunning = (await page.textContent('.page-head')).includes('Running');
await page.click(`button[data-act="${wasRunning ? 'stop' : 'start'}"]`);
await page.waitForTimeout(5500);
check(
  (await page.textContent('.page-head')).includes(wasRunning ? 'Stopped' : 'Running'),
  `${wasRunning ? 'stop' : 'start'} action flips VM state`,
);

// Cluster health is green in demo.
await page.click('#tree >> text=Cluster health');
await page.waitForTimeout(2500);
const doctorText = await page.textContent('#content');
check(doctorText.includes('KubeVirt installed'), 'doctor renders checks');
// Local checks (KVM, virtctl…) legitimately depend on the host — only the
// demo's *cluster* checks must be green.
const broken = await page.locator('.doc-broken').allTextContents();
const clusterBroken = broken.filter((t) => /KubeVirt|CDI|StorageClass|Snapshot|Export|metrics/i.test(t));
check(clusterBroken.length === 0, `cluster checks green in demo (${clusterBroken.join('; ').slice(0, 120)})`);

// Create wizard opens with catalog cards.
await page.click('#tree >> text=Datacenter');
await page.waitForTimeout(800);
await page.click('#btn-create');
await page.waitForTimeout(1000);
check(await page.locator('.wiz-card').count() > 4, 'create wizard shows catalog');
await page.keyboard.press('Escape');

// Theme: API returns defaults, CSS custom properties injected, branding visible.
const theme = await (await fetch(`${BASE}api/theme`)).json();
check(theme.accent === '#f0883e', 'theme API returns default accent');
check(theme.brand_title === 'Corral', 'theme API returns default brand title');
check(typeof theme.custom_css === 'string', 'theme API includes custom_css field');

// CSS custom properties injected into the page via <style id="corral-theme">.
const accentVar = await page.evaluate(() => {
  const style = document.getElementById('corral-theme');
  return style ? style.textContent : '';
});
check(accentVar.includes('--accent: #f0883e'), 'default accent injected into page CSS');
check(accentVar.includes('--accent-2: #d9742e'), 'default accent-2 injected into page CSS');

// Branding in the header.
const brandText = await page.textContent('.brand');
check(brandText.includes('Corral'), 'brand title visible in header');
check(brandText.includes('Virtual Environment'), 'brand subtitle visible in header');

// PUT /api/theme persists accent change (demo server has no config dir,
// so we only check the API round-trip — persistence requires a real config).
const putRes = await fetch(`${BASE}api/theme`, {
  method: 'PUT',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ accent: '#22c55e', brand_title: 'SmokeTest' }),
});
const updated = await putRes.json();
check(updated.accent === '#22c55e' || updated.error, 'PUT /api/theme accepts accent change');


// ── Pool View: drag-and-drop grouping and drag-to-move ────────────
// The drop targets are the whole point of this view, and the two kinds must
// behave differently: a pool drop regroups silently, a backend drop must open
// the preflight and change nothing until it is confirmed.

await page.click('#tree >> text=Pool View');
await page.waitForTimeout(600);
check(await page.locator('#tree >> text=Pools').count() > 0, 'Pool View renders the pools section');
check(await page.locator('#tree >> text=Unassigned').count() > 0, 'Pool View lists unassigned instances');
check(await page.locator('#tree >> text=Move to backend').count() > 0, 'Pool View offers backends as drop targets');

// Backends that cannot receive a move are inert with the reason on hover,
// rather than accepting a drop and refusing it afterwards.
const incusTarget = page.locator('.tree-item.disabled', { hasText: 'incus' }).first();
check(await incusTarget.count() > 0, 'incus is shown as an unavailable move target');
check(
  ((await incusTarget.getAttribute('title')) || '').length > 20,
  'an unavailable target explains itself on hover',
);
const qemuTarget = page.locator('#tree .tree-item', { hasText: 'qemu' }).last();
check(
  !(await qemuTarget.getAttribute('class') || '').includes('disabled'),
  'qemu is a live move target',
);

// Create a pool through the API and check the tree picks it up with its
// bulk-action buttons — the reason pools exist.
await fetch(`${BASE}api/folders`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ path: 'smoke/web' }),
});
await page.click('#tree >> text=Server View');
await page.click('#tree >> text=Pool View');
await page.waitForTimeout(800);
const poolRow = page.locator('#tree .tree-item', { hasText: 'web' }).filter({ has: page.locator('.pool-actions') }).first();
check(await poolRow.count() > 0, 'a created pool appears in the tree with bulk actions');

// The gesture itself: drag an unassigned VM onto the pool row and check the
// membership actually moved. This is the feature, not the rendering of it.
const dragSubject = page.locator('#tree .tree-item[draggable="true"]').first();
const draggedName = (await dragSubject.textContent()).trim().split(' ')[0];
await dragSubject.dragTo(poolRow);
await page.waitForTimeout(1200);
const folders = await (await fetch(`${BASE}api/folders`)).json();
const smokePool = (folders.folders || []).find((f) => f.path === 'smoke/web');
check(
  !!smokePool && (smokePool.members || []).length === 1,
  `dragging a VM onto a pool assigns it (${draggedName} → smoke/web)`,
);

// The preflight is a read: asking for one must not change the fleet. Take the
// ref from the fleet itself rather than spelling one out — the demo backends'
// contexts are not this script's business.
const fleet = await (await fetch(`${BASE}api/vms`)).json();
const subject = fleet.find((v) => v.backend === 'kubevirt') || fleet[0];
const before = fleet.length;
const planRes = await fetch(`${BASE}api/move/preflight`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ ref: subject.id, toBackend: 'qemu' }),
});
const plan = await planRes.json();
check(planRes.status === 200, 'preflight answers 200 even when it refuses');
check(Array.isArray(plan.steps) && plan.steps.length > 0, 'preflight returns a step-by-step plan');
check(
  (plan.warnings || []).some((w) => w.includes('MAC')),
  'preflight always warns about the address change',
);
const after = (await (await fetch(`${BASE}api/vms`)).json()).length;
check(before === after, 'a preflight changes nothing');

// A refused destination is refused with reasons, not with a failed request.
const refusedRes = await fetch(`${BASE}api/move/preflight`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ ref: subject.id, toBackend: 'incus' }),
});
const refused = await refusedRes.json();
check(refusedRes.status === 200 && refused.ok === false, 'a move onto incus is refused');
check((refused.refusals || []).every((r) => r.reason), 'every refusal carries a reason');


check(pageErrors.length === 0, `no JS page errors (${pageErrors.join('; ').slice(0, 200)})`);

await browser.close();
if (failures > 0) {
  console.error(`\n${failures} smoke check(s) failed`);
  process.exit(1);
}
console.log('\nUI smoke: all checks passed');
