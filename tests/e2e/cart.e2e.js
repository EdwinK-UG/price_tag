'use strict';
// Stage 1: Shopper cart ("scan and go") end-to-end browser tests.
const test = require('node:test');
const assert = require('node:assert/strict');
const { startServer, adminApi, launch, demoCode, demoItem, wait } = require('./helpers.js');

let srv, browser, admin;
test.before(async () => { srv = await startServer(); browser = await launch(); admin = await adminApi(srv.url); });
test.after(async () => { await browser.close(); await srv.stop(); });

async function phone(viewport = { width: 390, height: 780 }) {
  const ctx = await browser.newContext({ viewport });
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (['error', 'warning'].includes(m.type())) problems.push(m.type() + ': ' + m.text()); });
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message));
  await page.goto(srv.url + '/s/demo/');
  await page.waitForFunction(() => document.getElementById('live').classList.contains('on'));
  return { ctx, page, problems };
}

async function look(page, code) {
  await page.fill('#q', code);
  await page.click('#find button');
  await page.waitForSelector('.sheet-card');
}

test('add to cart, adjust quantity, remove, survive reload, and clear with confirmation', async () => {
  const { ctx, page, problems } = await phone();
  const sugar = demoItem('Sugar', '1 kg');   // normal 5,500, active offer 4,900
  const flour = demoItem('Wheat flour');     // normal 8,500, no offer

  // Cold load must not have fetched cart.js yet
  assert.equal(await page.evaluate(() => !!window.PTCart), false);

  await look(page, sugar.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.waitForFunction(() => document.getElementById('cartCnt').textContent === '1');
  // Increment from the product sheet stepper
  await page.click('.cart-act button[aria-label="Add one more to cart"]');
  await page.waitForFunction(() => document.getElementById('cartCnt').textContent === '2');
  await page.click('.sheet-card .close');

  await look(page, flour.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.waitForFunction(() => document.getElementById('cartCnt').textContent === '3');
  await page.click('text=View cart');
  await page.waitForSelector('.cart-list');

  // Running total uses effective price: 2 * 4,900 + 8,500 = 18,300
  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX 18,300');
  assert.equal(await page.textContent(`.cart-item[data-code="${'0' + sugar.code}"] .cart-lt`), 'UGX 9,800');
  assert.equal(await page.textContent(`.cart-item[data-code="${'0' + flour.code}"] .cart-lt`), 'UGX 8,500');

  // Remove flour in cart view
  await page.click(`.cart-item[data-code="${'0' + flour.code}"] .cart-rm`);
  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX 9,800');
  assert.equal(await page.textContent('#cartCnt'), '2');

  // Reload: badge and cart persist from localStorage
  await page.reload();
  await page.waitForFunction(() => document.getElementById('cartCnt').textContent === '2');
  await page.click('#cartBtn');
  await page.waitForSelector('.cart-list');
  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX 9,800');

  // Clear cart needs confirmation
  await page.click('.cart-clr');
  await page.waitForSelector('.cart-confirm');
  await page.click('.cart-confirm button:has-text("Cancel")');
  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX 9,800');
  await page.click('.cart-clr');
  await page.click('.cart-confirm button:has-text("Yes, clear cart")');
  await page.waitForFunction(() => /Your cart is empty/.test(document.querySelector('.sheet-card').textContent));
  assert.equal(await page.isHidden('#cartCnt'), true);

  assert.deepEqual(problems, []);
  await ctx.close();
});

test('live SSE price change reprices the cart and shows a "Price changed" marker', async () => {
  const { ctx, page } = await phone();
  const salt = demoItem('Salt');
  const beans = demoItem('Beans');

  await look(page, salt.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('.sheet-card .close');

  await look(page, beans.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('text=View cart');
  await page.waitForSelector('.cart-list');
  const beforeTot = (salt.price + beans.price).toLocaleString('en-US');
  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX ' + beforeTot);

  // Admin changes Salt price live
  const newSalt = salt.price + 500;
  const r = await admin('PUT', `products/${salt.code}/price`, { price: newSalt });
  assert.equal(r.status, 200, r.text);
  const afterTot = (newSalt + beans.price).toLocaleString('en-US');
  await page.waitForFunction((t) => {
    const e = document.querySelector('.cart-tot-amt');
    return e && e.textContent === 'UGX ' + t;
  }, afterTot, { timeout: 4000 });
  const saltRow = await page.textContent(`.cart-item[data-code="${'0' + salt.code}"]`);
  assert.match(saltRow, /Price changed/);
  assert.match(saltRow, new RegExp('was UGX ' + salt.priceText));
  await ctx.close();
});

test('offer starting and ending reprices the cart automatically with a marker', async () => {
  const { ctx, page } = await phone();
  const juice = demoItem('Juice'); // 5,200

  await look(page, juice.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('text=View cart');
  await page.waitForSelector('.cart-tot-amt');
  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX ' + juice.priceText);

  // Create a short 4-second offer
  const now = Math.floor(Date.now() / 1000);
  const r = await admin('POST', 'promos', {
    title: 'Quick juice deal', starts_at: now - 2, ends_at: now + 4, enabled: true,
    items: [{ code: juice.code, price: 4000 }],
  });
  assert.equal(r.status, 201, r.text);

  await page.waitForFunction(() => document.querySelector('.cart-tot-amt')?.textContent === 'UGX 4,000', null, { timeout: 4000 });
  assert.match(await page.textContent('.cart-list'), /Price changed/);

  // Wait for the offer to expire by itself (no network event)
  await page.waitForFunction((t) => document.querySelector('.cart-tot-amt')?.textContent === 'UGX ' + t, juice.priceText, { timeout: 9000 });
  assert.match(await page.textContent('.cart-list'), /was UGX 4,000/);
  await ctx.close();
});

test('offline cart works from saved prices, labels price age, marks total outdated, and catches up online', async () => {
  const { ctx, page } = await phone();
  await page.waitForFunction(() => navigator.serviceWorker && navigator.serviceWorker.controller, null, { timeout: 8000 });
  const paste = demoItem('Tomato paste');

  await look(page, paste.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('.sheet-card .close');

  // Go offline and reload (served by SW)
  await ctx.setOffline(true);
  await page.reload();
  await page.waitForFunction(() => document.getElementById('cartCnt').textContent === '1');
  await page.click('#cartBtn');
  await page.waitForSelector('.cart-list');

  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX ' + paste.priceText);
  assert.match(await page.textContent('.cart-age'), /Saved .*offline/);
  assert.match(await page.textContent('.cart-warn'), /No connection.*may be outdated/);
  assert.ok(await page.$('.cart-tot.stale'));

  // Shopper adjusts quantity while offline
  await page.click('.cart-list button[aria-label^="Increase quantity"]');
  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX ' + (paste.price * 2).toLocaleString('en-US'));

  // Price changes on server while phone is offline; reconnect catches up
  const np = paste.price + 400;
  await admin('PUT', `products/${paste.code}/price`, { price: np });
  await ctx.setOffline(false);
  await page.waitForFunction((t) => document.querySelector('.cart-tot-amt')?.textContent === 'UGX ' + t, (np * 2).toLocaleString('en-US'), { timeout: 8000 });
  assert.match(await page.textContent('.cart-list'), /Price changed/);
  assert.equal(await page.$('.cart-tot.stale'), null);
  await ctx.close();
});

test('unknown and deleted products show "Price unknown" and are excluded from total with a warning', async () => {
  const { ctx, page } = await phone();
  const pasta = demoItem('Spaghetti');
  const bleach = demoItem('Bleach');
  const unknownCode = '5901234123457';

  await look(page, pasta.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('.sheet-card .close');

  await look(page, bleach.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('.sheet-card .close');

  // Unknown barcode
  await look(page, unknownCode);
  await page.waitForSelector('button:has-text("Add to cart (price unknown)")');
  await page.click('button:has-text("Add to cart (price unknown)")');
  await page.waitForSelector('.cart-list');

  const expected1 = (pasta.price + bleach.price).toLocaleString('en-US');
  assert.equal(await page.textContent('.cart-tot-amt'), 'UGX ' + expected1);
  assert.match(await page.textContent('.sheet-card'), /1 item has an unknown price and is excluded from the total/);
  assert.match(await page.textContent(`.cart-item[data-code="${'0' + unknownCode}"]`), /Price unknown/);

  // Live delete of Bleach excludes it and updates warning count to 2
  const dr = await admin('DELETE', `products/${bleach.code}`);
  assert.equal(dr.status, 200);
  await page.waitForFunction((t) => document.querySelector('.cart-tot-amt')?.textContent === 'UGX ' + t, pasta.priceText, { timeout: 4000 });
  assert.match(await page.textContent('.sheet-card'), /2 items have unknown prices and are excluded from the total/);
  assert.match(await page.textContent(`.cart-item[data-code="${'0' + bleach.code}"]`), /no longer listed/);
  await ctx.close();
});

test('cross-tab cart sync and narrow 320 px layout have no horizontal overflow', async () => {
  const { ctx, page } = await phone({ width: 320, height: 640 });
  const page2 = await ctx.newPage();
  await page2.goto(srv.url + '/s/demo/');
  await page2.waitForFunction(() => document.getElementById('live').classList.contains('on'));

  const tp = demoItem('Toothpaste');
  await look(page, tp.code);
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('text=View cart');
  await page.waitForSelector('.cart-list');

  // Second tab sees the updated badge via storage event
  await page2.waitForFunction(() => document.getElementById('cartCnt').textContent === '1', null, { timeout: 4000 });

  // No sideways scroll on narrow 320 px screen
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
  assert.equal(overflow, false, 'cart view overflows 320px viewport');
  await ctx.close();
});
