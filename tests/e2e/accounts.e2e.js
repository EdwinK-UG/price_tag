'use strict';
// Shop registration -> approval -> owner set-up; shopper accounts (one account,
// consent per shop); owner customers, messages and shop-page sections.
const test = require('node:test');
const assert = require('node:assert/strict');
const { startServer, launch, demoItem, adminApi, wait } = require('./helpers.js');

const TOK = 'e2e-operator-token-0123456789abcdef';
let srv, browser;
test.before(async () => { srv = await startServer([], { PRICETAG_OPERATOR_TOKEN: TOK }); browser = await launch(); });
test.after(async () => { await browser.close(); await srv.stop(); });

async function op(method, path, body) {
  const r = await fetch(srv.url + '/api/v1/operator/' + path, { method, headers: { Authorization: 'Bearer ' + TOK, 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
  return r.json();
}
// The newest development-mode message to `to` (of `kind`), e.g. a sign-in code.
async function devMsg(to, kind) {
  for (let i = 0; i < 100; i++) {
    const st = await op('GET', 'status');
    const m = st.dev_messages.find((x) => x.to === to && (!kind || x.kind === kind));
    if (m) return m;
    await wait(50);
  }
  throw new Error('no message to ' + to);
}
async function phone(slug = 'demo') {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 780 } });
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (['error', 'warning'].includes(m.type()) && !/40[0139]/.test(m.text())) problems.push(m.text()); });
  page.on('pageerror', (e) => problems.push(e.message));
  await page.goto(srv.url + '/s/' + slug + '/');
  await page.waitForSelector('#accBtn');
  return { ctx, page, problems };
}
async function signIn(page, contact, via) {
  await page.click('#accBtn');
  await page.waitForSelector('.acc-in');
  await page.fill('.acc-in', contact);
  if (via) await page.check(`input[name=via][value=${via}]`);
  await page.click('.acc .btn >> text=Send code');
  await page.waitForSelector('.acc-code');
  const st = await page.evaluate(() => document.querySelector('.acc .sub').textContent);
  const to = contact.includes('@') ? contact.toLowerCase() : '+256' + contact.replace(/\D/g, '').replace(/^0/, '');
  const m = await devMsg(to, 'otp');
  await page.fill('.acc-code', m.text.match(/\b(\d{6})\b/)[1]);
  await page.click('.acc .btn >> text=Continue');
  return { st, m };
}
async function ownerPage(slug = 'demo', email = 'owner@demo.example', pw = 'demo-password-1') {
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 850 } });
  const page = await ctx.newPage();
  await page.goto(srv.url + '/s/' + slug + '/admin/');
  await page.fill('#email', email); await page.fill('#pass', pw);
  await page.click('button[type=submit]');
  await page.waitForSelector('nav');
  return { ctx, page };
}

