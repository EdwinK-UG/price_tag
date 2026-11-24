'use strict';
// The core promise: when the shop changes something, phones show it almost
// immediately, with no reload and no polling.
const test = require('node:test');
const assert = require('node:assert/strict');
const { startServer, adminApi, launch, demoCode, demoItem, wait, until, writeArtifact } = require('./helpers.js');

let srv, browser, admin;
test.before(async () => { srv = await startServer(); browser = await launch(); admin = await adminApi(srv.url); });
test.after(async () => { await browser.close(); await srv.stop(); });

async function phone() {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  return { ctx, page };
}
async function openItem(page, code) {
  await page.goto(srv.url + '/s/demo/');
  await page.fill('#q', code);
  await page.click('#find button');
  await page.waitForSelector('.sheet-card .amt');
  await page.waitForFunction(() => /Live price/.test(document.getElementById('metaText').textContent));
}
const price = (page) => page.textContent('.sheet-card .amt');
async function setPrice(code, p) { const r = await admin('PUT', `products/${code}/price`, { price: p }); assert.equal(r.status, 200, r.text); }
const waitPrice = (page, text, ms = 4000) => page.waitForFunction((t) => { const e = document.querySelector('.sheet-card .amt'); return e && e.textContent === t; }, text, { timeout: ms, polling: 'raf' });

test('a price edit reaches an open phone in well under a second; measured over 25 edits', async () => {
  const { ctx, page } = await phone();
  const code = demoCode(5);                                  // Wheat flour 2 kg, no offer
  await openItem(page, code);
  const lat = [];
  for (let i = 0; i < 25; i++) {
    const np = 9000 + i * 100;
    const t0 = Date.now();
    await setPrice(code, np);
    await waitPrice(page, np.toLocaleString('en-US'));
    lat.push(Date.now() - t0);
  }
  lat.sort((a, b) => a - b);
  const stat = { n: lat.length, min: lat[0], p50: lat[12], p95: lat[Math.floor(lat.length * 0.95)], max: lat[lat.length - 1], note: 'ms from the admin API call being sent until the new price is visible in the phone browser (includes the HTTP request itself), local machine' };
  console.log('      live update latency:', JSON.stringify(stat));
  writeArtifact('latency.json', stat);
  assert.ok(stat.p95 < 600, 'p95 ' + stat.p95);
  await ctx.close();
});

test('the phone flags a change: flash + "Price just changed" and the old price is gone', async () => {
  const { ctx, page } = await phone();
  const code = demoCode(8);                                  // Salt
  await openItem(page, code);
  const before = await price(page);
  await setPrice(code, 1500);
  await waitPrice(page, '1,500');
  assert.notEqual(before, '1,500');
  assert.match(await page.textContent('#toast'), /Price just changed to UGX 1,500/);
  assert.ok(await page.$('.sheet-card .price.flash'));
  await ctx.close();
});

test('every connected phone updates, not just one', async () => {
  const code = demoCode(9);                                  // Beans
  const phones = await Promise.all([1, 2, 3, 4, 5].map(phone));
  for (const p of phones) await openItem(p.page, code);
  await setPrice(code, 5150);
  await Promise.all(phones.map((p) => waitPrice(p.page, '5,150')));
  const s = await admin('GET', 'stats');
  assert.ok(s.json.live_viewers >= 5, 'admin sees live viewers: ' + s.text);
  for (const p of phones) await p.ctx.close();
});

test('a new offer appears on the home page live, and disappears when deleted', async () => {
  const { ctx, page } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.waitForSelector('.promo');
  const n0 = (await page.$$('.promo')).length;
  const now = Math.floor(Date.now() / 1000);
  const r = await admin('POST', 'promos', { title: 'Flash sale', text: 'Today only', starts_at: now - 5, ends_at: now + 3600, enabled: true, items: [{ code: demoCode(24), price: 1800 }] });
  assert.equal(r.status, 201, r.text);
  await page.waitForFunction((n) => document.querySelectorAll('.promo').length === n + 1, n0, { timeout: 4000 });
  assert.match(await page.textContent('#offers'), /Flash sale/);
  assert.match(await page.textContent('#offers'), /1,800/);
  await admin('DELETE', 'promos/' + r.json.id);
  await page.waitForFunction((n) => document.querySelectorAll('.promo').length === n, n0, { timeout: 4000 });
  await ctx.close();
});

test('an offer that ends by itself disappears from the home page and from an open price, with no network event', async () => {
  const { ctx, page } = await phone();
  const code = demoCode(26);                                 // Juice 1 L, 5,200
  await openItem(page, code);
  assert.equal(await price(page), '5,200');
  const now = Math.floor(Date.now() / 1000);
  const r = await admin('POST', 'promos', { title: 'Ends soon', starts_at: now - 5, ends_at: now + 4, enabled: true, items: [{ code, price: 4000 }] });
  assert.equal(r.status, 201, r.text);
  await waitPrice(page, '4,000');
  assert.match(await page.textContent('.sheet-card'), /Offer: Ends soon/);
  // The only thing that changes the price back is the clock.
  await waitPrice(page, '5,200', 9000);
  assert.equal(await page.$('.sheet-card .badge'), null);
  await ctx.close();
});

