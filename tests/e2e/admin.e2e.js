'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { startServer, launch, demoItem, withCheck, wait } = require('./helpers.js');

let srv, browser;
test.before(async () => { srv = await startServer(); browser = await launch(); });
test.after(async () => { await browser.close(); await srv.stop(); });

async function adminPage(email = 'owner@demo.example', password = 'demo-password-1') {
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 800 } });
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (['error', 'warning'].includes(m.type()) && !/401/.test(m.text())) problems.push(m.text()); });
  page.on('pageerror', (e) => problems.push(e.message));
  await page.goto(srv.url + '/s/demo/admin/');
  await page.fill('#email', email); await page.fill('#pass', password);
  await page.click('button[type=submit]');
  await page.waitForSelector('nav');
  return { ctx, page, problems };
}
async function shopper() {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  await page.goto(srv.url + '/s/demo/');
  await page.waitForFunction(() => document.getElementById('live').classList.contains('on'));
  return { ctx, page };
}
const waitAmt = (page, t, ms = 4000) => page.waitForFunction((x) => { const e = document.querySelector('.sheet-card .amt'); return e && e.textContent === x; }, t, { timeout: ms });

test('sign-in: wrong password is refused politely, the right one opens the admin, sign-out closes it; the cookie is invisible to scripts', async () => {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(srv.url + '/s/demo/admin/');
  await page.fill('#email', 'owner@demo.example'); await page.fill('#pass', 'wrong-password-1');
  await page.click('button[type=submit]');
  await page.waitForSelector('.msg.bad');
  assert.match(await page.textContent('.msg.bad'), /wrong e-mail or password/i);
  await page.fill('#pass', 'demo-password-1'); await page.click('button[type=submit]');
  await page.waitForSelector('nav');
  assert.equal(await page.evaluate(() => document.cookie), '', 'session cookie must be HttpOnly');
  const cookies = await ctx.cookies();
  assert.ok(cookies.find((c) => c.name === 'pt_demo' && c.httpOnly && c.sameSite === 'Strict'));
  await page.click('header button');
  await page.waitForSelector('#email');
  await page.reload(); await page.waitForSelector('#email');           // really signed out
  await ctx.close();
});

test('changing a price inline reaches a shopper\'s phone, and says how many phones saw it', async () => {
  const a = await adminPage(), s = await shopper();
  const item = demoItem('Salt');
  await s.page.fill('#q', item.code); await s.page.click('#find button'); await s.page.waitForSelector('.sheet-card .amt');
  await a.page.fill('input[type=search]', 'salt');
  await a.page.waitForFunction(() => document.querySelectorAll('.item').length === 1 && /Salt/.test(document.querySelector('.item').textContent));
  await a.page.click('.item .price button');
  await a.page.fill('.edit input', '1,350');
  await a.page.click('.edit .primary');
  await waitAmt(s.page, '1,350');
  await a.page.waitForFunction(() => /Price saved\. \d+ phones? (is|are) connected/.test(document.getElementById('toast').textContent));
  assert.equal(await a.page.textContent('.item .price button'), '1,350');
  assert.deepEqual(a.problems, []);
  await a.ctx.close(); await s.ctx.close();
});

test('bad input is rejected with plain messages: a mistyped barcode, a non-number price', async () => {
  const a = await adminPage();
  await a.page.click('text=Add product');
  await a.page.fill('.modal input[placeholder^="Scan or type"]', '2000000000014');
  assert.match(await a.page.textContent('.modal .err'), /last digit does not match/);
  await a.page.fill('.modal input[placeholder^="Scan or type"]', withCheck('299000000001'));
  await a.page.fill('.modal input:nth-of-type(1) >> nth=1', 'Test biscuits');
  const price = a.page.locator('.modal input[inputmode=decimal]');
  await price.fill('twelve');
  await a.page.click('.modal .primary');
  assert.match(await a.page.textContent('.modal .msg.bad'), /Enter the price as a number/);
  await price.fill('2,750');
  await a.page.click('.modal .primary');
  await a.page.waitForSelector('.modal', { state: 'detached' });
  const s = await shopper();
  await s.page.fill('#q', withCheck('299000000001')); await s.page.click('#find button');
  await waitAmt(s.page, '2,750');
  await a.ctx.close(); await s.ctx.close();
});

