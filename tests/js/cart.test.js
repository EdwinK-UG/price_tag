'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const Cart = require('../../web/shopper/cart.js');

function memStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: (k) => { m.delete(k); },
  };
}

test('eff() returns min(base, best running offer) and ignores higher or inactive offers', () => {
  const item = {
    code: '02000000000017', name: 'Sugar', price: 5500,
    promos: [
      { id: 1, title: 'Expired', price: 3000, from: 100, to: 500 },
      { id: 2, title: 'Higher than base', price: 6000, from: 500, to: 2000 },
      { id: 3, title: 'Active A', price: 4900, from: 900, to: 1500 },
      { id: 4, title: 'Active B', price: 4500, from: 950, to: 1400 },
      { id: 5, title: 'Upcoming', price: 4000, from: 1600, to: 2000 },
    ],
  };
  assert.equal(Cart.eff(item, 800).price, 5500);
  assert.equal(Cart.eff(item, 800).next.id, 3);
  assert.equal(Cart.eff(item, 920).price, 4900);
  assert.equal(Cart.eff(item, 1000).price, 4500);
  assert.equal(Cart.eff(item, 1000).promo.title, 'Active B');
  assert.equal(Cart.eff(item, 1400).price, 4900); // half-open [from, to)
  assert.equal(Cart.eff(item, 1500).price, 5500);
  assert.equal(Cart.eff(item, 1500).next.id, 5);
  assert.equal(Cart.eff(item, 1600).price, 4000);
  assert.equal(Cart.eff({ code: 'X', price: null }, 1000).price, null);
  assert.equal(Cart.eff({ code: 'X', price: 12.5 }, 1000).price, null);
});

test('add, quantity changes, remove, clear and integer totals', () => {
  const now = 1000;
  let lines = [];
  lines = Cart.addItem(lines, { code: 'A', name: 'Sugar', size: '1 kg', price: 5500, promos: [{ id: 1, title: 'Deal', price: 4900, from: 900, to: 2000 }], rev: 2, chk: 5, at: 10000 }, now);
  lines = Cart.addItem(lines, { code: 'A', name: 'Sugar', size: '1 kg', price: 5500, promos: [{ id: 1, title: 'Deal', price: 4900, from: 900, to: 2000 }], rev: 2, chk: 5, at: 10000 }, now);
  lines = Cart.addItem(lines, { code: 'B', name: 'Salt', size: '500 g', price: 1500, promos: [], rev: 1, chk: 5, at: 10000 }, now);
  assert.equal(Cart.count(lines), 3);
  assert.equal(Cart.qtyOf(lines, 'A'), 2);

  let sum = Cart.summarize(lines, now, true, 5);
  assert.equal(sum.knownTotal, 2 * 4900 + 1500);
  assert.equal(sum.unknownCount, 0);
  assert.equal(sum.staleCount, 0);
  assert.equal(sum.overflow, false);
  assert.equal(sum.nextBoundary, 2000);

  lines = Cart.setQty(lines, 'B', 4);
  sum = Cart.summarize(lines, now, true, 5);
  assert.equal(sum.knownTotal, 2 * 4900 + 4 * 1500);

  // Quantity 0 removes line; huge quantity clamps to MAX_QTY.
  lines = Cart.setQty(lines, 'A', 0);
  assert.equal(Cart.qtyOf(lines, 'A'), 0);
  lines = Cart.setQty(lines, 'B', 5000);
  assert.equal(Cart.qtyOf(lines, 'B'), Cart.MAX_QTY);

  lines = Cart.remove(lines, 'B');
  assert.equal(lines.length, 0);
  assert.deepEqual(Cart.clear(), []);
});

test('unknown-price items never break totals and are counted separately', () => {
  let lines = [];
  lines = Cart.addItem(lines, { code: 'A', name: 'Rice', price: 4000, rev: 1, chk: 10, at: 1000 }, 1000);
  lines = Cart.addUnknown(lines, 'U1', '2099999999991', 1000);
  lines = Cart.addUnknown(lines, 'U1', '2099999999991', 1000);
  lines = Cart.addItem(lines, { code: 'U2', name: 'No price', price: null }, 1000);

  const sum = Cart.summarize(lines, 1000, true, 10);
  assert.equal(sum.itemCount, 4);
  assert.equal(sum.knownTotal, 4000);
  assert.equal(sum.unknownCount, 2);
  assert.equal(sum.items[1].unknown, true);
  assert.equal(sum.items[1].lineTotal, null);
  assert.equal(sum.items[2].unknown, true);
});

