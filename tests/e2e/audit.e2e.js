'use strict';
// Stage 3: Theft / audit mode (spot-check recheck, mismatch logging, owner Audit Report & CSV).
const test = require('node:test');
const assert = require('node:assert/strict');
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
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 800 }, acceptDownloads: true });
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (['error', 'warning'].includes(m.type()) && !/401|403|404|409/.test(m.text())) problems.push(m.text()); });
  page.on('pageerror', (e) => problems.push(e.message));
  await page.goto(srv.url + '/s/demo/admin/');
  await page.fill('#email', email);
  await page.fill('#pass', password);
  await page.click('button[type=submit]');
  await page.waitForSelector('nav');
  return { ctx, page, problems };
}

async function createCodeFromShopper(page, code) {
  await page.fill('#q', code);
  await page.click('#find button');
  await page.waitForSelector('.cart-add');
  await page.click('.cart-add');
  await page.click('.sheet-card .close');
  await page.click('#cartBtn');
  await page.waitForSelector('.co-btn');
  const [resp] = await Promise.all([
    page.waitForResponse((r) => r.url().includes('/checkouts') && r.request().method() === 'POST'),
    page.click('.co-btn'),
  ]);
  const rawBody = await resp.text();
  await page.waitForSelector('.co-code');
  const checkoutCode = await page.getAttribute('.co-code', 'data-code');
  return { checkoutCode, rawBody };
}

