'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { startServer, launch, demoItem, DEMO_ITEMS, demoCode, ART } = require('./helpers.js');

let srv, browser;
test.before(async () => { srv = await startServer(); browser = await launch(); });
test.after(async () => { await browser.close(); await srv.stop(); });

test('every page loads with zero console errors or CSP violations, and is actually styled', async () => {
  for (const [url, selector, prop, expected] of [
    ['/', 'h1', 'display', 'block'], ['/s/demo/', '.top', 'position', 'sticky'], ['/s/demo/admin/', '.login', 'maxWidth', '380px'], ['/s/demo/test-sheet', '.grid', 'display', 'grid'],
  ]) {
    const ctx = await browser.newContext(); const page = await ctx.newPage();
    const problems = [];
    page.on('console', (m) => { if (['error', 'warning'].includes(m.type())) problems.push(m.text()); });
    page.on('pageerror', (e) => problems.push(e.message));
    await page.goto(srv.url + url);
    await page.waitForSelector(selector);
    const v = await page.$eval(selector, (el, p) => getComputedStyle(el)[p], prop);
    assert.equal(v, expected, `${url}: ${selector}.${prop} = ${v} (stylesheet blocked?)`);
    assert.deepEqual(problems, [], url);
    await ctx.close();
  }
});

test('the printable test sheet is scannable: every barcode it draws decodes back to the right product with the app\'s own decoder', async () => {
  const ctx = await browser.newContext({ viewport: { width: 1000, height: 900 }, deviceScaleFactor: 1 });
  const page = await ctx.newPage();
  await page.goto(srv.url + '/s/demo/test-sheet');
  await page.waitForSelector('figure svg');
  await page.addScriptTag({ url: '/static/shopper/code.js' });
  await page.addScriptTag({ url: '/static/shopper/decode.js' });
  const res = await page.evaluate(async () => {
    const out = [];
    for (const fig of document.querySelectorAll('figure')) {
      const svg = fig.querySelector('svg'), want = svg.getAttribute('aria-label').replace('Barcode ', '');
      const xml = new XMLSerializer().serializeToString(svg);
      const img = new Image();
      await new Promise((ok, bad) => { img.onload = ok; img.onerror = bad; img.src = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(xml); });
      const w = 640, h = Math.round(640 * img.naturalHeight / img.naturalWidth) + 200;      // the code roughly fills a camera frame
      const c = document.createElement('canvas'); c.width = w; c.height = h;
      const x = c.getContext('2d'); x.fillStyle = '#fff'; x.fillRect(0, 0, w, h);
      x.drawImage(img, 0, 100, w, h - 200);
      const r = PTDecode.decodeImage(x.getImageData(0, 0, w, h).data, w, h);
      out.push({ want, got: r && r.code });
    }
    return out;
  });
  assert.ok(res.length >= 20, 'sheet shows ' + res.length + ' barcodes');
  const bad = res.filter((r) => r.want !== r.got);
  assert.deepEqual(bad, [], 'barcodes that did not decode back');
});

test('the shopper app is usable at a narrow 320 px screen without sideways scrolling', async () => {
  const ctx = await browser.newContext({ viewport: { width: 320, height: 640 } });
  const page = await ctx.newPage();
  await page.goto(srv.url + '/s/demo/');
  await page.waitForSelector('.promo');
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  assert.ok(overflow <= 0, 'page is ' + overflow + 'px wider than the screen');
  await page.fill('#q', demoItem('Sugar', '2 kg').code); await page.click('#find button');
  await page.waitForSelector('.sheet-card .amt');
  const o2 = await page.evaluate(() => { const c = document.querySelector('.sheet-card'); return c.scrollWidth - c.clientWidth; });
  assert.ok(o2 <= 0, 'sheet overflows by ' + o2);
  await page.screenshot({ path: path.join(ART, 'shopper-320.png') });
  await ctx.close();
});

test('dark mode and large text stay readable (screenshots kept as artifacts)', async () => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 }, colorScheme: 'dark', deviceScaleFactor: 2 });
  const page = await ctx.newPage();
  await page.goto(srv.url + '/s/demo/');
  await page.waitForSelector('.promo');
  await page.screenshot({ path: path.join(ART, 'shopper-dark.png') });
  await page.fill('#q', demoCode(0)); await page.click('#find button');
  await page.waitForSelector('.sheet-card .amt');
  await page.screenshot({ path: path.join(ART, 'shopper-item-dark.png') });
  await ctx.close();
});
