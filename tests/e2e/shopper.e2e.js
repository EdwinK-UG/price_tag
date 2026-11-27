'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { startServer, launch, demoCode } = require('./helpers.js');

let srv, browser;
test.before(async () => { srv = await startServer(); browser = await launch(); });
test.after(async () => { await browser.close(); await srv.stop(); });

async function phone(opts = {}) {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 }, deviceScaleFactor: 2, ...opts });
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (['error', 'warning'].includes(m.type())) problems.push(m.type() + ': ' + m.text()); });
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message));
  return { ctx, page, problems };
}

test('home page shows the shop, its announcement and live offers, with no console errors or CSP violations', async () => {
  const { ctx, page, problems } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.waitForSelector('.promo');
  assert.equal(await page.textContent('#storeName'), 'Price Tag');
  assert.match(await page.textContent('#announce'), /Open 8am/);
  const titles = await page.$$eval('.promo h3', (n) => n.map((x) => x.textContent));
  assert.deepEqual(titles.sort(), ['Fresh & dairy special', 'Weekend pantry deals']);        // "Back to school" is scheduled, not live
  assert.ok(!titles.includes('Back to school'));
  await page.waitForFunction(() => document.getElementById('live').classList.contains('on'));
  assert.deepEqual(problems, []);
  await ctx.close();
});

test('typing a barcode shows the price, the offer, and that it is live', async () => {
  const { ctx, page, problems } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.fill('#q', demoCode(0));                       // Sugar 1 kg, 5,500 with a 4,900 weekend offer
  await page.click('#find button');
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await page.textContent('.sheet-card .name'), 'Sugar');
  assert.equal(await page.textContent('.sheet-card .amt'), '4,900');
  assert.match(await page.textContent('.sheet-card'), /Normal price UGX 5,500/);
  assert.match(await page.textContent('.sheet-card'), /Offer: Weekend pantry deals/);
  await page.waitForFunction(() => /Live price/.test(document.getElementById('metaText').textContent));
  assert.deepEqual(problems, []);
  await ctx.close();
});

test('a product with no offer shows its normal price', async () => {
  const { ctx, page } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.fill('#q', demoCode(5));                       // Wheat flour 2 kg
  await page.click('#find button');
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await page.textContent('.sheet-card .amt'), '8,500');
  assert.equal(await page.$('.sheet-card .badge'), null);
  await ctx.close();
});

test('misread barcodes (bad check digit) are caught on the phone without a network call', async () => {
  const { ctx, page } = await phone();
  let apiCalls = 0;
  page.on('request', (r) => { if (r.url().includes('/items/')) apiCalls++; });
  await page.goto(srv.url + '/s/demo/');
  await page.fill('#q', '2000000000014');                   // right digits, wrong last digit
  await page.click('#find button');
  await page.waitForSelector('.big');
  assert.match(await page.textContent('.big'), /does not look right/);
  assert.equal(apiCalls, 0);
  await ctx.close();
});

test('an unknown product says so plainly', async () => {
  const { ctx, page } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.fill('#q', '5901234123457');
  await page.click('#find button');
  await page.waitForSelector('.big');
  assert.match(await page.textContent('.big'), /Not in the price list/);
  await ctx.close();
});

test('searching by name works for damaged barcodes', async () => {
  const { ctx, page } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.fill('#q', 'sugar');
  await page.click('#find button');
  await page.waitForSelector('.sheet-card .row');
  const names = await page.$$eval('.sheet-card .row b', (n) => n.map((x) => x.textContent));
  assert.ok(names.length >= 2 && names.every((x) => /sugar/i.test(x)), names.join());
  await page.click('.sheet-card .row');
  await page.waitForSelector('.sheet-card .amt');
  assert.match(await page.textContent('.sheet-card .name'), /Sugar/);
  await ctx.close();
});

test('tapping an offer item opens its price; Back closes the sheet without leaving the app', async () => {
  const { ctx, page } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.waitForSelector('.promo .row');
  await page.click('.promo .row');
  await page.waitForSelector('.sheet-card .amt');
  await page.goBack();
  await page.waitForSelector('#sheet', { state: 'hidden' });
  assert.match(page.url(), /\/s\/demo\/$/);
  await ctx.close();
});

test('recently checked items are remembered', async () => {
  const { ctx, page } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.fill('#q', demoCode(5));
  await page.click('#find button');
  await page.waitForSelector('.sheet-card .amt');
  await page.click('.sheet-card .close');
  await page.waitForSelector('#recentList .row');
  assert.match(await page.textContent('#recentList'), /Wheat flour/);
  await page.reload();
  await page.waitForSelector('#recentList .row');
  assert.match(await page.textContent('#recentList'), /Wheat flour/);
  await ctx.close();
});
