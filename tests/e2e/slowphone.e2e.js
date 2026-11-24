'use strict';
// A cheap Android phone on a poor mobile network: CPU 6x slower, Slow 3G / Fast 3G.
// Measures real bytes and times, and enforces budgets so regressions fail the build.
const test = require('node:test');
const assert = require('node:assert/strict');
const { startServer, launch, demoItem, wait, writeArtifact } = require('./helpers.js');

let srv, browser;
const results = {};
test.before(async () => { srv = await startServer(); browser = await launch(); });
test.after(async () => { writeArtifact('perf.json', results); await browser.close(); await srv.stop(); });

const NETS = {
  'Slow 3G (400 kbit/s, 400 ms RTT)': { latency: 400, down: 400 * 1000 / 8, up: 400 * 1000 / 8 },
  'Fast 3G (1.6 Mbit/s, 150 ms RTT)': { latency: 150, down: 1600 * 1000 / 8, up: 750 * 1000 / 8 },
};

async function throttled(net) {
  const ctx = await browser.newContext({ viewport: { width: 360, height: 740 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true });
  const page = await ctx.newPage();
  const cdp = await ctx.newCDPSession(page);
  await cdp.send('Network.enable');
  await cdp.send('Emulation.setCPUThrottlingRate', { rate: 6 });
  await cdp.send('Network.emulateNetworkConditions', { offline: false, latency: net.latency, downloadThroughput: net.down, uploadThroughput: net.up });
  const reqs = new Map();
  cdp.on('Network.requestWillBeSent', (e) => reqs.set(e.requestId, { url: e.request.url, bytes: 0, done: false }));
  cdp.on('Network.loadingFinished', (e) => { const r = reqs.get(e.requestId); if (r) { r.bytes = e.encodedDataLength; r.done = true; } });
  const mark = () => reqs.size;
  const since = (n0) => { const l = [...reqs.values()].slice(n0).filter((r) => r.done && !r.url.includes('/events')); return { requests: l.length, bytes: l.reduce((a, r) => a + r.bytes, 0), list: l.map((r) => new URL(r.url).pathname + ' ' + r.bytes) }; };
  return { ctx, page, cdp, mark, since };
}

for (const [label, net] of Object.entries(NETS)) {
  test(`phone on ${label}, CPU 6x slower`, async () => {
    const { ctx, page, mark, since } = await throttled(net);
    const out = {};
    // 1. very first visit ever: nothing cached
    let t0 = Date.now();
    await page.goto(srv.url + '/s/demo/', { waitUntil: 'commit' });
    await page.waitForSelector('.promo', { timeout: 60000 });
    out.cold_home_visible_ms = Date.now() - t0;
    await page.waitForFunction(() => document.getElementById('live').classList.contains('on'), null, { timeout: 60000 });
    out.cold_live_ms = Date.now() - t0;
    out.cold_fcp_ms = Math.round(await page.evaluate(() => (performance.getEntriesByName('first-contentful-paint')[0] || {}).startTime || 0));
    const cold = since(0); out.cold_bytes = cold.bytes; out.cold_requests = cold.requests; out.cold_files = cold.list;

    // 2. first price lookup
    const item = demoItem('Rice', '1 kg');
    let n0 = mark(); t0 = Date.now();
    await page.fill('#q', item.code); await page.tap('#find button');
    await page.waitForSelector('.sheet-card .amt', { timeout: 60000 });
    out.lookup_ms = Date.now() - t0;
    const lk = since(n0); out.lookup_bytes = lk.bytes; out.lookup_requests = lk.requests;

    // 3. same product again while the live connection says nothing changed: no network at all
    await page.tap('.sheet-card .close');
    n0 = mark(); t0 = Date.now();
    await page.fill('#q', item.code); await page.tap('#find button');
    await page.waitForSelector('.sheet-card .amt');
    out.repeat_lookup_ms = Date.now() - t0;
    out.repeat_lookup_requests = since(n0).requests;
    await page.tap('.sheet-card .close');

    // 4. next visit (service worker + saved home page)
    await page.waitForFunction(() => navigator.serviceWorker.controller, null, { timeout: 30000 });
    await wait(2500);
    n0 = mark(); t0 = Date.now();
    await page.reload({ waitUntil: 'commit' });
    await page.waitForSelector('.promo', { timeout: 60000 });
    out.warm_home_visible_ms = Date.now() - t0;
    await page.waitForFunction(() => document.getElementById('live').classList.contains('on'), null, { timeout: 60000 });
    await wait(500);
    const warm = since(n0); out.warm_bytes = warm.bytes; out.warm_requests = warm.requests; out.warm_files = warm.list;

    results[label] = out;
    console.log(`      ${label}: ${JSON.stringify({ ...out, cold_files: undefined, warm_files: undefined })}`);
    await ctx.close();

    // budgets (cold = everything a first-time visitor downloads: page, CSS, JS, home data)
    assert.ok(out.cold_bytes < 30 * 1024, `cold visit downloads ${out.cold_bytes} bytes`);
    assert.ok(out.lookup_bytes < 650, `a price lookup costs ${out.lookup_bytes} bytes`);
    assert.equal(out.repeat_lookup_requests, 0, 'a repeat lookup while live must not touch the network');
    assert.ok(out.warm_bytes < 6 * 1024, `a repeat visit costs ${out.warm_bytes} bytes`);
    assert.ok(out.cold_home_visible_ms < (label.startsWith('Slow') ? 9000 : 5000), `cold home visible after ${out.cold_home_visible_ms} ms`);
    assert.ok(out.lookup_ms < (label.startsWith('Slow') ? 3500 : 2000), `lookup took ${out.lookup_ms} ms`);
  });
}
