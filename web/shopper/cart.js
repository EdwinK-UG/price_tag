/* Shopping cart (Scan & Go, Stage 1). Lazy-loaded; plain ES2017; integer minor units. */
(function (root, factory) {
  var m = factory();
  if (typeof module === 'object' && module.exports) module.exports = m; else root.PTCart = m;
})(this, function () {
  'use strict';
  var MAX_QTY = 999, MAX_LINES = 200;
  function key(slug) { return 'pt:' + slug + ':cart'; }
  function isInt(n) { return typeof n === 'number' && Number.isSafeInteger(n); }
  function clone(l) { return Object.assign({}, l, { promos: l.promos ? l.promos.slice() : [] }); }

  function eff(it, now) {
    if (!it || !isInt(it.price) || it.price < 0) return { price: null, promo: null, next: null };
    var best = null, next = null, ps = it.promos || [];
    for (var i = 0; i < ps.length; i++) {
      var p = ps[i];
      if (!p || !isInt(p.price) || p.price < 0 || p.price >= it.price) continue;
      if (p.from <= now && now < p.to) { if (!best || p.price < best.price) best = p; }
      else if (p.from > now && (!next || p.from < next.from)) next = p;
    }
    return { price: best ? best.price : it.price, promo: best, next: next };
  }

  function cleanPromos(ps) {
    if (!Array.isArray(ps)) return [];
    var out = [];
    for (var i = 0; i < ps.length; i++) {
      var p = ps[i];
      if (p && isInt(p.price) && p.price >= 0 && isInt(p.from) && isInt(p.to))
        out.push({ id: p.id || 0, title: String(p.title || ''), price: p.price, from: p.from, to: p.to });
    }
    return out;
  }

  function sanitizeLine(r) {
    if (!r || typeof r !== 'object' || typeof r.code !== 'string' || !r.code) return null;
    var q = Math.floor(Number(r.qty) || 0);
    if (q < 1) return null;
    var price = isInt(r.price) && r.price >= 0 ? r.price : null, unk = !!r.unknown || price === null;
    return {
      code: r.code, name: String(r.name || r.code), size: String(r.size || ''), qty: Math.min(MAX_QTY, q),
      price: unk ? null : price, promos: unk ? [] : cleanPromos(r.promos),
      effPrice: unk ? null : (isInt(r.effPrice) && r.effPrice >= 0 ? r.effPrice : price),
      prevPrice: isInt(r.prevPrice) && r.prevPrice >= 0 ? r.prevPrice : null,
      changed: !!r.changed, unknown: unk,
      rev: isInt(r.rev) && r.rev >= 0 ? r.rev : 0, chk: isInt(r.chk) && r.chk >= 0 ? r.chk : 0,
      at: typeof r.at === 'number' && r.at > 0 ? r.at : Date.now()
    };
  }

  function load(st, slug) {
    try {
      var arr = JSON.parse((st && st.getItem(key(slug))) || '[]');
      if (!Array.isArray(arr)) return [];
      var out = [], seen = {};
      for (var i = 0; i < arr.length && out.length < MAX_LINES; i++) {
        var l = sanitizeLine(arr[i]);
        if (l && !seen[l.code]) { seen[l.code] = true; out.push(l); }
      }
      return out;
    } catch (e) { return []; }
  }
  function save(st, slug, lines) {
    try {
      if (!st) return false;
      if (lines && lines.length) st.setItem(key(slug), JSON.stringify(lines)); else st.removeItem(key(slug));
      return true;
    } catch (e) { return false; }
  }
  function count(lines) { var n = 0; for (var i = 0; i < lines.length; i++) n += lines[i].qty; return n; }
  function qtyOf(lines, code) { for (var i = 0; i < lines.length; i++) if (lines[i].code === code) return lines[i].qty; return 0; }

  function addItem(lines, it, now) {
    if (!it || !it.code) return lines.slice();
    var e = eff(it, now), unk = e.price === null, out = [], found = false;
    var base = {
      name: String(it.name || it.code), size: String(it.size || ''), price: unk ? null : it.price,
      promos: unk ? [] : cleanPromos(it.promos), effPrice: e.price, prevPrice: null, changed: false,
      unknown: unk, rev: isInt(it.rev) ? it.rev : 0, chk: isInt(it.chk) ? it.chk : 0, at: it.at || Date.now()
    };
    for (var i = 0; i < lines.length; i++) {
      var l = clone(lines[i]);
      if (l.code === it.code) { found = true; Object.assign(l, base, { qty: Math.min(MAX_QTY, l.qty + 1), rev: isInt(it.rev) ? it.rev : l.rev, chk: isInt(it.chk) ? it.chk : l.chk }); }
      out.push(l);
    }
    if (!found && out.length < MAX_LINES) out.push(Object.assign({ code: it.code, qty: 1 }, base));
    return out;
  }

  function addUnknown(lines, code, disp, nowMs) {
    if (!code) return lines.slice();
    var out = [], found = false;
    for (var i = 0; i < lines.length; i++) {
      var l = clone(lines[i]);
      if (l.code === code) { found = true; l.qty = Math.min(MAX_QTY, l.qty + 1); }
      out.push(l);
    }
    if (!found && out.length < MAX_LINES)
      out.push({ code: code, name: String(disp || code), size: '', qty: 1, price: null, promos: [], effPrice: null, prevPrice: null, changed: false, unknown: true, rev: 0, chk: 0, at: nowMs || Date.now() });
    return out;
  }

  function remove(lines, code) { return lines.filter(function (l) { return l.code !== code; }); }
  function clear() { return []; }
  function setQty(lines, code, qty) {
    var q = Math.floor(Number(qty) || 0);
    if (q <= 0) return remove(lines, code);
    return lines.map(function (l) {
      var c = clone(l);
      if (c.code === code) { c.qty = Math.min(MAX_QTY, q); c.changed = false; c.prevPrice = null; }
      return c;
    });
  }

  function reprice(lines, itemMap, goneList, now, opts) {
    opts = opts || {};
    var nowMs = opts.nowMs || Date.now(), gone = {}, next = [], changed = [], mutated = false;
    if (goneList) for (var g = 0; g < goneList.length; g++) gone[goneList[g]] = true;
    for (var i = 0; i < lines.length; i++) {
      var l = clone(lines[i]);
      if (gone[l.code]) {
        if (!l.unknown) { l.prevPrice = l.effPrice; l.changed = true; changed.push(l.code); }
        l.unknown = true; l.price = null; l.promos = []; l.effPrice = null; l.at = nowMs; mutated = true;
      } else if (itemMap && itemMap[l.code]) {
        var inc = itemMap[l.code], incRev = isInt(inc.rev) ? inc.rev : 0;
        if (opts.force || l.unknown || incRev >= l.rev) {
          var e = eff(inc, now), unk = e.price === null;
          if ((!l.unknown && !unk && l.effPrice !== null && e.price !== l.effPrice) || (l.unknown !== unk)) {
            l.changed = true; l.prevPrice = l.unknown ? null : l.effPrice; changed.push(l.code);
          }
          var nc = isInt(inc.chk) ? inc.chk : (isInt(opts.v) ? opts.v : l.chk);
          Object.assign(l, { name: String(inc.name || l.name), size: String(inc.size || ''), price: unk ? null : inc.price, promos: unk ? [] : cleanPromos(inc.promos), effPrice: e.price, unknown: unk, rev: incRev, chk: opts.force ? nc : Math.max(l.chk, nc), at: inc.at || nowMs });
          mutated = true;
        }
      } else if (!l.unknown && l.price !== null) {
        var cur = eff(l, now);
        if (l.effPrice !== null && cur.price !== l.effPrice) {
          l.prevPrice = l.effPrice; l.effPrice = cur.price; l.changed = true; changed.push(l.code); mutated = true;
        }
      }
      next.push(l);
    }
    return { lines: next, changedCount: changed.length, changedCodes: changed, mutated: mutated };
  }

  function summarize(lines, now, live, fresh) {
    var items = [], itemCount = 0, knownTotal = 0, overflow = false, unknownCount = 0, staleCount = 0, oldestAt = Infinity, nextBoundary = Infinity;
    for (var i = 0; i < lines.length; i++) {
      var l = lines[i], e = (!l.unknown && l.price !== null) ? eff(l, now) : { price: null, promo: null };
      itemCount += l.qty;
      if (e.price === null) {
        unknownCount++;
        items.push({ line: l, unitPrice: null, lineTotal: null, promo: null, isLive: false, unknown: true });
        continue;
      }
      var lt = e.price * l.qty;
      if (!Number.isSafeInteger(lt) || !Number.isSafeInteger(knownTotal + lt)) overflow = true; else knownTotal += lt;
      var isLive = !!(live && l.chk >= (fresh || 0));
      if (!isLive) staleCount++;
      if (l.at && l.at < oldestAt) oldestAt = l.at;
      var ps = l.promos || [];
      for (var j = 0; j < ps.length; j++) if (ps[j].price < l.price) {
        if (ps[j].from > now && ps[j].from < nextBoundary) nextBoundary = ps[j].from;
        else if (ps[j].to > now && ps[j].to < nextBoundary) nextBoundary = ps[j].to;
      }
      items.push({ line: l, unitPrice: e.price, lineTotal: lt, promo: e.promo, isLive: isLive, unknown: false });
    }
    return { items: items, itemCount: itemCount, lineCount: lines.length, knownTotal: knownTotal, overflow: overflow, unknownCount: unknownCount, staleCount: staleCount, oldestAt: oldestAt < Infinity ? oldestAt : 0, nextBoundary: nextBoundary };
  }

  function mount(host) {
    var h = host.h, lines = load(window.localStorage, host.slug), confirmClear = false, timer = null;
    function persist() { save(window.localStorage, host.slug, lines); host.onBadge(count(lines)); schedule(); }
    function schedule() {
      clearTimeout(timer);
      var now = host.nowSec(), sum = summarize(lines, now, host.state.live, host.state.fresh);
      if (sum.nextBoundary < Infinity) timer = setTimeout(onTick, Math.min(2147483000, Math.max(500, (sum.nextBoundary - now) * 1000 + 300)));
    }
    function onTick() {
      var r = reprice(lines, null, null, host.nowSec());
      if (r.mutated) { lines = r.lines; persist(); if (host.state.cartOpen) { render(); if (r.changedCount) host.toast('Cart prices updated'); } }
      else schedule();
    }
    function render() {
      if (!host.state.cartOpen) return;
      var now = host.nowSec(), r = reprice(lines, null, null, now);
      if (r.mutated) { lines = r.lines; save(window.localStorage, host.slug, lines); host.onBadge(count(lines)); }
      var sum = summarize(lines, now, host.state.live, host.state.fresh), st = host.store(), cur = st.currency + ' ', nodes = [];
      nodes.push(h('div', { class: 'cart-head' }, [
        h('h2', { id: 'sheetTitle', class: 'cart-title', text: 'Cart' + (sum.itemCount ? ' (' + sum.itemCount + ')' : '') }),
        lines.length && !confirmClear ? h('button', { class: 'link cart-clr', type: 'button', text: 'Clear cart', onclick: function () { confirmClear = true; render(); } }) : null
      ]));
      if (confirmClear && lines.length) {
        nodes.push(h('div', { class: 'cart-confirm', role: 'alert' }, [
          h('p', { text: 'Clear all ' + sum.itemCount + ' item' + (sum.itemCount === 1 ? '' : 's') + ' from your cart?' }),
          h('div', { class: 'cart-confirm-btns' }, [
            h('button', { class: 'btn danger', type: 'button', text: 'Yes, clear cart', onclick: function () { confirmClear = false; clearAll(); host.toast('Cart cleared'); } }),
            h('button', { class: 'btn ghost', type: 'button', text: 'Cancel', onclick: function () { confirmClear = false; render(); } })
          ])
        ]));
      }
      if (!lines.length) {
        nodes.push(h('div', { class: 'big' }, h('p', { text: 'Your cart is empty. Scan or look up a product to add it.' })));
        host.sheet(nodes); return;
      }
      var off = !host.state.online, unc = off || !host.state.live || sum.staleCount > 0;
      if (unc) nodes.push(h('p', { class: 'cart-warn', role: 'status', text: off ? 'No connection. Prices are saved copies and this total may be outdated.' : 'Reconnecting to live prices. This total may be outdated.' }));
      if (sum.unknownCount) nodes.push(h('p', { class: 'cart-warn', role: 'status', text: sum.unknownCount + ' item' + (sum.unknownCount === 1 ? ' has an unknown price and is' : 's have unknown prices and are') + ' excluded from the total.' }));

      var list = h('ul', { class: 'cart-list' });
      sum.items.forEach(function (e) {
        var l = e.line, info = [
          h('button', { class: 'cart-name', type: 'button', onclick: function () { host.state.cartOpen = false; host.lookup(l.code, 'cart'); } }, [h('b', { text: l.name }), l.size ? h('small', { text: ' · ' + l.size }) : null]),
          h('p', { class: 'cart-unit' + (e.unknown ? ' unk' : (e.promo ? ' deal' : '')), text: e.unknown ? 'Price unknown' : cur + host.money(e.unitPrice) + ' each' + (e.promo ? ' · Offer' : '') })
        ];
        if (l.changed) info.push(h('span', { class: 'chg', text: e.unknown ? 'Price changed (no longer listed)' : (l.prevPrice !== null ? 'Price changed · was ' + cur + host.money(l.prevPrice) : 'Price changed') }));
        if (!e.unknown && !e.isLive) info.push(h('p', { class: 'cart-age', text: 'Saved ' + host.ago(l.at) + (off ? ' · offline' : ' · unverified') }));
        list.appendChild(h('li', { class: 'cart-item' + (l.changed ? ' changed' : ''), 'data-code': l.code }, [
          h('div', { class: 'cart-info' }, info),
          h('div', { class: 'cart-ctrl' }, [
            h('div', { class: 'cart-qty' }, [
              h('button', { type: 'button', 'aria-label': 'Decrease quantity of ' + l.name, text: '−', onclick: function () { setLineQty(l.code, l.qty - 1); } }),
              h('span', { class: 'q', text: String(l.qty) }),
              h('button', { type: 'button', 'aria-label': 'Increase quantity of ' + l.name, text: '+', onclick: function () { setLineQty(l.code, l.qty + 1); } })
            ]),
            h('span', { class: 'cart-lt' + (e.unknown ? ' unk' : ''), text: e.unknown ? 'Price unknown' : cur + host.money(e.lineTotal) }),
            h('button', { class: 'link cart-rm', type: 'button', 'aria-label': 'Remove ' + l.name, text: 'Remove', onclick: function () { removeLine(l.code); } })
          ])
        ]));
      });
      nodes.push(list);
      nodes.push(h('div', { class: 'cart-tot' + (unc ? ' stale' : '') }, [
        h('div', { class: 'cart-tot-row' }, [h('span', { text: unc || sum.unknownCount ? 'Estimated total' : 'Total' }), h('strong', { class: 'cart-tot-amt', text: sum.overflow ? 'Too large' : cur + host.money(sum.knownTotal) })]),
        unc ? h('p', { class: 'cart-tot-note', text: 'Offline/unverified prices — may be outdated' }) : null,
        sum.unknownCount ? h('p', { class: 'cart-tot-note', text: 'Excludes ' + sum.unknownCount + ' unknown-price item' + (sum.unknownCount === 1 ? '' : 's') }) : null
      ]));
      host.sheet(nodes); schedule();
    }

    function add(it) {
      if (lines.length >= MAX_LINES && !qtyOf(lines, it.code)) { host.toast('Cart is full (max ' + MAX_LINES + ' products)'); return; }
      lines = addItem(lines, it, host.nowSec()); confirmClear = false; persist(); render();
    }
    function addUnk(code, disp) {
      if (lines.length >= MAX_LINES && !qtyOf(lines, code)) { host.toast('Cart is full (max ' + MAX_LINES + ' products)'); return; }
      lines = addUnknown(lines, code, disp, Date.now()); confirmClear = false; persist(); render();
    }
    function setLineQty(code, q) { lines = setQty(lines, code, q); confirmClear = false; persist(); render(); }
    function removeLine(code) { lines = remove(lines, code); confirmClear = false; persist(); render(); }
    function clearAll() { lines = clear(); confirmClear = false; persist(); render(); }
    function open() { confirmClear = false; host.openSheet(); host.state.open = null; host.state.cartOpen = true; render(); }
    function itemBar(it, rerender) {
      var q = qtyOf(lines, it.code);
      if (!q) return h('div', { class: 'cart-act' }, h('button', { class: 'btn cart-add', type: 'button', text: 'Add to cart', onclick: function () { add(it); rerender(); host.toast('Added to cart'); } }));
      return h('div', { class: 'cart-act' }, [
        h('div', { class: 'cart-qty' }, [
          h('button', { type: 'button', 'aria-label': 'Remove one from cart', text: '−', onclick: function () { setLineQty(it.code, q - 1); rerender(); } }),
          h('span', { class: 'q', text: q + ' in cart' }),
          h('button', { type: 'button', 'aria-label': 'Add one more to cart', text: '+', onclick: function () { setLineQty(it.code, q + 1); rerender(); } })
        ]),
        h('button', { class: 'btn ghost', type: 'button', text: 'View cart', onclick: open })
      ]);
    }
    function onEvents(items, gone, v, opts) {
      if (!lines.length) return;
      var map = {}; if (items) for (var i = 0; i < items.length; i++) map[items[i].code] = items[i];
      var r = reprice(lines, map, gone, host.nowSec(), { v: v, force: !!(opts && opts.force) });
      if (r.mutated) { lines = r.lines; persist(); if (host.state.cartOpen) { render(); if (r.changedCount) host.toast('Cart prices updated'); } }
    }
    function onStorage() { lines = load(window.localStorage, host.slug); confirmClear = false; host.onBadge(count(lines)); schedule(); if (host.state.cartOpen) render(); }
    host.onBadge(count(lines)); schedule();
    return {
      count: function () { return count(lines); }, qtyOf: function (c) { return qtyOf(lines, c); },
      codes: function () { return lines.map(function (l) { return l.code; }); }, has: function (c) { return qtyOf(lines, c) > 0; },
      add: add, addUnknown: addUnk, setQty: setLineQty, remove: removeLine, clear: clearAll, open: open, itemBar: itemBar,
      onEvents: onEvents, onStatus: function () { if (host.state.cartOpen) render(); }, onStorage: onStorage
    };
  }
  return { MAX_QTY: MAX_QTY, MAX_LINES: MAX_LINES, eff: eff, load: load, save: save, count: count, qtyOf: qtyOf, addItem: addItem, addUnknown: addUnknown, setQty: setQty, remove: remove, clear: clear, reprice: reprice, summarize: summarize, mount: mount };
});