test('reprice() flags live price changes, offer boundaries, deletions, and newly listed items', () => {
  let lines = [];
  lines = Cart.addItem(lines, {
    code: 'A', name: 'Sugar', price: 5500,
    promos: [{ id: 1, title: 'Flash', price: 4900, from: 1000, to: 2000 }],
    rev: 1, chk: 1, at: 1000,
  }, 1500);
  lines = Cart.addUnknown(lines, 'U1', '5901234123457', 1000);

  // 1. Live price change via SSE
  let r = Cart.reprice(lines, {
    A: { code: 'A', name: 'Sugar', price: 6000, promos: [], rev: 2, chk: 2, at: 2000 },
  }, [], 1500, { v: 2 });
  assert.equal(r.changedCount, 1);
  assert.equal(r.lines[0].effPrice, 6000);
  assert.equal(r.lines[0].prevPrice, 4900);
  assert.equal(r.lines[0].changed, true);

  // 2. Stale rev is ignored unless force=true (backup restore)
  let stale = Cart.reprice(r.lines, {
    A: { code: 'A', name: 'Sugar', price: 5500, promos: [], rev: 1, chk: 1, at: 3000 },
  }, [], 1500, { v: 1 });
  assert.equal(stale.lines[0].effPrice, 6000);
  let forced = Cart.reprice(r.lines, {
    A: { code: 'A', name: 'Sugar', price: 5500, promos: [], rev: 1, chk: 1, at: 3000 },
  }, [], 1500, { v: 1, force: true });
  assert.equal(forced.lines[0].effPrice, 5500);
  assert.equal(forced.lines[0].changed, true);

  // 3. Offer boundary ends with no network items
  let withPromo = Cart.addItem([], {
    code: 'P', name: 'Juice', price: 5200,
    promos: [{ id: 2, title: 'Ends soon', price: 4000, from: 1000, to: 2000 }],
    rev: 1, chk: 1, at: 1000,
  }, 1500);
  let ended = Cart.reprice(withPromo, null, null, 2000);
  assert.equal(ended.changedCount, 1);
  assert.equal(ended.lines[0].effPrice, 5200);
  assert.equal(ended.lines[0].prevPrice, 4000);
  assert.equal(ended.lines[0].changed, true);

  // 4. Unknown product becomes listed; known product deleted (gone)
  let r2 = Cart.reprice(r.lines, {
    U1: { code: 'U1', name: 'New arrival', price: 7777, promos: [], rev: 3, chk: 3, at: 4000 },
  }, ['A'], 1500, { v: 3 });
  assert.equal(r2.changedCount, 2);
  assert.equal(r2.lines[0].unknown, true);
  assert.equal(r2.lines[0].changed, true);
  assert.equal(r2.lines[1].unknown, false);
  assert.equal(r2.lines[1].effPrice, 7777);
  assert.equal(r2.lines[1].changed, true);
});

test('summarize() detects stale/offline lines and integer overflow', () => {
  const lines = [
    { code: 'A', name: 'A', size: '', qty: 2, price: 5000, promos: [], effPrice: 5000, prevPrice: null, changed: false, unknown: false, rev: 1, chk: 5, at: 12345 },
  ];
  assert.equal(Cart.summarize(lines, 1000, false, 5).staleCount, 1);
  assert.equal(Cart.summarize(lines, 1000, true, 6).staleCount, 1);
  assert.equal(Cart.summarize(lines, 1000, true, 5).staleCount, 0);

  const huge = [
    { code: 'H', name: 'Huge', size: '', qty: 100, price: Math.pow(2, 50), promos: [], effPrice: Math.pow(2, 50), prevPrice: null, changed: false, unknown: false, rev: 1, chk: 5, at: 1 },
  ];
  assert.equal(Cart.summarize(huge, 1000, true, 5).overflow, true);
});

test('localStorage persistence is per-shop, resilient to corruption and storage errors', () => {
  const st = memStorage();
  const a = Cart.addItem([], { code: 'A', name: 'Milk', price: 3200, rev: 1, chk: 2, at: 5000 }, 100);
  const b = Cart.addItem([], { code: 'B', name: 'Bread', price: 4500, rev: 1, chk: 2, at: 5000 }, 100);
  assert.equal(Cart.save(st, 'shop-a', a), true);
  assert.equal(Cart.save(st, 'shop-b', b), true);
  assert.equal(Cart.load(st, 'shop-a')[0].name, 'Milk');
  assert.equal(Cart.load(st, 'shop-b')[0].name, 'Bread');

  // Corrupt JSON and invalid entries do not crash
  st.setItem('pt:shop-a:cart', '{not json');
  assert.deepEqual(Cart.load(st, 'shop-a'), []);
  st.setItem('pt:shop-a:cart', JSON.stringify([{ code: '', qty: 1 }, { code: 'X', qty: 0 }, { code: 'Y', qty: 2, price: 'bad' }, { code: 'Y', qty: 5, price: 100 }]));
  const cleaned = Cart.load(st, 'shop-a');
  assert.equal(cleaned.length, 1);
  assert.equal(cleaned[0].code, 'Y');
  assert.equal(cleaned[0].unknown, true);

  // Throwing storage (private browsing / quota full) never throws
  const broken = {
    getItem: () => { throw new Error('denied'); },
    setItem: () => { throw new Error('quota'); },
    removeItem: () => { throw new Error('denied'); },
  };
  assert.deepEqual(Cart.load(broken, 'shop-a'), []);
  assert.equal(Cart.save(broken, 'shop-a', a), false);
});

test('fingerprint() is order-independent and changes on qty, item, or effective price change', () => {
  const a = { code: 'A', name: 'Sugar', price: 5500, promos: [{ id: 1, title: 'D', price: 4900, from: 100, to: 500 }], rev: 1, chk: 1, at: 1 };
  const b = { code: 'B', name: 'Salt', price: 1500, promos: [], rev: 1, chk: 1, at: 1 };
  const l1 = Cart.addItem(Cart.addItem([], a, 200), b, 200);
  const l2 = Cart.addItem(Cart.addItem([], b, 200), a, 200);
  assert.equal(Cart.fingerprint(l1, 200), Cart.fingerprint(l2, 200));
  assert.notEqual(Cart.fingerprint(l1, 200), Cart.fingerprint(l1, 600)); // offer ended
  assert.notEqual(Cart.fingerprint(l1, 200), Cart.fingerprint(Cart.setQty(l1, 'B', 2), 200));
  assert.equal(Cart.fmtCode('K7M9P2'), 'K7M 9P2');
});