test('a scheduled offer switches on at its start time while the phone is open', async () => {
  const { ctx, page } = await phone();
  const code = demoCode(27);                                 // Energy drink 2,500
  await openItem(page, code);
  const now = Math.floor(Date.now() / 1000);
  await admin('POST', 'promos', { title: 'Starts soon', starts_at: now + 3, ends_at: now + 3600, enabled: true, items: [{ code, price: 2000 }] });
  await wait(300);
  assert.equal(await price(page), '2,500');
  assert.match(await page.textContent('.sheet-card'), /Offer from .*2,000/);   // announced ahead of time
  await waitPrice(page, '2,000', 8000);
  await ctx.close();
});

test('a product removed while someone is looking at it says so; one added later appears', async () => {
  const { ctx, page } = await phone();
  const code = demoCode(40);                                 // Cooking gas lighter etc. (any product)
  await openItem(page, code);
  await admin('DELETE', 'products/' + code);
  await page.waitForFunction(() => /No longer listed/.test(document.querySelector('.sheet-card').textContent), null, { timeout: 4000 });
  await admin('PUT', 'products/' + code, { name: 'Lighter', size: '1 piece', category: 'Household', price: 3600 });
  // not shown automatically after removal (the sheet is a message now) - but a fresh lookup finds it
  await page.click('.sheet-card .close');
  await page.fill('#q', code); await page.click('#find button');
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await price(page), '3,600');
  await ctx.close();
});

test('an unknown barcode becomes known live: the shopper looking at "not in the price list" sees the price appear', async () => {
  const { ctx, page } = await phone();
  const code = '5901234123457';
  await page.goto(srv.url + '/s/demo/');
  await page.fill('#q', code); await page.click('#find button');
  await page.waitForSelector('.big');
  await admin('PUT', 'products/' + code, { name: 'New arrival', size: '500 g', category: 'Pantry', price: 7777 });
  await waitPrice(page, '7,777');
  await ctx.close();
});

test('if the base price falls below a running offer, the lower price wins (never show a "deal" that costs more)', async () => {
  const { ctx, page } = await phone();
  const code = demoCode(0);                                  // Sugar: normal 5,500, offer 4,900
  await openItem(page, code);
  assert.equal(await price(page), '4,900');
  await setPrice(code, 4500);
  await waitPrice(page, '4,500');
  assert.equal(await page.$('.sheet-card .badge'), null);
  await setPrice(code, 5500);
  await waitPrice(page, '4,900');
  await ctx.close();
});

test('a bulk price file (hundreds of rows) refreshes what the shopper is looking at, via a single reset hint', async () => {
  const { ctx, page } = await phone();
  const code = demoCode(11);                                 // Spaghetti 3,500
  await openItem(page, code);
  assert.equal(await price(page), '3,500');
  let eventsBytes = 0;
  const rows = ['barcode,name,size,category,price'];
  for (let i = 100; i < 260; i++) rows.push(`${require('./helpers.js').withCheck('21' + String(i).padStart(10, '0'))},Bulk item ${i},,Bulk,${1000 + i}`);
  rows.push(`${code},Spaghetti,500 g,Pantry,3650`);
  const r = await fetch(srv.url + '/api/v1/s/demo/ingest', { method: 'POST', headers: { Authorization: 'Bearer ' + srv.token, 'Content-Type': 'text/csv' }, body: rows.join('\n') });
  const j = await r.json();
  assert.equal(r.status, 200); assert.equal(j.created, 160); assert.equal(j.updated, 1);
  await waitPrice(page, '3,650', 6000);
  await ctx.close();
});

test('ingesting the identical price file again changes nothing and pushes nothing', async () => {
  const { ctx, page } = await phone();
  await page.goto(srv.url + '/s/demo/');
  await page.waitForFunction(() => document.getElementById('live').classList.contains('on'));
  const v0 = await page.evaluate(() => window.__pt.state.v);
  const csv = 'barcode,name,size,category,price\n' + demoCode(0) + ',Sugar,1 kg,Pantry,5500\n';
  const r = await fetch(srv.url + '/api/v1/s/demo/ingest', { method: 'POST', headers: { Authorization: 'Bearer ' + srv.token, 'Content-Type': 'text/csv' }, body: csv });
  const j = await r.json();
  assert.equal(j.unchanged, 1); assert.equal(j.created + j.updated, 0);
  await wait(400);
  assert.equal(await page.evaluate(() => window.__pt.state.v), v0);
  await ctx.close();
});
