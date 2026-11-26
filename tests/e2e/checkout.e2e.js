'use strict';
// Stage 2: Cart code for the till (shopper checkout + cashier verification).
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { startServer, adminApi, launch, demoItem } = require('./helpers.js');

let srv, browser, ownerApi;
test.before(async () => { srv = await startServer(); browser = await launch(); ownerApi = await adminApi(srv.url); });
test.after(async () => { await browser.close(); await srv.stop(); });

async function shopper() {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (['error', 'warning'].includes(m.type())) problems.push(m.type() + ': ' + m.text()); });
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message));
  await page.goto(srv.url + '/s/demo/');
  await page.waitForFunction(() => document.getElementById('live').classList.contains('on'));
  return { ctx, page, problems };
}

async function staffPage(email, password) {
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 800 } });
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (['error', 'warning'].includes(m.type()) && !/401|404|409/.test(m.text())) problems.push(m.text()); });
  page.on('pageerror', (e) => problems.push(e.message));
  await page.goto(srv.url + '/s/demo/admin/');
  await page.fill('#email', email);
  await page.fill('#pass', password);
  await page.click('button[type=submit]');
  await page.waitForSelector('nav');
  return { ctx, page, problems };
}

async function addItem(page, code) {
  await page.fill('#q', code);
  await page.click('#find button');
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('.sheet-card .close');
}

test('checkout is off by default; enabling in Settings shows Checkout button, creates code + QR, reuses unchanged cart, creates new on change', async () => {
  const s = await shopper();
  const flour = demoItem('Wheat flour');
  const salt = demoItem('Salt');

  await addItem(s.page, flour.code);
  await s.page.click('#cartBtn');
  await s.page.waitForSelector('.cart-list');
  assert.equal(await s.page.$('.co-btn'), null, 'Checkout button must be hidden when disabled');
  assert.equal(await s.page.evaluate(() => !!window.qrcode), false, 'QR lib must not load before Checkout');

  // Owner enables checkout in Settings
  const o = await staffPage('owner@demo.example', 'demo-password-1');
  await o.page.click('nav a[href="#settings"]');
  await o.page.waitForSelector('#coEnabled');
  assert.equal(await o.page.isChecked('#coEnabled'), false);
  await o.page.check('#coEnabled');
  await o.page.click('.card .primary >> text=Save');
  await o.page.waitForFunction(() => /Saved/.test(document.getElementById('toast').textContent));

  // Shopper gets Checkout button live (via SSE home update)
  await s.page.waitForSelector('.co-btn', { timeout: 5000 });
  await s.page.click('.co-btn');
  await s.page.waitForSelector('.co-code');
  await s.page.waitForSelector('.co-qr svg');

  const code1 = await s.page.getAttribute('.co-code', 'data-code');
  assert.match(code1, /^[2-9A-HJ-NP-Z]{6}$/, 'code must use unambiguous 6-char alphabet: ' + code1);
  assert.ok((await s.page.evaluate(() => document.querySelector('.co-qr svg').outerHTML.length)) > 400);

  // Back to cart and re-tap Checkout on unchanged cart -> returns exact same code
  await s.page.click('text=Back to cart');
  await s.page.waitForSelector('.co-btn');
  await s.page.click('.co-btn');
  await s.page.waitForSelector('.co-code');
  assert.equal(await s.page.getAttribute('.co-code', 'data-code'), code1);

  // Change cart -> new code created
  await s.page.click('text=Back to cart');
  await s.page.click('.sheet-card .close');
  await addItem(s.page, salt.code);
  await s.page.click('#cartBtn');
  await s.page.waitForSelector('.co-btn');
  await s.page.click('.co-btn');
  await s.page.waitForSelector('.co-code');
  const code2 = await s.page.getAttribute('.co-code', 'data-code');
  assert.notEqual(code2, code1, 'changed cart must create a new code');

  assert.deepEqual(s.problems, []);
  assert.deepEqual(o.problems, []);
  await s.ctx.close();
  await o.ctx.close();
});

