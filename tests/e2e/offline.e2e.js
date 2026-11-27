'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { startServer, adminApi, launch, demoCode, demoItem, wait, until } = require('./helpers.js');

let srv, browser, admin;
test.before(async () => { srv = await startServer(); browser = await launch(); admin = await adminApi(srv.url); });
test.after(async () => { await browser.close(); await srv.stop(); });

const price = (page) => page.textContent('.sheet-card .amt');
const waitPrice = (page, text, ms = 12000) => page.waitForFunction((t) => { const e = document.querySelector('.sheet-card .amt'); return e && e.textContent === t; }, text, { timeout: ms, polling: 'raf' });
async function ready(page) {
  await page.goto(srv.url + '/s/demo/');
  await page.waitForFunction(() => document.getElementById('live').classList.contains('on'));
  await page.waitForFunction(() => navigator.serviceWorker && navigator.serviceWorker.controller, null, { timeout: 8000 });
}
async function look(page, code) {
  await page.fill('#q', code); await page.click('#find button'); await page.waitForSelector('.sheet-card');
}

test('already-seen prices still work with no connection, and the app itself opens offline', async () => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  await ready(page);
  const flour = demoItem('Wheat flour');
  await look(page, flour.code);                                        // cached by looking at it
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await price(page), flour.priceText);
  await page.click('.sheet-card .close');

  await ctx.setOffline(true);
  await page.reload();                                                  // served by the service worker
  await page.waitForSelector('#storeName');
  assert.equal(await page.textContent('#storeName'), 'Price Tag');
  await page.waitForSelector('.promo');                                 // home page from the saved copy
  assert.match(await page.textContent('#notice'), /No connection/);

  await look(page, flour.code);
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await price(page), flour.priceText);
  assert.match(await page.textContent('#metaText'), /Saved .*No connection/);

  await page.click('.sheet-card .close');
  await look(page, demoItem('Honey').code);                             // never seen before
  await page.waitForSelector('.big');
  assert.match(await page.textContent('.big'), /No connection/);
  await ctx.close();
});

test('prices changed while the phone was offline are fixed the moment it reconnects (one catch-up message)', async () => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  await ready(page);
  const paste = demoItem('Tomato paste'), code = paste.code;
  await look(page, code);
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await price(page), paste.priceText);
  await ctx.setOffline(true);
  await page.waitForFunction(() => !document.getElementById('live').classList.contains('on'));
  for (const p of [1600, 1700, 1800]) await admin('PUT', `products/${code}/price`, { price: p });
  await wait(300);
  assert.equal(await price(page), paste.priceText);                      // offline: still the old one, honestly labelled
  assert.match(await page.textContent('#metaText'), /No connection|Saved/);
  const t0 = Date.now();
  await ctx.setOffline(false);
  await waitPrice(page, '1,800');
  console.log(`      back online -> correct price on screen in ${Date.now() - t0} ms`);
  assert.ok(Date.now() - t0 < 4000);
  await ctx.close();
});

test('the whole price list can be saved for offline use; unseen products and name search then work with no signal', async () => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  await ready(page);
  await page.click('#menuBtn');
  await page.click('.menu .btn:not(.ghost)');
  await page.waitForFunction(() => /Done\. Prices now work/.test(document.querySelector('.menu .sub').textContent), null, { timeout: 15000 });
  assert.equal(await page.evaluate(() => window.__pt.state.full), true);
  await page.click('.sheet-card .close');

  await ctx.setOffline(true);
  const crisps = demoItem('Crisps');
  await look(page, crisps.code);                                         // never opened before
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await price(page), crisps.priceText);
  await page.click('.sheet-card .close');
  await page.fill('#q', 'toothpaste'); await page.click('#find button');   // local name search
  await page.waitForSelector('.sheet-card .row');
  assert.match(await page.textContent('.sheet-card'), /Toothpaste/);
  await page.click('.sheet-card .close');

  // a change made while offline arrives, and is applied to the saved list, on reconnect
  await admin('PUT', `products/${crisps.code}/price`, { price: 3300 });
  await ctx.setOffline(false);
  await page.waitForFunction(() => document.getElementById('live').classList.contains('on'), null, { timeout: 8000 });
  await wait(500);
  await look(page, crisps.code);
  await waitPrice(page, '3,300', 5000);
  await ctx.close();
});

test('the server restarting is invisible to phones: they reconnect on their own and catch up', async () => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  await ready(page);
  const code = demoItem('Bleach').code;
  await look(page, code);
  await page.waitForSelector('.sheet-card .amt');
  const before = await price(page);
  await srv.restart(300);
  admin = await adminApi(srv.url);                                       // new session after restart (sessions persist, but be explicit)
  const np = (+before.replace(/,/g, '')) + 250;
  await admin('PUT', `products/${code}/price`, { price: np });
  const t0 = Date.now();
  await waitPrice(page, np.toLocaleString('en-US'), 20000);
  console.log(`      server restart -> phone reconnected and showed the new price after ${Date.now() - t0} ms`);
  await ctx.close();
});

test('restoring the shop from an OLDER backup: phones that were ahead notice, discard stale prices and show the restored data', async () => {
  const { execFileSync } = require('node:child_process');
  const fs = require('node:fs'); const path = require('node:path');
  const { BIN } = require('./helpers.js');
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  await ready(page);
  const item = demoItem('Hair oil'), code = item.code;
  await look(page, code);
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await price(page), item.priceText);

  const backup = path.join(srv.dir, 'older-copy.db');
  execFileSync(BIN, ['store', 'backup', '-data', srv.dir, '-slug', 'demo', '-out', backup]);   // the "yesterday" backup
  admin = await adminApi(srv.url);
  await admin('PUT', `products/${code}/price`, { price: item.price + 700 });
  await waitPrice(page, (item.price + 700).toLocaleString('en-US'));                              // phone is now AHEAD of the backup

  await srv.restart(200, async () => {                                                            // disaster: restore yesterday's file
    for (const ext of ['', '-wal', '-shm']) fs.rmSync(path.join(srv.dir, 'demo.db' + ext), { force: true });
    fs.copyFileSync(backup, path.join(srv.dir, 'demo.db'));
  });

  await page.waitForFunction(() => document.getElementById('live').classList.contains('on'), null, { timeout: 15000 });
  await wait(500);
  await page.click('.sheet-card .close').catch(() => { });
  await look(page, code);
  await page.waitForSelector('.sheet-card .amt');
  assert.equal(await price(page), item.priceText, 'must show the restored price, not the newer one cached on the phone');
  await ctx.close();
});