test('a supermarket registers, the operator approves it, the owner sets a password from the link and loads prices', async () => {
  const ctx = await browser.newContext(); const page = await ctx.newPage();
  await page.goto(srv.url + '/');
  await page.click('text=Register it on Price Tag');
  await page.fill('#shop', 'Fresh Mart Mbarara');
  assert.equal(await page.inputValue('#slug'), 'fresh-mart-mbarara');            // address suggested from the name
  await page.fill('#name', 'Ann Owner'); await page.fill('#email', 'ann@freshmart.example'); await page.fill('#phone', '0772 555 111'); await page.fill('#town', 'Mbarara');
  await page.click('#go');
  await page.waitForSelector('#done:not([hidden])');

  // operator console
  await page.goto(srv.url + '/operator/');
  await page.fill('#tok', TOK); await page.click('button[type=submit]');
  await page.waitForSelector('text=Fresh Mart Mbarara');
  await page.click('.item:has-text("Fresh Mart Mbarara") >> text=Approve');
  await page.waitForSelector('.copy');
  const setup = await page.textContent('.copy');
  assert.match(setup, /\/s\/fresh-mart-mbarara\/admin\/#setup=pw_/);
  const welcome = await devMsg('ann@freshmart.example', 'welcome');
  assert.ok(welcome.text.includes(setup), 'the owner is e-mailed the same link');

  // owner opens the link and chooses a password
  const o = await browser.newContext({ viewport: { width: 1100, height: 850 } }); const op2 = await o.newPage();
  await op2.goto(setup);
  await op2.waitForSelector('#np1');
  await op2.fill('#np1', 'ann-own-pass-1'); await op2.fill('#np2', 'ann-own-pass-1');
  await op2.click('button[type=submit]');
  await op2.waitForSelector('nav');
  assert.ok(!op2.url().includes('setup='), 'token removed from the address bar');
  assert.match(await op2.textContent('header h1'), /Fresh Mart Mbarara/);
  // shop page exists and is empty until prices are imported
  const r = await fetch(srv.url + '/api/v1/s/fresh-mart-mbarara/home');
  assert.equal(r.status, 200);
  await ctx.close(); await o.close();
});

test('forgotten password: the owner gets a one-time link by e-mail', async () => {
  const ctx = await browser.newContext({ viewport: { width: 1100, height: 850 } }); const page = await ctx.newPage();
  await page.goto(srv.url + '/s/demo/admin/');
  await page.fill('#email', 'clerk@demo.example');
  await page.click('text=Forgot your password?');
  await page.waitForFunction(() => /we sent it a link/.test(document.getElementById('toast').textContent));
  const m = await devMsg('clerk@demo.example', 'password');
  const link = m.text.match(/(http\S+#setup=pw_\S+)/)[1];
  await page.goto(link);
  await page.fill('#np1', 'clerk-new-pass-1'); await page.fill('#np2', 'clerk-new-pass-1');
  await page.click('button[type=submit]');
  await page.waitForSelector('nav');
  await ctx.close();
});

test('shopper: sign up by e-mail, choose details, join with offers; the owner sees them, messages them; unsubscribe works', async () => {
  const s = await phone();
  await signIn(s.page, 'Jane@Mail.example');
  await s.page.waitForSelector('text=Your details');
  await s.page.fill('.acc-in', 'Jane');
  await s.page.click('.acc .btn >> text=Save');
  await s.page.waitForSelector('.acc-share');
  assert.match(await s.page.textContent('.acc-share'), /will see your name \(Jane\) and jane@mail\.example\. Other shops will not\./);
  assert.equal(await s.page.isChecked('#accOffers'), false, 'offers must be off by default');
  await s.page.check('#accOffers');
  await s.page.click('.acc .btn >> text=Join');
  await s.page.waitForSelector('.acc-dl');
  assert.ok(await s.page.$('#accBtn.on'), 'header shows signed in');

  const o = await ownerPage();
  await o.page.click('nav a[href="#customers"]');
  await o.page.waitForSelector('.item:has-text("jane@mail.example")');
  assert.match(await o.page.textContent('.item:has-text("jane@mail.example")'), /Offers on/);

  await o.page.click('nav a[href="#messages"]');
  await o.page.fill('textarea', 'Sugar 1 kg is UGX 4,900 this weekend only!');
  await o.page.click('text=Check before sending');
  await o.page.waitForSelector('.msg.ok');
  assert.match(await o.page.textContent('.stats'), /1want offers/);
  await o.page.click('text=Send now');
  await o.page.click('.modal .primary');
  await o.page.waitForFunction(() => /Sent to 1 customers/.test(document.getElementById('toast').textContent));
  const mail = await devMsg('jane@mail.example', 'broadcast');
  assert.match(mail.text, /UGX 4,900/);
  const unsub = mail.text.match(/(http\S+\/u\/\S+)/)[1];

  const u = await s.ctx.newPage();
  await u.goto(unsub);
  assert.match(await u.textContent('body'), /will not send you offers any more/);
  await s.page.reload();
  await s.page.click('#accBtn');
  await s.page.waitForSelector('#accMk');
  assert.equal(await s.page.isChecked('#accMk'), false);
  assert.deepEqual(s.problems, []);
  await s.ctx.close(); await o.ctx.close();
});

test('one account everywhere: in another shop the shopper is signed in but shares nothing until they join there', async () => {
  // a second shop
  await op('POST', 'applications/0/approve').catch(() => {});
  const res = await fetch(srv.url + '/api/v1/operator/stores', { method: 'POST', headers: { Authorization: 'Bearer ' + TOK, 'Content-Type': 'application/json' }, body: JSON.stringify({ slug: 'corner-shop', name: 'Corner Shop', owner_email: 'c@corner.example', owner_password: 'corner-pass-1' }) });
  assert.equal(res.status, 201);
  const s = await phone('demo');
  await signIn(s.page, 'paul@mail.example');
  await s.page.waitForSelector('text=Your details');
  await s.page.click('.acc .btn >> text=Save');
  await s.page.click('.acc .btn >> text=Join');
  await s.page.waitForSelector('.acc-dl');

  await s.page.goto(srv.url + '/s/corner-shop/');
  await s.page.waitForSelector('#accBtn.on');                                    // already signed in: no new account
  const c = await ownerPage('corner-shop', 'c@corner.example', 'corner-pass-1');
  await c.page.click('nav a[href="#customers"]');
  await c.page.waitForSelector('text=Nobody has joined yet');                  // Corner Shop sees nothing yet
  await s.page.click('#accBtn');
  await s.page.waitForSelector('.acc-share');
  assert.match(await s.page.textContent('.acc-share'), /Corner Shop will see your name and paul@mail\.example/);
  await s.page.click('.acc .btn >> text=Join Corner Shop');
  await s.page.waitForSelector('.acc-shops');
  assert.match(await s.page.textContent('.acc-shops'), /Price Tag[\s\S]*Corner Shop|Corner Shop[\s\S]*Price Tag/);
  await c.page.reload(); await c.page.click('nav a[href="#customers"]');
  await c.page.waitForSelector('.item:has-text("paul@mail.example")');
  // leaving removes access again
  await s.page.click('.acc .btn >> text=Leave Corner Shop');
  await s.page.waitForFunction(() => /can no longer see your details/.test(document.getElementById('toast').textContent));
  await c.page.reload(); await c.page.click('nav a[href="#customers"]');
  await c.page.waitForSelector('text=Nobody has joined yet');
  await s.ctx.close(); await c.ctx.close();
});

test('phone sign-in: the shopper chooses WhatsApp or SMS for the code', async () => {
  const s = await phone();
  const { st, m } = await signIn(s.page, '0772 123 456', 'whatsapp');
  assert.match(st, /by WhatsApp to \+25677/);
  assert.equal(m.channel, 'whatsapp');
  await s.page.waitForSelector('text=Your details');
  assert.equal(await s.page.isDisabled('input[name=ch][value=email]'), true, 'no e-mail yet, so e-mail messages cannot be chosen');
  await s.ctx.close();
});

test('the owner arranges the shop page; the scan button and search always stay on top', async () => {
  const o = await ownerPage();
  await o.page.click('nav a[href="#page"]');
  await o.page.waitForSelector('select[aria-label="Add a section"]');
  await o.page.selectOption('select[aria-label="Add a section"]', 'signup');
  await o.page.selectOption('select[aria-label="Add a section"]', 'text');
  const inputs = o.page.locator('.item input[type=text]');
  await inputs.nth(0).fill('Get our offers on WhatsApp');
  await inputs.nth(1).fill('Opening hours');
  await o.page.locator('.item textarea').nth(1).fill('Monday to Sunday, 8am to 10pm');
  await o.page.click('.card .primary >> text=Save');
  await o.page.waitForFunction(() => /Shoppers see the new page/.test(document.getElementById('toast').textContent));

  const s = await phone();
  await s.page.waitForSelector('.blk.signup');
  assert.match(await s.page.textContent('.blk.signup'), /Get our offers on WhatsApp/);
  assert.match(await s.page.textContent('#offers'), /Opening hours[\s\S]*8am to 10pm/);
  const order = await s.page.evaluate(() => [...document.querySelectorAll('main > *')].filter((e) => !e.hidden).map((e) => e.id || e.className));
  assert.deepEqual(order.slice(0, 2), ['scanBtn', 'find'], 'scan button then search come first: ' + order.join(','));
  await o.ctx.close(); await s.ctx.close();
});

test('the operator sets message allowances; a shop without SMS allowance only offers e-mail codes', async () => {
  await op('PUT', 'shops/corner-shop/limits', { sms_month_cap: 0, wa_month_cap: 0 });
  const s = await phone('corner-shop');
  await s.page.click('#accBtn');
  await s.page.waitForSelector('.acc-in');
  assert.match(await s.page.textContent('.acc'), /can only send codes by e-mail/);
  await s.ctx.close();
});
