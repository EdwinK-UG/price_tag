'use strict';
// Guards that keep the "very light, very fast, works on old phones" promise from regressing.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const zlib = require('node:zlib');
const acorn = require('acorn');

const WEB = path.resolve(__dirname, '../../web');
const read = (f) => fs.readFileSync(path.join(WEB, f));
const gz = (f) => zlib.gzipSync(read(f), { level: 9 }).length;

test('browser code is plain ES2017 (runs on Chrome 58+, i.e. old and cheap Android phones; no ?. ?? or object spread)', () => {
  const files = ['shopper/app.js', 'shopper/code.js', 'shopper/cart.js', 'shopper/decode.js', 'shopper/scanner.js', 'shared/sw.js', 'shared/ean.js', 'shared/test-sheet.js', 'admin/admin.js'];
  for (const f of files) {
    try { acorn.parse(read(f).toString(), { ecmaVersion: 2017, sourceType: 'script' }); }
    catch (e) { assert.fail(`${f}: ${e.message} (line ${e.loc && e.loc.line})`); }
  }
});

test('size budget: what a first-time shopper downloads for the app itself', () => {
  const critical = ['shopper/index.html', 'shopper/app.css', 'shopper/code.js', 'shopper/app.js'];
  const sizes = Object.fromEntries(critical.map((f) => [f, gz(f)]));
  const total = Object.values(sizes).reduce((a, b) => a + b, 0);
  const lazy = gz('shopper/scanner.js') + gz('shopper/decode.js');
  const cart = gz('shopper/cart.js') + gz('shopper/cart.css');
  const admin = gz('admin/admin.js') + gz('admin/admin.css') + gz('admin/index.html');
  console.log(`      shopper app (gzip): ${JSON.stringify(sizes)} = ${total} bytes; cart (lazy js+css): ${cart} bytes; scanner (loaded only on tap): ${lazy} bytes; admin: ${admin} bytes`);
  assert.ok(total < 16 * 1024, `shopper app is ${total} bytes gzipped; budget 16 KB`);
  assert.ok(cart < 8 * 1024, `cart module is ${cart} bytes gzipped; budget 8 KB`);
  assert.ok(lazy < 8 * 1024, `scanner+decoder is ${lazy} bytes gzipped; budget 8 KB`);
});

test('no framework or third-party code on the shopper path', () => {
  const src = ['shopper/app.js', 'shopper/cart.js', 'shopper/scanner.js', 'shopper/decode.js'].map((f) => read(f).toString()).join('\n');
  assert.ok(!/\bimport\s+.*from\b|\brequire\(\s*['"][^.]/.test(src.replace(/require\('\.\/code\.js'\)/g, '')));
  const html = read('shopper/index.html').toString();
  assert.deepEqual([...html.matchAll(/<script[^>]*src="([^"]+)"/g)].map((m) => m[1]), ['/static/shopper/code.js', '/static/shopper/app.js']);
});

test('HTML has no inline script, inline style attribute or inline handler (the CSP forbids them)', () => {
  for (const f of ['shopper/index.html', 'admin/index.html', 'shared/test-sheet.html']) {
    const html = read(f).toString();
    for (const m of html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>/g)) assert.fail(`${f}: inline <script>`);
    assert.ok(!/\sstyle="/.test(html), `${f}: style attribute`);
    assert.ok(!/\son[a-z]+="/.test(html), `${f}: inline event handler`);
  }
});

test('app code never builds markup from server data with innerHTML / document.write / eval', () => {
  for (const f of ['shopper/app.js', 'shopper/cart.js', 'shopper/scanner.js', 'admin/admin.js', 'shared/test-sheet.js', 'shopper/decode.js']) {
    const src = read(f).toString();
    assert.ok(!/\.innerHTML\s*=/.test(src), `${f}: innerHTML assignment`);
    assert.ok(!/document\.write|\beval\(|new Function\(|insertAdjacentHTML/.test(src), `${f}: unsafe sink`);
  }
});

test('the service worker never caches API responses or the admin app', () => {
  const sw = read('shared/sw.js').toString();
  assert.match(sw, /\/api\/.*return/s);
  assert.match(sw, /static\/admin/);
});

test('no HTML page uses an inline <style> block (style-src self would block it and leave the page unstyled)', () => {
  for (const f of ['shopper/index.html', 'admin/index.html', 'shared/index.html', 'shared/test-sheet.html']) {
    assert.ok(!/<style[\s>]/.test(read(f).toString()), `${f}: inline <style>`);
  }
});