test('cashier (editor role) sees price changes, ticks items off, verifies order, blocks reuse, and logs audit trail', async () => {
  const s = await shopper();
  const sugar = demoItem('Sugar', '1 kg'); // 4,900 offer
  const pasta = demoItem('Spaghetti');     // 3,500 normal

  await addItem(s.page, sugar.code);
  await addItem(s.page, pasta.code);
  await s.page.click('#cartBtn');
  await s.page.waitForSelector('.co-btn');
  await s.page.click('.co-btn');
  await s.page.waitForSelector('.co-code');
  const code = await s.page.getAttribute('.co-code', 'data-code');

  // Price changes after shopper created the code
  const newPasta = pasta.price + 600;
  const pr = await ownerApi('PUT', `products/${pasta.code}/price`, { price: newPasta });
  assert.equal(pr.status, 200);

  // Clerk (editor role) opens Checkout tab and looks up code
  const clerk = await staffPage('clerk@demo.example', 'demo-password-2');
  await clerk.page.click('nav a[href="#checkout"]');
  await clerk.page.fill('.co-input', code.toLowerCase());
  await clerk.page.click('button:has-text("Look up")');
  await clerk.page.waitForSelector('.co-card');

  // Both old and current prices are clearly shown
  const cardTxt = await clerk.page.textContent('.co-card');
  assert.match(cardTxt, /1 item changed price/);
  assert.match(cardTxt, new RegExp('Shopper saw UGX ' + pasta.priceText + ' → Now UGX ' + newPasta.toLocaleString('en-US')));
  assert.equal(await clerk.page.textContent('.co-tot-amt'), 'UGX ' + (4900 + newPasta).toLocaleString('en-US'));

  // Tick-off mode (default) lets cashier check items off one by one
  assert.equal((await clerk.page.$$('.co-chk')).length, 2);
  await clerk.page.click('.co-chk >> nth=0');
  assert.match(await clerk.page.textContent('.co-prog'), /Checked 1 of 2/);

  // Mark verified / paid
  await clerk.page.click('button:has-text("Mark verified / paid")');
  await clerk.page.waitForSelector('.msg.ok');
  assert.match(await clerk.page.textContent('.msg.ok'), new RegExp('Verified! Code ' + code));

  // Reusing the verified code is rejected
  await clerk.page.fill('.co-input', code);
  await clerk.page.click('button:has-text("Look up")');
  await clerk.page.waitForSelector('.msg.bad');
  assert.match(await clerk.page.textContent('.msg.bad'), /already been verified/i);

  // Audit trail records who verified and when
  await clerk.page.click('nav a[href="#activity"]');
  await clerk.page.selectOption('select[aria-label="Type of activity"]', 'checkout');
  await clerk.page.waitForFunction((c) => (document.querySelector('.list')?.textContent || '').includes(c), code);
  const actTxt = await clerk.page.textContent('.list');
  assert.match(actTxt, /clerk@demo\.example/);
  assert.match(actTxt, new RegExp(code));

  assert.deepEqual(clerk.problems, []);
  await s.ctx.close();
  await clerk.ctx.close();
});

test('per-shop rescan default mode works and expired/unknown codes show the same error', async () => {
  // Switch shop default mode to "rescan"
  const curSet = (await ownerApi('GET', 'settings')).json;
  const upd = await ownerApi('PUT', 'settings', { ...curSet, checkout_enabled: true, checkout_default_mode: 'rescan' });
  assert.equal(upd.status, 200);

  const s = await shopper();
  const beans = demoItem('Beans');
  await addItem(s.page, beans.code);
  await s.page.click('#cartBtn');
  await s.page.waitForSelector('.co-btn');
  await s.page.click('.co-btn');
  await s.page.waitForSelector('.co-code');
  const code = await s.page.getAttribute('.co-code', 'data-code');

  const clerk = await staffPage('clerk@demo.example', 'demo-password-2');
  await clerk.page.click('nav a[href="#checkout"]');
  await clerk.page.fill('.co-input', code);
  await clerk.page.click('button:has-text("Look up")');
  await clerk.page.waitForSelector('.co-card');

  // Opens directly in rescan mode (no checkboxes)
  assert.equal((await clerk.page.$$('.co-chk')).length, 0);
  assert.match(await clerk.page.textContent('.co-card'), /Rescan mode/);

  // Unknown code error text
  await clerk.page.fill('.co-input', 'ZZZZZZ');
  await clerk.page.click('button:has-text("Look up")');
  await clerk.page.waitForSelector('.msg.bad');
  const missErr = await clerk.page.textContent('.msg.bad');

  // Force the checkout code to expire in SQLite (via python3 sqlite3 module)
  execFileSync('python3', ['-c', `import sqlite3, sys; c=sqlite3.connect(sys.argv[1]); c.execute("UPDATE checkouts SET expires_at=1 WHERE code=?", (sys.argv[2],)); c.commit()`, path.join(srv.dir, 'demo.db'), code]);

  await clerk.page.fill('.co-input', code);
  await clerk.page.click('button:has-text("Look up")');
  await clerk.page.waitForSelector('.msg.bad');
  const expErr = await clerk.page.textContent('.msg.bad');
  assert.equal(expErr, missErr, 'expired and nonexistent codes must show the exact same error');

  await s.ctx.close();
  await clerk.ctx.close();
});
