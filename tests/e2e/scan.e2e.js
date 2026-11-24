'use strict';
// Camera scanning end to end: a fake webcam shows a barcode; the app must read it
// and show the price. Headless Chromium has no BarcodeDetector, so this exercises
// the built-in fallback decoder (the path used by phones without Google services).
const test = require('node:test');
const assert = require('node:assert/strict');
const { startServer, launch, demoItem } = require('./helpers.js');
const { barcodeVideo } = require('./fixtures.js');
const { chromium } = require('playwright');

let srv;
test.before(async () => { srv = await startServer(); });
test.after(async () => { await srv.stop(); });

async function withCamera(video, fn, init) {
  const browser = await chromium.launch({ args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--use-file-for-fake-video-capture=' + video] });
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 }, permissions: ['camera'] });
  if (init) await ctx.addInitScript(init);
  const page = await ctx.newPage();
  const problems = [];
  page.on('pageerror', (e) => problems.push(e.message));
  try { await fn(page, problems); } finally { await browser.close(); }
}

test('pointing the camera at a barcode shows the price (built-in decoder)', async () => {
  const sugar = demoItem('Sugar', '1 kg');
  const video = barcodeVideo('sugar', sugar.code);     // 13-digit EAN as printed
  await withCamera(video, async (page, problems) => {
    await page.goto(srv.url + '/s/demo/');
    assert.equal(await page.evaluate(() => typeof BarcodeDetector), 'undefined', 'this test is meant to use the fallback decoder');
    const t0 = Date.now();
    await page.click('#scanBtn');
    await page.waitForSelector('.sheet-card .amt', { timeout: 8000 });
    const ms = Date.now() - t0;
    assert.equal(await page.textContent('.sheet-card .name'), 'Sugar');
    assert.equal(await page.textContent('.sheet-card .amt'), '4,900');   // offer price
    await page.waitForSelector('#scanner', { state: 'hidden' });
    console.log(`      fake camera -> price on screen in ${ms} ms (includes camera start and lazy-loading the scanner)`);
    assert.deepEqual(problems, []);
  });
});

test('works with a slow phone: 6x CPU throttle still reads the barcode in a few seconds', async () => {
  const flour = demoItem('Wheat flour');
  const video = barcodeVideo('flour', flour.code);
  await withCamera(video, async (page) => {
    await page.goto(srv.url + '/s/demo/');
    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Emulation.setCPUThrottlingRate', { rate: 6 });
    const t0 = Date.now();
    await page.click('#scanBtn');
    await page.waitForSelector('.sheet-card .amt', { timeout: 15000 });
    const ms = Date.now() - t0;
    assert.equal(await page.textContent('.sheet-card .amt'), flour.priceText);
    console.log(`      6x slower CPU: barcode read and price shown in ${ms} ms`);
    assert.ok(ms < 8000);
  });
});

test('a barcode that is not in the shop says so', async () => {
  const video = barcodeVideo('unknown', '5901234123457');
  await withCamera(video, async (page) => {
    await page.goto(srv.url + '/s/demo/');
    await page.click('#scanBtn');
    await page.waitForSelector('.big', { timeout: 8000 });
    assert.match(await page.textContent('.big'), /Not in the price list/);
  });
});

test('the browser\'s native BarcodeDetector is used when present (stubbed), including UPC-E expansion', async () => {
  const stub = () => {
    window.BarcodeDetector = class { constructor(o) { this.f = o.formats; } static getSupportedFormats() { return Promise.resolve(['ean_13', 'ean_8', 'upc_a', 'upc_e']); }
      detect() { window.__detects = (window.__detects || 0) + 1; return Promise.resolve(window.__detects < 3 ? [] : [{ rawValue: '01234565', format: 'upc_e' }]); } };
  };
  await withCamera(barcodeVideo('blank', '0000000000000', { module: 1 }), async (page) => {
    const asked = [];
    page.on('request', (r) => { if (r.url().includes('/items/')) asked.push(new URL(r.url()).pathname.split('/').pop()); });
    await page.goto(srv.url + '/s/demo/');
    await page.click('#scanBtn');
    await page.waitForSelector('.sheet-card, .big', { timeout: 6000 });
    assert.deepEqual(asked, ['00012345000065'], 'UPC-E 01234565 must be expanded to UPC-A 012345000065 before lookup');
  }, stub);
});

test('a native detector that throws falls back to the built-in decoder', async () => {
  const stub = () => { window.BarcodeDetector = class { static getSupportedFormats() { return Promise.resolve(['ean_13']); } detect() { return Promise.reject(new Error('Barcode service unavailable')); } }; };
  const sugar = demoItem('Sugar', '1 kg');
  await withCamera(barcodeVideo('sugar', sugar.code), async (page) => {
    await page.goto(srv.url + '/s/demo/');
    await page.click('#scanBtn');
    await page.waitForSelector('.sheet-card .amt', { timeout: 8000 });
    assert.equal(await page.textContent('.sheet-card .name'), 'Sugar');
  }, stub);
});

test('camera problems are explained in plain words and typing still works', async () => {
  const cases = {
    NotAllowedError: /Camera access is blocked/, NotFoundError: /No camera was found/, NotReadableError: /used by another app/,
  };
  const browser = await launch();
  try {
    for (const [name, re] of Object.entries(cases)) {
      const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
      await ctx.addInitScript((n) => { navigator.mediaDevices.getUserMedia = () => Promise.reject(Object.assign(new Error(n), { name: n })); }, name);
      const page = await ctx.newPage();
      await page.goto(srv.url + '/s/demo/');
      await page.click('#scanBtn');
      await page.waitForFunction((src) => new RegExp(src).test(document.getElementById('scanMsg').textContent), re.source, { timeout: 5000 });
      await page.click('#scanClose');
      await page.fill('#q', demoItem('Salt').code); await page.click('#find button');
      await page.waitForSelector('.sheet-card .amt');
      await ctx.close();
    }
    const ctx = await browser.newContext();
    await ctx.addInitScript(() => { Object.defineProperty(navigator, 'mediaDevices', { value: undefined }); });
    const page = await ctx.newPage();
    await page.goto(srv.url + '/s/demo/');
    await page.click('#scanBtn');
    await page.waitForFunction(() => /cannot use the camera|secure/.test(document.getElementById('scanMsg').textContent));
  } finally { await browser.close(); }
});
