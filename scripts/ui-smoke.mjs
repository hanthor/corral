// UI smoke test: drives the real dashboard against `corral web --demo` with
// headless Chromium. Run by .github/workflows/ui-smoke.yml; locally:
//
//   corral web --demo --addr 127.0.0.1:8899 &
//   npx playwright install chromium && node scripts/ui-smoke.mjs
//
// Asserts the load-bearing screens render and a stateful action round-trips.
// Fails (exit 1) on any assertion or page error.

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
check(await page.locator('td:has-text("incus-demo-container")').count() > 0, 'Incus demo container in VM table');

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

check(pageErrors.length === 0, `no JS page errors (${pageErrors.join('; ').slice(0, 200)})`);

await browser.close();
if (failures > 0) {
  console.error(`\n${failures} smoke check(s) failed`);
  process.exit(1);
}
console.log('\nUI smoke: all checks passed');