test('creating an offer in the admin puts it on the shop home page and on the item price, live', async () => {
  const a = await adminPage(), s = await shopper();
  await a.page.click('nav a[href="#offers"]');
  await a.page.click('text=New offer');
  await a.page.fill('input[placeholder^="for example Weekend"]', 'Payday sale');
  await a.page.fill('input[placeholder="Search products to add"]', 'toothpaste');
  await a.page.click('.list .item button:has-text("Add")');
  const inputs = a.page.locator('.item .edit input');
  await inputs.first().fill('3,900');
  await a.page.click('text=Create offer');
  await a.page.waitForSelector('text=Payday sale');
  await s.page.waitForFunction(() => /Payday sale/.test(document.getElementById('offers').textContent), null, { timeout: 4000 });
  const tp = demoItem('Toothpaste');
  await s.page.fill('#q', tp.code); await s.page.click('#find button');
  await waitAmt(s.page, '3,900');
  // end it now: back to the normal price on the phone
  await a.page.click('.item:has-text("Payday sale") >> text=End now');
  await waitAmt(s.page, tp.priceText, 5000);
  assert.deepEqual(a.problems, []);
  await a.ctx.close(); await s.ctx.close();
});

test('offer validation: an offer price that is not lower is refused with a clear reason', async () => {
  const a = await adminPage();
  await a.page.click('nav a[href="#offers"]');
  await a.page.click('text=New offer');
  await a.page.fill('input[placeholder^="for example Weekend"]', 'Bad deal');
  await a.page.fill('input[placeholder="Search products to add"]', 'bleach');
  await a.page.click('.list .item button:has-text("Add")');
  await a.page.locator('.item .edit input').first().fill('99,999');
  assert.match(await a.page.textContent('.item .err'), /must be lower/);
  await a.page.click('text=Create offer');
  await a.page.waitForSelector('.msg.bad');
  assert.match(await a.page.textContent('.msg.bad'), /must be lower than its normal price/);
  await a.ctx.close();
});

test('import: preview first (nothing saved), then apply; problems are listed by row', async () => {
  const a = await adminPage(), s = await shopper();
  const salt = demoItem('Charcoal stove lighter');
  const csv = ['Barcode,Item Description,Selling Price', `${salt.code},Charcoal stove lighter,"UGX 2,222"`, `${withCheck('299000000018')},Imported thing,1500`, '4006381333932,Misread code,100'].join('\n');
  const f = path.join(os.tmpdir(), 'pt-import-' + Date.now() + '.csv'); fs.writeFileSync(f, csv);
  await a.page.click('nav a[href="#import"]');
  await a.page.setInputFiles('input[type=file]', f);
  await a.page.waitForSelector('.stats');
  assert.match(await a.page.textContent('.msg.warn'), /Preview only/);
  const stats = await a.page.$$eval('.stat', (n) => n.map((x) => x.textContent));
  assert.deepEqual(stats, ['1new', '1changed', '0unchanged', '1problems']);
  assert.match(await a.page.textContent('table'), /check digit is wrong/);
  // nothing was saved by the preview
  const r = await fetch(srv.url + '/api/v1/s/demo/items/' + withCheck('299000000018'));
  assert.equal(r.status, 404);
  await a.page.click('text=Apply these changes');
  await a.page.waitForSelector('.msg.ok');
  await s.page.fill('#q', withCheck('299000000018')); await s.page.click('#find button');
  await waitAmt(s.page, '1,500');
  await a.ctx.close(); await s.ctx.close(); fs.rmSync(f);
});

test('team: owner adds an editor (generated password); the editor cannot see Team or Integrations', async () => {
  const a = await adminPage();
  await a.page.click('nav a[href="#team"]');
  await a.page.fill('input[type=email]', 'till@demo.example');
  await a.page.locator('.card input[type=text]').first().fill('Till Operator');
  await a.page.click('text=Add team member');
  await a.page.waitForSelector('.msg.ok');
  const pw = (await a.page.textContent('.msg.ok')).match(/password is: (\S+)/)[1];
  const e = await adminPage('till@demo.example', pw);
  const nav = await e.page.$$eval('nav a', (n) => n.map((x) => x.textContent));
  assert.ok(!nav.includes('Team') && !nav.includes('Integrations'), nav.join());
  assert.ok(nav.includes('Prices') && nav.includes('Offers'));
  const direct = await e.page.evaluate(() => fetch('/api/v1/s/demo/admin/users').then((r) => r.status));
  assert.equal(direct, 403);
  await a.ctx.close(); await e.ctx.close();
});

