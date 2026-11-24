'use strict';
// Starts a real pricetag server (the production binary) on a free port with a
// freshly seeded demo shop, so tests exercise exactly what ships.
const { spawn, execFileSync } = require('node:child_process');
const net = require('node:net');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const ROOT = path.resolve(__dirname, '../..');
const BIN = process.env.PRICETAG_BIN || path.join(ROOT, 'bin', 'pricetag');
const ART = path.join(__dirname, '.artifacts');
fs.mkdirSync(ART, { recursive: true });

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); });
    s.on('error', reject);
  });
}

async function waitHealthy(url, proc, logs) {
  const t0 = Date.now();
  while (Date.now() - t0 < 10000) {
    if (proc.exitCode !== null) throw new Error('server exited early:\n' + logs.join(''));
    try { const r = await fetch(url + '/healthz'); if (r.ok) return; } catch (e) { /* not up yet */ }
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error('server did not start:\n' + logs.join(''));
}

async function startServer(extraArgs = [], env = {}) {
  if (!fs.existsSync(BIN)) throw new Error(`build the server first: go build -o bin/pricetag ./cmd/pricetag (${BIN} missing)`);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pt-e2e-'));
  execFileSync(BIN, ['demo', '-data', dir], { stdio: 'pipe' });
  const port = await freePort();
  const logs = [];
  const url = 'http://127.0.0.1:' + port;
  let proc;
  const spawnIt = async () => {
    proc = spawn(BIN, ['serve', '-addr', '127.0.0.1:' + port, '-data', dir, ...extraArgs], { stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, ...env } });
    proc.stdout.on('data', (d) => logs.push(d.toString()));
    proc.stderr.on('data', (d) => logs.push(d.toString()));
    await waitHealthy(url, proc, logs);
  };
  const killIt = async () => { const p = proc; p.kill('SIGTERM'); await new Promise((r) => { p.on('exit', r); setTimeout(r, 4000); }); };
  await spawnIt();
  const token = fs.readFileSync(path.join(dir, 'demo.token'), 'utf8').trim();
  return {
    url, dir, port, token, logs,
    async restart(downMs = 0, whileDown) { await killIt(); if (downMs) await new Promise((r) => setTimeout(r, downMs)); if (whileDown) await whileDown(); await spawnIt(); },
    async stop() { await killIt(); fs.rmSync(dir, { recursive: true, force: true }); },
  };
}

// GS1 check digit and the demo store's barcodes (see internal/demo)
function withCheck(body) { let s = 0; for (let i = 0; i < body.length; i++) s += (i % 2 === 0 ? 3 : 1) * +body[body.length - 1 - i]; return body + ((10 - s % 10) % 10); }
const demoCode = (i) => withCheck('20' + String(i + 1).padStart(10, '0'));
// The demo catalogue is read from the Go source so tests can never drift from it.
const DEMO_ITEMS = (() => {
  const src = fs.readFileSync(path.join(ROOT, 'internal/demo/demo.go'), 'utf8');
  const out = [];
  for (const m of src.matchAll(/\{"([^"]+)", "([^"]*)", "([^"]+)", (\d+)\}/g)) out.push({ name: m[1], size: m[2], category: m[3], price: +m[4] });
  return out;
})();
// demoItem('Tomato paste') -> { i, code, name, size, price }
function demoItem(name, size) {
  const i = DEMO_ITEMS.findIndex((x) => x.name === name && (size === undefined || x.size === size));
  if (i < 0) throw new Error('no demo item ' + name);
  return { i, code: demoCode(i), ...DEMO_ITEMS[i], priceText: DEMO_ITEMS[i].price.toLocaleString('en-US') };
}

// Node-side admin API client with a cookie, for driving changes while a browser watches
async function adminApi(url, slug = 'demo', email = 'owner@demo.example', password = 'demo-password-1') {
  const base = `${url}/api/v1/s/${slug}/admin/`;
  let cookie = '';
  const call = async (method, p, body, headers = {}) => {
    const res = await fetch(base + p, { method, headers: { 'X-PT-CSRF': '1', ...(body !== undefined && typeof body !== 'string' ? { 'Content-Type': 'application/json' } : {}), ...(cookie ? { Cookie: cookie } : {}), ...headers }, body: body === undefined ? undefined : (typeof body === 'string' ? body : JSON.stringify(body)) });
    const txt = await res.text();
    let json = null; try { json = JSON.parse(txt); } catch (e) { /* not json */ }
    const sc = res.headers.getSetCookie && res.headers.getSetCookie();
    if (sc && sc.length) cookie = sc.map((c) => c.split(';')[0]).join('; ');
    return { status: res.status, json, text: txt };
  };
  const r = await call('POST', 'login', { email, password });
  if (r.status !== 200) throw new Error('admin login failed: ' + r.text);
  return call;
}

async function launch(extra = {}) {
  const { chromium } = require('playwright');
  return chromium.launch({ ...extra });
}

const wait = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms = 5000, step = 25) { const t0 = Date.now(); for (;;) { const v = await fn(); if (v) return v; if (Date.now() - t0 > ms) throw new Error('timed out waiting for condition: ' + fn); await wait(step); } }

function writeArtifact(name, data) { fs.writeFileSync(path.join(ART, name), typeof data === 'string' ? data : JSON.stringify(data, null, 2)); }

module.exports = { startServer, adminApi, demoCode, demoItem, DEMO_ITEMS, withCheck, launch, wait, until, writeArtifact, ART, ROOT, BIN };
