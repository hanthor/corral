#!/usr/bin/env node
// Reproducible documentation screenshots from Corral's real demo surfaces.
// Usage: npm install --prefix e2e && node scripts/capture-docs.mjs [output-dir]

import {execFileSync, spawn} from 'node:child_process';
import {mkdirSync, mkdtempSync, rmSync, unlinkSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {resolve} from 'node:path';
import {setTimeout as delay} from 'node:timers/promises';
import {chromium} from '../e2e/node_modules/playwright/index.mjs';

const root = resolve(import.meta.dirname, '..');
const output = resolve(process.argv[2] || `${root}/docs/screenshots/generated`);
const binary = process.env.CORRAL_BIN || `${root}/corral`;
const base = process.env.CORRAL_URL || 'http://127.0.0.1:8899';
const fixtureHome = mkdtempSync(`${tmpdir()}/corral-docs-`);
const fixtureEnv = {
  ...process.env,
  XDG_CONFIG_HOME: `${fixtureHome}/config`,
  XDG_DATA_HOME: `${fixtureHome}/data`,
};
mkdirSync(output, {recursive: true});

const server = spawn(binary, ['web', '--demo', '--addr', '127.0.0.1:8899'], {
  cwd: root, env: fixtureEnv, stdio: ['ignore', 'pipe', 'pipe'],
});
let serverLog = '';
server.stderr.on('data', (b) => { serverLog += b; });

async function ready() {
  for (let i = 0; i < 60; i++) {
    try { if ((await fetch(base)).ok) return; } catch { /* starting */ }
    await delay(250);
  }
  throw new Error(`Corral web did not start: ${serverLog}`);
}

function tmux(...args) {
  return execFileSync('tmux', args, {cwd: root, encoding: 'utf8'});
}

function terminalHTML(text, title) {
  const safe = text.replace(/[&<>]/g, (c) => ({'&': '&amp;', '<': '&lt;', '>': '&gt;'}[c]));
  return `<!doctype html><html><head><meta charset="utf-8"><style>
    *{box-sizing:border-box}body{margin:0;background:#080b10;color:#d8dee9;font:17px/1.35 ui-monospace,SFMono-Regular,Consolas,monospace}
    .frame{width:1440px;height:900px;padding:36px;background:radial-gradient(circle at 20% 0,#2b1938 0,#10151d 44%,#080b10 100%)}
    .terminal{height:100%;border:1px solid #384152;border-radius:14px;background:#0d1117;box-shadow:0 24px 80px #000b;overflow:hidden}
    .bar{height:42px;padding:12px 16px;background:#171d27;color:#9aa5b5;font:13px system-ui}.dots{color:#ff5f57;letter-spacing:5px}.title{margin-left:18px}
    pre{margin:0;padding:22px 26px;white-space:pre-wrap;color:#e5e9f0} .pink{color:#ff87d7}
  </style></head><body><div class="frame"><div class="terminal"><div class="bar"><span class="dots">● ● ●</span><span class="title">${title}</span></div><pre>${safe}</pre></div></div></body></html>`;
}

async function captureTUI(browser) {
  const session = `corral-docs-${process.pid}`;
  try {
    const command = `XDG_CONFIG_HOME=${fixtureEnv.XDG_CONFIG_HOME} XDG_DATA_HOME=${fixtureEnv.XDG_DATA_HOME} ${binary} --demo`;
    tmux('new-session', '-d', '-s', session, '-x', '118', '-y', '35', command);
    await delay(1800);
    const page = await browser.newPage({viewport: {width: 1440, height: 900}, deviceScaleFactor: 1});
    for (const [name, key, title] of [
      ['tui-fleet.png', null, 'corral --demo · unified fleet'],
      ['tui-help.png', '?', 'corral --demo · command deck'],
    ]) {
      if (key) { tmux('send-keys', '-t', session, key); await delay(350); }
      const text = tmux('capture-pane', '-p', '-t', session).replace(/\s+$/g, '');
      const html = `${output}/${name}.html`;
      writeFileSync(html, terminalHTML(text, title));
      await page.goto(`file://${html}`);
      await page.screenshot({path: `${output}/${name}`});
      unlinkSync(html);
    }
    await page.close();
  } finally {
    try { tmux('kill-session', '-t', session); } catch { /* already closed */ }
  }
}

await ready();
const browser = await chromium.launch();
try {
  const page = await browser.newPage({viewport: {width: 1440, height: 900}, deviceScaleFactor: 1});
  await page.goto(base);
  await page.waitForSelector('td:has-text("web-prod")', {timeout: 30000});
  await page.screenshot({path: `${output}/web-fleet.png`});

  await page.click('#tree >> text=web-prod');
  await page.waitForSelector('.tab.active:has-text("Summary")');
  await delay(500);
  await page.screenshot({path: `${output}/web-vm-summary.png`});

  await page.click('#tree >> text=Cluster health');
  await page.waitForSelector('#content >> text=KubeVirt installed');
  await delay(500);
  await page.screenshot({path: `${output}/web-doctor.png`});

  await page.click('#tree >> text=Datacenter');
  await page.click('#btn-create');
  await page.waitForSelector('#create-dialog[open] .wiz-card');
  await page.screenshot({path: `${output}/web-create.png`});

  await page.close();
  await captureTUI(browser);
} finally {
  await browser.close();
  server.kill('SIGTERM');
  rmSync(fixtureHome, {recursive: true, force: true});
}

console.log(`Captured Corral documentation screenshots in ${output}`);