test('integration key: shown once, works for pushing prices, can be revoked', async () => {
  const a = await adminPage();
  await a.page.click('nav a[href="#keys"]');
  await a.page.fill('input[placeholder^="for example Nightly"]', 'Till export');
  await a.page.click('text=Create key');
  await a.page.waitForSelector('.msg.warn');
  const token = await a.page.textContent('.copy >> nth=0');
  assert.match(token, /^ptk_/);
  assert.match(await a.page.textContent('.copy >> nth=1'), /curl -X POST .*\/ingest/);
  const put = (body) => fetch(srv.url + '/api/v1/s/demo/ingest', { method: 'POST', headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const wheat = demoItem('Wheat flour');
  let r = await put({ products: [{ code: wheat.code, name: 'Wheat flour', size: '2 kg', price: 8800 }] });
  assert.equal(r.status, 200);
  await a.page.reload(); await a.page.click('nav a[href="#keys"]').catch(() => { });
  await a.page.waitForSelector('.item:has-text("Till export")');
  assert.ok(!(await a.page.content()).includes(token), 'the key must not be shown again');
  await a.page.click('.item:has-text("Till export") >> text=Revoke');
  await a.page.click('.modal .danger');
  await a.page.waitForSelector('.item:has-text("Revoked")');
  r = await put({ products: [] });
  assert.equal(r.status, 401);
  await a.ctx.close();
});

test('settings: renaming the shop updates every phone live; the entrance-poster QR code is drawn', async () => {
  const a = await adminPage(), s = await shopper();
  await a.page.click('nav a[href="#settings"]');
  assert.ok(await a.page.waitForSelector('.poster svg'));
  assert.ok((await a.page.evaluate(() => document.querySelector('.poster svg').outerHTML.length)) > 800, 'QR code drawn');
  await a.page.fill('.card input[type=text] >> nth=0', 'Demo Mart Renamed');
  await a.page.fill('.card textarea', 'Closed on Sunday');
  await a.page.click('.card .primary >> text=Save');
  await s.page.waitForFunction(() => document.getElementById('storeName').textContent === 'Demo Mart Renamed' && /Closed on Sunday/.test(document.getElementById('announce').textContent), null, { timeout: 4000 });
  assert.match(await a.page.textContent('.card:has-text("Currency")'), /fixed once you have products/);
  await a.ctx.close(); await s.ctx.close();
});

test('activity shows who changed what; per-product history shows the price trail', async () => {
  const a = await adminPage();
  await a.page.click('nav a[href="#activity"]');
  await a.page.waitForSelector('select[aria-label="Type of activity"]');
  await a.page.waitForSelector('.list .item');
  const txt = await a.page.textContent('.list');
  assert.match(txt, /owner@demo\.example/);
  assert.match(txt, /Price changed: Salt/);
  await a.page.click('nav a[href="#prices"]');
  await a.page.fill('input[type=search]', 'salt');
  await a.page.waitForFunction(() => document.querySelectorAll('.item').length === 1);
  await a.page.click('.item >> text=History');
  await a.page.waitForSelector('.modal table');
  assert.match(await a.page.textContent('.modal table'), /→/);
  await a.ctx.close();
});

test('database export: the owner can download everything and it is a real SQLite file', async () => {
  const a = await adminPage();
  await a.page.click('nav a[href="#import"]');
  const [dl] = await Promise.all([a.page.waitForEvent('download'), a.page.click('text=Download everything')]);
  const p = await dl.path();
  assert.equal(fs.readFileSync(p).subarray(0, 15).toString(), 'SQLite format 3');
  const [dl2] = await Promise.all([a.page.waitForEvent('download'), a.page.click('text=Download price list')]);
  assert.match(fs.readFileSync(await dl2.path(), 'utf8'), /^code,name,size,category,price/);
  await a.ctx.close();
});