test('flagged spot-check hides tick-off, requires rescan confirmation, logs mismatch, and updates Audit Report + CSV (owner only)', async () => {
  const flour = demoItem('Wheat flour');
  const salt = demoItem('Salt');

  // Owner enables checkout + 100% spot-check audit in Settings.
  const owner = await staffPage('owner@demo.example', 'demo-password-1');
  await owner.page.click('nav a[href="#settings"]');
  await owner.page.waitForSelector('#auditEnabled');
  const settingsText = await owner.page.textContent('main');
  assert.match(settingsText, /catches more un-scanned items, but slows down the till/i);
  assert.match(settingsText, /repeat offenders cannot be tracked/i);
  assert.equal(await owner.page.isChecked('#auditEnabled'), false, 'audit must be off by default');
  await owner.page.check('#coEnabled');
  await owner.page.check('#auditEnabled');
  await owner.page.fill('#auditRate', '100');
  await owner.page.click('.card .primary >> text=Save');
  await owner.page.waitForFunction(() => /Saved/.test(document.getElementById('toast').textContent));

  // Shopper #1 creates a checkout code; shopper response must NOT leak audit flag.
  const s1 = await shopper();
  const c1 = await createCodeFromShopper(s1.page, flour.code);
  assert.equal(/audit|recheck/i.test(c1.rawBody), false, 'shopper checkout response must not reveal audit flag');
  assert.equal(/spot-check|audit|rescan/i.test(await s1.page.textContent('.sheet-card')), false, 'shopper UI must not reveal audit flag');

  // Cashier (editor role) looks up code #1.
  const clerk = await staffPage('clerk@demo.example', 'demo-password-2');
  // Editor must NOT see the owner-only Audit report nav link.
  assert.equal(await clerk.page.$('nav a[href="#audit"]'), null, 'editor must not see Audit report tab');
  await clerk.page.click('nav a[href="#checkout"]');
  await clerk.page.fill('.co-input', c1.checkoutCode);
  await clerk.page.click('button:has-text("Look up")');
  await clerk.page.waitForSelector('.co-audit');

  assert.match(await clerk.page.textContent('.co-audit'), /Spot-check: Rescan everything/);
  assert.equal(await clerk.page.$('.co-modes'), null, 'mode switcher must be hidden on flagged order');
  assert.equal((await clerk.page.$$('.co-row .co-chk')).length, 0, 'tick-off checkboxes must be hidden on flagged order');
  assert.equal(await clerk.page.isDisabled('button:has-text("Mark verified / paid")'), true, 'verify must be disabled until rescan confirmed');

  // Cashier logs a mismatch: 2x Salt (barcoded extra), 1x free-text extra, 1x missing flour, and formula-like note.
  await clerk.page.click('#mmToggle');
  await clerk.page.waitForSelector('#mmBox');
  await clerk.page.fill('#mmExtraCode', salt.code);
  await clerk.page.fill('#mmExtraQty', '2');
  await clerk.page.click('#mmAddExtra');
  await clerk.page.fill('#mmExtraCode', '');
  await clerk.page.fill('#mmExtraDesc', 'Loose bakery bun');
  await clerk.page.fill('#mmExtraQty', '1');
  await clerk.page.click('#mmAddExtra');
  await clerk.page.click('#mmAddMiss');
  await clerk.page.fill('#mmNote', '=SUM(1,2) bottom of trolley');

  // Confirm full rescan -> verify button unlocks and succeeds.
  await clerk.page.check('#rescanConfirm');
  assert.equal(await clerk.page.isDisabled('button:has-text("Mark verified / paid")'), false);
  await clerk.page.click('button:has-text("Mark verified / paid")');
  await clerk.page.waitForSelector('.msg.ok');
  assert.match(await clerk.page.textContent('.msg.ok'), /Mismatch logged to the audit report/);

  // Activity trail records the spot-check and mismatch.
  await clerk.page.click('nav a[href="#activity"]');
  await clerk.page.selectOption('select[aria-label="Type of activity"]', 'checkout');
  await clerk.page.waitForFunction((c) => (document.querySelector('.list')?.textContent || '').includes(c), c1.checkoutCode);
  const actTxt = await clerk.page.textContent('.list');
  assert.match(actTxt, /spot-check/);
  assert.match(actTxt, /MISMATCH: 3 extra, 1 missing/);

  // Editor direct access to #audit shows owner-only refusal and API returns 403.
  const editorApiStatus = await clerk.page.evaluate(async () => (await fetch('/api/v1/s/demo/admin/audit-report')).status);
  assert.equal(editorApiStatus, 403);

  // Shopper #2 creates a second flagged checkout and cashier verifies it cleanly (no mismatch).
  const s2 = await shopper();
  const c2 = await createCodeFromShopper(s2.page, flour.code);
  await clerk.page.click('nav a[href="#checkout"]');
  await clerk.page.fill('.co-input', c2.checkoutCode);
  await clerk.page.click('button:has-text("Look up")');
  await clerk.page.waitForSelector('#rescanConfirm');
  await clerk.page.check('#rescanConfirm');
  await clerk.page.click('button:has-text("Mark verified / paid")');
  await clerk.page.waitForSelector('.msg.ok');

  // Owner views Audit Report: 2 verified, 2 spot-checked, 1 mismatch -> 50.0%, missed = 2 * salt.price + 1 unpriced.
  await owner.page.click('nav a[href="#audit"]');
  await owner.page.waitForSelector('#audTotal');
  assert.equal(await owner.page.textContent('#audTotal'), '2');
  assert.equal(await owner.page.textContent('#audRechecked'), '2');
  assert.equal(await owner.page.textContent('#audMismatches'), '1');
  assert.equal(await owner.page.textContent('#audRate'), '50.0%');
  assert.equal(await owner.page.textContent('#audMissed'), 'UGX ' + (salt.price * 2).toLocaleString('en-US'));
  const reportText = await owner.page.textContent('main');
  assert.match(reportText, /repeat offenders cannot be tracked/i);
  assert.match(reportText, new RegExp(c1.checkoutCode));
  assert.match(reportText, /\+1 unpriced/);

  // Download CSV and verify formula guard + contents.
  const csvHref = await owner.page.getAttribute('#auditCsv', 'href');
  const csvText = await owner.page.evaluate(async (u) => (await fetch(u)).text(), csvHref);
  assert.match(csvText, new RegExp(c1.checkoutCode));
  assert.match(csvText, /'=SUM\(1,2\) bottom of trolley/);
  assert.match(csvText, /Loose bakery bun/);

  // Filtering to an older date range returns 0 checkouts.
  await owner.page.fill('#auditFrom', '2020-01-01');
  await owner.page.fill('#auditTo', '2020-01-31');
  await owner.page.click('button:has-text("Apply")');
  await owner.page.waitForFunction(() => document.getElementById('audTotal')?.textContent === '0');
  assert.match(await owner.page.textContent('main'), /No mismatches logged in this date range/);

  assert.deepEqual(s1.problems, []);
  assert.deepEqual(s2.problems, []);
  assert.deepEqual(clerk.problems, []);
  assert.deepEqual(owner.problems, []);
  await s1.ctx.close();
  await s2.ctx.close();
  await clerk.ctx.close();
  await owner.ctx.close();
});
