/* Shopper app: scan or type a barcode, see the shop's current price.
 *
 * Design rules (slow phones, expensive data, flaky shop Wi-Fi):
 *  - no framework, no build step, ES2017 only; first paint comes from the HTML
 *    shell and from the last saved home page, before any network call;
 *  - a price lookup is ONE small request (~200 bytes); repeat looks cost nothing
 *    while the live connection says nothing changed;
 *  - changes are pushed over one server-sent-event stream, folded into a single
 *    catch-up message after being offline;
 *  - everything seen is kept on the phone and still shown (marked) when offline;
 *  - the camera/decoder code is only loaded when "Scan" is tapped.
 */
(function () {
  'use strict';
  var C = window.PTCode;
  var slug = location.pathname.split('/')[2];
  var API = '/api/v1/s/' + encodeURIComponent(slug) + '/';
  var $ = function (id) { return document.getElementById(id); };
  var DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

  // ---- tiny helpers -------------------------------------------------------------------
  function h(tag, a, kids) {
    var e = document.createElement(tag), k, v;
    if (a) for (k in a) {
      v = a[k];
      if (v == null || v === false) continue;
      if (k === 'class') e.className = v; else if (k === 'text') e.textContent = v;
      else if (k.slice(0, 2) === 'on') e.addEventListener(k.slice(2), v); else e.setAttribute(k, v === true ? '' : v);
    }
    if (kids != null) [].concat(kids).forEach(function (c) { if (c != null && c !== false) e.appendChild(typeof c === 'object' ? c : document.createTextNode(c)); });
    return e;
  }
  var ls = {
    get: function (k) { try { return localStorage.getItem('pt:' + slug + ':' + k); } catch (e) { return null; } },
    set: function (k, v) { try { localStorage.setItem('pt:' + slug + ':' + k, v); } catch (e) { /* private mode / full */ } },
    del: function (k) { try { localStorage.removeItem('pt:' + slug + ':' + k); } catch (e) { } }
  };
  function jget(k, d) { try { var v = ls.get(k); return v ? JSON.parse(v) : d; } catch (e) { return d; } }
  function pad(n) { return (n < 10 ? '0' : '') + n; }

  // ---- state ----------------------------------------------------------------------------
  var S = {
    home: jget('home', null), etag: ls.get('etag'),
    v: ls.get('v') == null ? null : +ls.get('v'),       // last change version applied
    fresh: +(ls.get('fresh') || 0),                       // cached items verified before this are suspect
    full: ls.get('full') === '1',                         // whole catalogue saved for offline
    live: false, online: navigator.onLine !== false, offset: +(ls.get('offset') || 0),
    open: null, cartOpen: false,                          // code shown in the sheet / cart view open
    es: null, backoff: 1500, hiddenAt: 0
  };
  function saveMeta() {
    if (S.v != null) ls.set('v', S.v);
    var lf = +(ls.get('fresh') || 0);
    if (S.fresh < lf && S.v != null && lf <= S.v) S.fresh = lf;
    ls.set('fresh', S.fresh); ls.set('full', S.full ? '1' : '0');
  }
  function store() { return (S.home && S.home.store) || { name: 'Price check', currency: 'UGX', decimals: 0, tz_offset: 180 }; }

  // ---- money & time ------------------------------------------------------------------------
  function money(v) {
    var d = store().decimals | 0, s = String(Math.abs(v)), frac = '';
    if (d) { while (s.length <= d) s = '0' + s; frac = '.' + s.slice(-d); s = s.slice(0, -d); }
    return s.replace(/\B(?=(\d{3})+(?!\d))/g, ',') + frac;
  }
  function priceNode(v, cls) {
    return h('p', { class: 'price' + (cls ? ' ' + cls : '') }, [h('span', { class: 'cur', text: store().currency }), h('span', { class: 'amt', text: money(v) })]);
  }
  function nowSec() { return Math.floor((Date.now() + S.offset) / 1000); }
  function shopDate(unix) { return new Date((unix + (store().tz_offset | 0) * 60) * 1000); } // read with getUTC*
  function when(unix) {
    var d = shopDate(unix), n = shopDate(nowSec());
    var diff = Math.round((Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()) - Date.UTC(n.getUTCFullYear(), n.getUTCMonth(), n.getUTCDate())) / 864e5);
    var day = diff === 0 ? 'today' : diff === 1 ? 'tomorrow' : DAYS[d.getUTCDay()] + ' ' + d.getUTCDate() + '/' + (d.getUTCMonth() + 1);
    return day + ' ' + pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes());
  }
  function ago(ms) {
    var s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (s < 90) return 'just now'; if (s < 3600) return Math.round(s / 60) + ' min ago';
    if (s < 86400) return Math.round(s / 3600) + ' h ago'; return Math.round(s / 86400) + ' days ago';
  }

  // effective price right now: the best offer that is running, if it really is lower
  function eff(it) {
    var now = nowSec(), best = null, next = null;
    (it.promos || []).forEach(function (p) {
      if (p.price >= it.price) return;
      if (p.from <= now && now < p.to) { if (!best || p.price < best.price) best = p; }
      else if (p.from > now && (!next || p.from < next.from)) next = p;
    });
    return { price: best ? best.price : it.price, promo: best, next: next };
  }

  // ---- local item cache (IndexedDB, with an in-memory fallback) -----------------------------
  var mem = {}, idb = null, idbReady = new Promise(function (resolve) {
    try {
      var r = indexedDB.open('pt-' + slug, 1);
      r.onupgradeneeded = function () { r.result.createObjectStore('items', { keyPath: 'code' }); };
      r.onsuccess = function () { idb = r.result; resolve(); };
      r.onerror = r.onblocked = function () { resolve(); };
    } catch (e) { resolve(); }
  });
  function unwrap(o) { return o && typeof o === 'object' && 'result' in o ? o.result : undefined; }
  function tx(mode, fn) {
    return idbReady.then(function () {
      if (!idb) return unwrap(fn(null));
      return new Promise(function (resolve, reject) {
        var t = idb.transaction('items', mode), out = fn(t.objectStore('items'));
        t.oncomplete = function () { resolve(unwrap(out)); };
        t.onerror = t.onabort = function () { reject(t.error); };
      });
    });
  }
  var cache = {
    get: function (code) { return tx('readonly', function (st) { return st ? st.get(code) : { result: mem[code] }; }).then(function (r) { return r || null; }).catch(function () { return null; }); },
    put: function (items) { return tx('readwrite', function (st) { items.forEach(function (it) { if (st) st.put(it); else mem[it.code] = it; }); }).catch(function () { }); },
    del: function (code) { return tx('readwrite', function (st) { if (st) st.delete(code); else delete mem[code]; }).catch(function () { }); },
    clear: function () { mem = {}; return tx('readwrite', function (st) { if (st) st.clear(); }).catch(function () { }); },
    search: function (q, limit) {
      q = q.toLowerCase();
      return idbReady.then(function () {
        return new Promise(function (resolve) {
          var out = [];
          if (!idb) { Object.keys(mem).forEach(function (k) { if (out.length < limit && mem[k].name.toLowerCase().indexOf(q) >= 0) out.push(mem[k]); }); return resolve(out); }
          var cur = idb.transaction('items').objectStore('items').openCursor();
          cur.onsuccess = function () {
            var c = cur.result;
            if (!c || out.length >= limit) return resolve(out);
            if (c.value.name.toLowerCase().indexOf(q) >= 0) out.push(c.value);
            c.continue();
          };
          cur.onerror = function () { resolve(out); };
        });
      });
    }
  };

  // ---- network -------------------------------------------------------------------------------
  function getRaw(path, headers, ms) {
    var ctl = window.AbortController ? new AbortController() : null, timer = ctl && setTimeout(function () { ctl.abort(); }, ms || 12000);
    return fetch(API + path, { headers: headers || {}, signal: ctl ? ctl.signal : undefined, credentials: 'omit' }).then(function (res) {
      clearTimeout(timer); noteServer(res); setOnline(true); return res;
    }, function (e) { clearTimeout(timer); setOnline(false); throw e; });
  }
  function noteServer(res) { // correct our clock from the server's Date header (offers are time-windowed)
    var d = res.headers.get('date');
    if (d) { var t = Date.parse(d); if (t) { S.offset = t - Date.now(); ls.set('offset', S.offset); } }
  }
  function setOnline(on) {
    if (S.online === on) return;
    S.online = on; paintStatus();
    if (on && !S.live) { disconnect(); connect(); }        // don't wait for the browser's retry timer
  }

  // ---- cart (lazy-loaded on first use or when a saved cart is non-empty) ---------------------
  var cartCtl = null, cartLoading = null;
  function rawCart() { var a = jget('cart', []); return Array.isArray(a) ? a : []; }
  function cartCount() { return cartCtl ? cartCtl.count() : rawCart().reduce(function (n, l) { return n + (l && l.qty > 0 ? l.qty : 0); }, 0); }
  function cartQty(code) { if (cartCtl) return cartCtl.qtyOf(code); var a = rawCart(); for (var i = 0; i < a.length; i++) if (a[i] && a[i].code === code) return a[i].qty || 0; return 0; }
  function cartCodes() { return cartCtl ? cartCtl.codes() : rawCart().map(function (l) { return l && l.code; }).filter(Boolean); }
  function paintCartBadge(n) { var b = $('cartCnt'), c = n == null ? cartCount() : n; b.textContent = String(c); b.hidden = !c; }
  function loadCart() {
    if (cartCtl) return Promise.resolve(cartCtl);
    if (!cartLoading) {
      if (!document.querySelector('link[href="/static/shopper/cart.css"]')) document.head.appendChild(h('link', { rel: 'stylesheet', href: '/static/shopper/cart.css' }));
      cartLoading = (window.PTCart ? Promise.resolve() : loadScript('/static/shopper/cart.js')).then(function () {
        cartCtl = window.PTCart.mount({ slug: slug, store: store, money: money, ago: ago, nowSec: nowSec, C: C, h: h, openSheet: openSheet, closeSheet: closeSheet, sheet: sheet, toast: toast, state: S, cache: cache, lookup: lookup, fetchItem: fetchItem, onBadge: paintCartBadge });
        return cartCtl;
      }, function (e) { cartLoading = null; throw e; });
    }
    return cartLoading;
  }
  function withCart(fn) { return loadCart().then(fn, function () { toast('Could not load cart. Check your connection.'); }); }

  // ---- UI: header, notices -----------------------------------------------------------------------
  function paintStatus() {
    var d = $('live');
    d.className = 'dot ' + (S.live ? 'on' : (!S.online ? 'off' : ''));
    d.setAttribute('aria-label', S.live ? 'Live prices' : (S.online ? 'Connecting' : 'Offline, showing saved prices'));
    var n = $('notice');
    if (!S.online) { n.textContent = 'No connection. Showing prices saved on this phone.'; n.hidden = false; } else n.hidden = true;
    if (S.open) refreshSheetMeta();
    if (cartCtl) cartCtl.onStatus();
  }
  function toast(msg) {
    var t = $('toast'); t.textContent = msg; t.hidden = false;
    clearTimeout(toast.t); toast.t = setTimeout(function () { t.hidden = true; }, 3200);
  }

  // ---- home page: offers ----------------------------------------------------------------------------
  var boundaryTimer;
  function renderHome() {
    var st = store();
    $('storeName').textContent = st.name; document.title = st.name + ' - prices';
    var a = $('announce'); a.textContent = st.announcement || ''; a.hidden = !st.announcement;
    var box = $('offers'), now = nowSec(), promos = (S.home && S.home.promos) || [], next = Infinity;
    box.textContent = '';
    var live = promos.filter(function (p) {
      if (p.from > now) next = Math.min(next, p.from);
      else if (p.to > now) { next = Math.min(next, p.to); return true; }
      return false;
    });
    if (live.length) {
      var head = h('div', { class: 'sec-h' }, h('h2', { text: 'Offers' })); box.appendChild(head);
    }
    live.forEach(function (p) {
      var rows = h('ul', { class: 'rows' });
      (p.items || []).forEach(function (it) {
        rows.appendChild(h('li', null, h('button', { class: 'row', type: 'button', 'data-code': it.code, onclick: function () { lookup(it.code, 'home'); } }, [
          h('span', { class: 'n' }, [h('b', { text: it.name }), h('small', { text: it.size || '' })]),
          h('span', { class: 'p deal' }, [money(it.promo_price), h('s', { text: money(it.price) })])
        ])));
      });
      box.appendChild(h('article', { class: 'promo' }, [
        h('header', null, [h('h3', { text: p.title }), h('p', { text: (p.text ? p.text + ' · ' : '') + 'ends ' + when(p.to) })]),
        rows, p.more ? h('div', { class: 'more', text: '+ ' + p.more + ' more in store' }) : null
      ]));
    });
    clearTimeout(boundaryTimer); // re-render exactly when an offer starts or ends, no polling
    if (next < Infinity) boundaryTimer = setTimeout(renderHome, Math.min(2147483000, Math.max(500, (next - now) * 1000 + 300)));
  }

  var homeTimer;
  function loadHomeSoon() { clearTimeout(homeTimer); homeTimer = setTimeout(loadHome, 120); }
  function loadHome() {
    return getRaw('home', S.etag ? { 'If-None-Match': S.etag } : {}).then(function (res) {
      if (res.status === 304) return;
      if (!res.ok) throw new Error('home ' + res.status);
      S.etag = res.headers.get('etag');
      return res.json().then(function (j) { S.home = j; ls.set('home', JSON.stringify(j)); if (S.etag) ls.set('etag', S.etag); renderHome(); renderRecent(); if (S.open) refreshSheet(); });
    }).catch(function () { /* offline: keep the saved page */ });
  }

  // ---- recents -------------------------------------------------------------------------------------------
  function addRecent(code) {
    var r = jget('recent', []).filter(function (x) { return x !== code; });
    r.unshift(code); ls.set('recent', JSON.stringify(r.slice(0, 12)));
    renderRecent();
  }
  function renderRecent() {
    var codes = jget('recent', []), list = $('recentList');
    $('recent').hidden = !codes.length;
    Promise.all(codes.map(function (c) { return cache.get(c); })).then(function (items) {
      list.textContent = '';
      items.forEach(function (it, i) {
        if (!it) return;
        var e = eff(it);
        list.appendChild(h('li', null, h('button', { class: 'row', type: 'button', onclick: function () { lookup(codes[i], 'recent'); } }, [
          h('span', { class: 'n' }, [h('b', { text: it.name }), h('small', { text: it.size || '' })]),
          h('span', { class: 'p' + (e.promo ? ' deal' : '') }, money(e.price))
        ])));
      });
    });
  }

  // ---- the item sheet ----------------------------------------------------------------------------------------
  var opener = null, sheetItem = null, sheetTimer;
  // One history entry stands for "an overlay (sheet or scanner) is open", so the
  // phone's Back button closes the overlay instead of leaving the page. Moving
  // from the scanner to the result sheet reuses the same entry (no back/push race).
  function pushOverlay() { try { if (!(history.state && history.state.ov)) history.pushState({ ov: 1 }, ''); } catch (e) { } }
  function popOverlay() { try { if (history.state && history.state.ov) history.back(); } catch (e) { } }
  function openSheet() {
    var s = $('sheet');
    if (s.hidden) { opener = document.activeElement; s.hidden = false; pushOverlay(); }
  }
  function closeSheet(fromPop) {
    var s = $('sheet');
    if (s.hidden) return;
    s.hidden = true; S.open = null; S.cartOpen = false; sheetItem = null; clearTimeout(sheetTimer);
    if (!fromPop && $('scanner').hidden) popOverlay();
    if (opener && opener.focus) try { opener.focus(); } catch (e) { }
  }
  function sheet(children) {
    var card = $('sheetCard'); card.textContent = '';
    card.appendChild(h('button', { class: 'close', type: 'button', 'aria-label': 'Close', text: '×', onclick: function () { closeSheet(); } }));
    [].concat(children).forEach(function (c) { card.appendChild(c); });
    var btn = card.querySelector('button.close'); if (btn) btn.focus();
  }
  function message(title, text, action) {
    openSheet(); S.cartOpen = false; sheetItem = null;
    sheet(h('div', { class: 'big' }, [h('h2', { id: 'sheetTitle', text: title }), h('p', { text: text }), action || null]));
  }
  function metaText(it) {
    if (S.live && it.chk >= S.fresh) return 'Live price. Updates by itself.';
    if (!S.online) return 'Saved ' + ago(it.at || Date.now()) + '. No connection.';
    return 'Checking for the latest price…';
  }
  function renderItem(it, changed) {
    openSheet(); S.cartOpen = false; sheetItem = it;
    var e = eff(it), nodes = [];
    nodes.push(h('p', { class: 'cat', text: it.category || '' }));
    nodes.push(h('h2', { class: 'name', id: 'sheetTitle', text: it.name }));
    nodes.push(h('p', { class: 'size', text: it.size || '' }));
    if (e.promo) nodes.push(h('span', { class: 'badge', text: 'Offer: ' + e.promo.title }));
    var pn = priceNode(e.price, e.promo ? 'deal' : '');
    if (changed) pn.classList.add('flash');
    nodes.push(pn);
    if (e.promo) {
      nodes.push(h('p', { class: 'sub', text: 'Normal price ' + store().currency + ' ' + money(it.price) + ' · you save ' + money(it.price - e.price) }));
      nodes.push(h('p', { class: 'sub', text: 'Offer ends ' + when(e.promo.to) }));
    } else if (e.next) nodes.push(h('p', { class: 'sub', text: 'Offer from ' + when(e.next.from) + ': ' + store().currency + ' ' + money(e.next.price) }));
    nodes.push(cartCtl ? cartCtl.itemBar(it, function () { renderItem(it); }) : h('div', { class: 'cart-act' }, h('button', { class: 'btn cart-add', type: 'button', text: cartQty(it.code) ? 'In cart (' + cartQty(it.code) + ') · Add 1' : 'Add to cart', onclick: function () { withCart(function (c) { c.add(it); renderItem(it); toast('Added to cart'); }); } })));
    nodes.push(h('div', { class: 'meta' }, [h('span', { class: 'ld' + (S.live && it.chk >= S.fresh ? ' on' : '') }), h('span', { id: 'metaText', text: metaText(it), 'aria-live': 'polite' }), h('code', { text: C.display(it.code) })]));
    sheet(nodes);
    clearTimeout(sheetTimer);
    var nb = Infinity, nw = nowSec();
    (it.promos || []).forEach(function (p) { if (p.price < it.price) { if (p.from > nw) nb = Math.min(nb, p.from); else if (p.to > nw) nb = Math.min(nb, p.to); } });
    if (nb < Infinity) sheetTimer = setTimeout(function () { if (sheetItem && sheetItem.code === it.code) renderItem(sheetItem); }, Math.min(2147483000, Math.max(500, (nb - nw) * 1000 + 300)));
    if (changed) toast('Price just changed to ' + store().currency + ' ' + money(e.price));
  }
  function refreshSheetMeta() { var m = $('metaText'); if (m && sheetItem) { m.textContent = metaText(sheetItem); var ld = m.previousSibling; if (ld) ld.className = 'ld' + (S.live && sheetItem.chk >= S.fresh ? ' on' : ''); } }
  function refreshSheet() { if (S.open && sheetItem) cache.get(S.open).then(function (it) { if (it && S.open === it.code) { sheetItem = it; renderItem(it); } }); }

  // ---- lookup --------------------------------------------------------------------------------------------------
  var ERR = { empty: 'Type or scan a barcode first.', long: 'That number is too long.', chars: 'Use only letters, numbers and dashes.', check: 'That barcode number does not look right. Check the digits and try again, or scan it.' };
  function lookup(raw, src) {
    var c = C.canonical(raw);
    if (!c.ok) { message('Check the number', ERR[c.error] || 'Invalid code.'); return Promise.resolve(); }
    var code = c.code; S.open = code; S.cartOpen = false;
    return cache.get(code).then(function (cached) {
      if (S.open !== code) return;
      if (cached) {
        sheetItem = cached; renderItem(cached);
        if (S.live && cached.chk >= S.fresh) { addRecent(code); return; }      // the live stream guarantees it is current
      } else { openSheet(); sheet(h('div', { class: 'big' }, [h('div', { class: 'spin', role: 'status', 'aria-label': 'Loading' }), h('p', { text: 'Getting the price…' })])); }
      return fetchItem(code, cached).then(function (it) {
        if (S.open !== code) return;
        if (it === 404) {
          cache.del(code);
          if (cartQty(code)) withCart(function (ct) { ct.onEvents(null, [code], S.v); });
          message('Not in the price list', 'We could not find this product (' + C.display(code) + '). Ask a member of staff for the price.',
            h('button', { class: 'btn ghost', type: 'button', text: 'Add to cart (price unknown)', onclick: function () { withCart(function (ct) { ct.addUnknown(code, C.display(code)); toast('Added to cart (price unknown)'); ct.open(); }); } }));
          return;
        }
        if (cartQty(it.code)) withCart(function (ct) { ct.onEvents([it], null, it.chk); });
        renderItem(it); addRecent(code);
      }, function () {
        if (S.open !== code) return;
        if (cached) { renderItem(cached); return; }
        message('No connection', 'We could not reach the shop. Check your connection and try again.', h('button', { class: 'btn', type: 'button', text: 'Try again', onclick: function () { lookup(code, src); } }));
      });
    });
  }
  function fetchItem(code, cached) {
    var hdr = cached ? { 'If-None-Match': 'W/"' + cached.rev + '"' } : {};
    return getRaw('items/' + code, hdr, 15000).then(function (res) {
      var v = +res.headers.get('x-pt-v') || 0;
      if (res.status === 404) return 404;
      if (res.status === 304 && cached) {
        return cache.get(code).then(function (cur) {
          if (cur && cur.rev > cached.rev) return cur;
          cached.chk = Math.max(cached.chk || 0, v); cached.at = Date.now(); cache.put([cached]); return cached;
        });
      }
      if (!res.ok) throw new Error('item ' + res.status);
      return res.json().then(function (it) {
        return cache.get(code).then(function (cur) {
          if (cur && (cur.rev > it.rev || (cur.chk || 0) > v)) return cur; // an event already brought something newer
          it.chk = v; it.at = Date.now(); cache.put([it]); return it;
        });
      });
    });
  }

  // ---- search by name (damaged barcode) --------------------------------------------------------------------------
  function searchName(q) {
    var p = S.full ? cache.search(q, 20) : getRaw('search?q=' + encodeURIComponent(q)).then(function (r) { return r.json(); }).then(function (j) { return j.items; });
    return p.then(function (items) {
      openSheet(); S.cartOpen = false; sheetItem = null; S.open = null;
      if (!items.length) { sheet(h('div', { class: 'big' }, [h('h2', { id: 'sheetTitle', text: 'No match' }), h('p', { text: 'Nothing found for "' + q + '". Try another word or scan the barcode.' })])); return; }
      var list = h('ul', { class: 'rows' });
      items.forEach(function (it) {
        var e = eff(it);
        list.appendChild(h('li', null, h('button', { class: 'row', type: 'button', onclick: function () {
          cache.get(it.code).then(function (cur) {
            if (!cur || cur.rev < it.rev) { it.chk = it.chk || 0; it.at = Date.now(); return cache.put([it]); }
          }).then(function () { lookup(it.code, 'search'); });
        } }, [
          h('span', { class: 'n' }, [h('b', { text: it.name }), h('small', { text: it.size || '' })]),
          h('span', { class: 'p' + (e.promo ? ' deal' : ''), text: money(e.price) })
        ])));
      });
      sheet([h('h2', { id: 'sheetTitle', text: 'Matching products' }), list]);
    }, function () { message('No connection', 'Searching by name needs a connection. Scanning or typing a barcode works with saved prices.'); });
  }

  // ---- live updates (server-sent events) ----------------------------------------------------------------------------
  function connect() {
    if (S.es || !window.EventSource || document.hidden) return;
    clearTimeout(S.retryT);
    var es = new EventSource(API + 'events' + (S.v != null ? '?since=' + S.v : ''));
    S.es = es;
    es.addEventListener('hello', function (e) {
      var d = JSON.parse(e.data); S.backoff = 1500;
      S.offset = d.now * 1000 - Date.now(); ls.set('offset', S.offset);
      if (S.v == null) { S.v = d.v; S.fresh = d.v; saveMeta(); }
      S.live = (S.v === d.v); paintStatus();
    });
    es.addEventListener('change', function (e) { try { onChange(JSON.parse(e.data)); } catch (x) { /* ignore a malformed frame */ } });
    es.onerror = function () {
      // Never rely on the browser's automatic retry: it reuses the ORIGINAL url, whose
      // ?since= is stale, so the catch-up message would be skipped. Reconnect ourselves,
      // with the current cursor, after a jittered back-off (no thundering herd).
      es.close(); S.es = null; S.live = false; paintStatus();
      if (!S.online || document.hidden) return;              // online / visible handlers reconnect
      var wait = S.backoff * (0.6 + Math.random() * 0.8);
      S.backoff = Math.min(30000, S.backoff * 1.6);
      clearTimeout(S.retryT); S.retryT = setTimeout(connect, wait);
    };
  }
  function disconnect() { if (S.es) { S.es.close(); S.es = null; } S.live = false; paintStatus(); }

  function onChange(ev) {
    if (ev.reset && S.v != null && ev.v < S.v) {                  // shop data was restored from an older backup: start clean
      S.v = ev.v; S.fresh = ev.v; S.live = true; saveMeta(); paintStatus();
      var openCode = S.open;
      cache.clear().then(function () {
        renderRecent();
        if (openCode && S.open === openCode) lookup(openCode, 'restore');
        if (cartCodes().length) revalidateVisible(true);
        if (S.full) syncCatalog(true).catch(function () { });
      });
      loadHomeSoon(); return;
    }
    if (S.v != null && ev.v <= S.v) { if (S.es && !S.live) { S.live = true; paintStatus(); } return; } // duplicate
    var gap = S.v != null && ev.p !== S.v;
    if (gap || ev.reset) S.fresh = ev.v;                            // we cannot vouch for older cached items any more
    S.v = ev.v;
    if (S.es && !S.live) { S.live = true; paintStatus(); }
    var items = ev.items || [], gone = ev.gone || [];
    var work = items.map(function (it) {
      return cache.get(it.code).then(function (old) {
        if (!old && !S.full && S.open !== it.code && !cartQty(it.code)) return; // never looked at it: don't store it
        if (old && old.rev > it.rev) return;
        var changed = !old || old.price !== it.price || JSON.stringify(old.promos || []) !== JSON.stringify(it.promos || []);
        it.chk = ev.v; it.at = Date.now();
        return cache.put([it]).then(function () { if (S.open === it.code) { sheetItem = it; renderItem(it, changed); } });
      });
    });
    gone.forEach(function (code) {
      work.push(cache.del(code).then(function () { if (S.open === code) message('No longer listed', 'This product was just removed from the shop\'s price list. Ask staff for the price.'); }));
    });
    if (cartCount() > 0 && (items.length || gone.length)) withCart(function (c) { c.onEvents(items, gone, ev.v); });
    Promise.all(work).then(function () { renderRecent(); });
    saveMeta();
    if (ev.reset && S.full) syncCatalog(false).catch(function () { });
    else if (gap || ev.reset) revalidateVisible();                // a bulk change: re-check what the shopper can see
    if (ev.home || ev.reset) loadHomeSoon();
    refreshSheetMeta();
  }

  // After a bulk change (e.g. the nightly price file) we do not know which cached
  // prices moved, so re-check only what is on screen: the open item, recents and
  // cart items. Each is a conditional request (~100 bytes when nothing changed).
  function revalidateVisible(force) {
    var codes = jget('recent', []).slice(0, 12);
    cartCodes().forEach(function (c) { if (codes.indexOf(c) < 0) codes.push(c); });
    if (S.open && codes.indexOf(S.open) < 0) codes.unshift(S.open);
    codes.forEach(function (code) {
      cache.get(code).then(function (cached) {
        var inC = cartQty(code) > 0;
        if (!cached && !force && !inC) return;
        return fetchItem(code, force ? null : cached).then(function (it) {
          if (it === 404) {
            cache.del(code);
            if (inC) withCart(function (c) { c.onEvents(null, [code], S.v, { force: !!force }); });
            if (S.open === code) message('No longer listed', 'This product was just removed from the shop\'s price list. Ask staff for the price.');
            return;
          }
          if (inC) withCart(function (c) { c.onEvents([it], null, it.chk, { force: !!force }); });
          if (S.open === code && (!cached || it.price !== cached.price || it.rev !== cached.rev)) { sheetItem = it; renderItem(it, true); }
          else if (S.open === code) refreshSheetMeta();
        });
      }).catch(function () { }).then(renderRecent);
    });
  }

  // ---- offline catalogue (opt-in) -------------------------------------------------------------------------------------
  function syncCatalog(fresh, onProgress) {
    var since = fresh || S.v == null ? '' : '?since=' + S.v, n = 0, hdr = null, batch = [], buf = '', dec = window.TextDecoder ? new TextDecoder() : null;
    function flush() { var b = batch; batch = []; return cache.put(b); }
    function line(l) {
      if (!l) return Promise.resolve();
      var o = JSON.parse(l);
      if (o.code) { o.chk = hdr.v; o.at = Date.now(); batch.push(o); n++; if (onProgress && n % 200 === 0) onProgress(n, hdr.total || 0); return batch.length >= 500 ? flush() : Promise.resolve(); }
      if (o.gone) return cache.del(o.gone);
      if (o.end) { hdr.done = true; return Promise.resolve(); }
      if (o.v != null) { hdr = o; return o.full ? cache.clear() : Promise.resolve(); }
      return Promise.resolve();
    }
    return getRaw('catalog' + since, {}, 300000).then(function (res) {
      if (!res.ok) throw new Error('catalog ' + res.status);
      var chain = Promise.resolve();
      var done = function () { return chain.then(flush).then(function () { if (!hdr || !hdr.done) throw new Error('download was cut short'); S.full = true; S.v = hdr.v; S.fresh = hdr.v; saveMeta(); }); };
      if (!res.body || !res.body.getReader || !dec) return res.text().then(function (t) { t.split('\n').forEach(function (l) { chain = chain.then(function () { return line(l); }); }); return done(); });
      var rd = res.body.getReader();
      function pump() {
        return rd.read().then(function (r) {
          if (r.done) { chain = chain.then(function () { return line(buf); }); return done(); }
          buf += dec.decode(r.value, { stream: true });
          var parts = buf.split('\n'); buf = parts.pop();
          parts.forEach(function (l) { chain = chain.then(function () { return line(l); }); });
          return chain.then(pump);
        });
      }
      return pump();
    });
  }

  function openMenu() {
    openSheet(); S.cartOpen = false; sheetItem = null; S.open = null;
    var prog = h('progress', { max: '100', value: '0', hidden: true }), status = h('p', { class: 'sub', text: S.full ? 'Prices are saved on this phone for use without a connection.' : 'Save the whole price list on this phone to scan with no signal. Best done on Wi-Fi (about 1 MB per 20,000 products).' });
    var dl = h('button', { class: 'btn', type: 'button', text: S.full ? 'Update saved prices' : 'Save prices for offline use', onclick: function () {
      dl.disabled = true; prog.hidden = false; status.textContent = 'Downloading…';
      syncCatalog(!S.full, function (n, t) { prog.max = t || n; prog.value = n; status.textContent = 'Saving ' + n.toLocaleString() + (t ? ' of ' + t.toLocaleString() : '') + ' products…'; })
        .then(function () { status.textContent = 'Done. Prices now work without a connection.'; prog.hidden = true; dl.disabled = false; dl.textContent = 'Update saved prices'; renderRecent(); })
        .catch(function () { status.textContent = 'The download did not finish. Try again on a better connection.'; prog.hidden = true; dl.disabled = false; });
    } });
    var rm = h('button', { class: 'btn ghost', type: 'button', text: 'Forget everything saved on this phone', onclick: function () {
      cache.clear().then(function () { ['recent', 'v', 'fresh', 'full', 'home', 'etag', 'cart'].forEach(ls.del); if (cartCtl) cartCtl.clear(); else paintCartBadge(0); S.full = false; S.v = null; S.fresh = 0; S.home = null; S.etag = null; renderHome(); renderRecent(); disconnect(); connect(); loadHome(); toast('Saved prices removed'); closeSheet(); });
    } });
    sheet(h('div', { class: 'menu' }, [h('h2', { id: 'sheetTitle', text: store().name }), status, prog, dl, rm]));
  }

  // ---- scanner ----------------------------------------------------------------------------------------------------------
  var scanCtl = null, audio = null;
  function loadScript(src) { return new Promise(function (ok, bad) { var s = document.createElement('script'); s.src = src; s.onload = ok; s.onerror = bad; document.head.appendChild(s); }); }
  function beep() {
    try {
      audio = audio || new (window.AudioContext || window.webkitAudioContext)();
      var o = audio.createOscillator(), g = audio.createGain(); o.frequency.value = 1100; g.gain.value = 0.12;
      o.connect(g); g.connect(audio.destination); o.start(); o.stop(audio.currentTime + 0.09);
    } catch (e) { }
    if (navigator.vibrate) navigator.vibrate(40);
  }
  function openScanner() {
    try { audio = audio || new (window.AudioContext || window.webkitAudioContext)(); if (audio.resume) audio.resume(); } catch (e) { }
    var box = $('scanner'); box.hidden = false; $('scanMsg').textContent = 'Starting the camera…';
    $('torchBtn').hidden = $('zoomBtn').hidden = true;
    pushOverlay();
    (window.PTScan ? Promise.resolve() : loadScript('/static/shopper/decode.js').then(function () { return loadScript('/static/shopper/scanner.js'); }))
      .then(function () {
        if (box.hidden) return;
        return PTScan.start($('cam'), {
          onCode: function (code) { beep(); closeScanner(false, true); lookup(code, 'scan'); },
          onStatus: function (m) { $('scanMsg').textContent = m; },
          onCaps: function (c) { $('torchBtn').hidden = !c.torch; $('zoomBtn').hidden = !c.zoom; }
        }).then(function (ctl) { scanCtl = ctl; if (box.hidden) ctl.stop(); }, function (err) { $('scanMsg').textContent = err.message || 'The camera could not start. You can type the number instead.'; });
      }, function () { $('scanMsg').textContent = 'Could not load the scanner. Check your connection or type the number.'; });
  }
  function closeScanner(fromPop, keepEntry) {
    var box = $('scanner');
    if (box.hidden) return;
    box.hidden = true;
    if (scanCtl) { scanCtl.stop(); scanCtl = null; }
    if (!fromPop && !keepEntry && $('sheet').hidden) popOverlay();
  }

  // ---- wiring -------------------------------------------------------------------------------------------------------------
  $('scanBtn').addEventListener('click', openScanner);
  $('scanClose').addEventListener('click', function () { closeScanner(); });
  $('torchBtn').addEventListener('click', function () { if (scanCtl) scanCtl.torch(); });
  $('zoomBtn').addEventListener('click', function () { if (scanCtl) scanCtl.zoom(); });
  $('cartBtn').addEventListener('click', function () { withCart(function (c) { c.open(); }); });
  $('menuBtn').addEventListener('click', openMenu);
  $('clearRecent').addEventListener('click', function () { ls.del('recent'); renderRecent(); });
  $('sheet').addEventListener('click', function (e) { if (e.target === $('sheet')) closeSheet(); });
  $('find').addEventListener('submit', function (e) {
    e.preventDefault();
    var q = $('q').value.trim(); if (!q) return;
    $('q').blur();
    if (/^[0-9\s]+$/.test(q)) lookup(q, 'typed'); else searchName(q);
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { if (!$('scanner').hidden) closeScanner(); else closeSheet(); } });
  window.addEventListener('popstate', function () { closeScanner(true); closeSheet(true); });
  window.addEventListener('online', function () { setOnline(true); loadHome(); });
  window.addEventListener('offline', function () { S.online = false; disconnect(); });   // keep "live" honest: no connection, no live claim
  document.addEventListener('visibilitychange', function () { // save battery and data while the app is in the background
    if (document.hidden) { S.hiddenAt = Date.now(); if (!$('scanner').hidden) closeScanner(); setTimeout(function () { if (document.hidden) disconnect(); }, 30000); }
    else { connect(); if (Date.now() - S.hiddenAt > 30000) loadHome(); if (sheetItem) refreshSheetMeta(); }
  });

  window.addEventListener('storage', function (e) {
    var pfx = 'pt:' + slug + ':';
    if (e.key === pfx + 'fresh' && e.newValue && +e.newValue > S.fresh) { S.fresh = +e.newValue; refreshSheetMeta(); if (cartCtl) cartCtl.onStatus(); }
    else if (e.key === pfx + 'recent') renderRecent();
    else if (e.key === pfx + 'cart') { paintCartBadge(); if (cartCtl) cartCtl.onStorage(); else if (cartCount() > 0) loadCart().catch(function () { }); }
  });

  renderHome(); renderRecent(); paintStatus(); paintCartBadge();
  if (cartCount() > 0) setTimeout(function () { loadCart().catch(function () { }); }, 0);
  connect(); loadHome();
  if ('serviceWorker' in navigator) window.addEventListener('load', function () {
    setTimeout(function () {
      navigator.serviceWorker.register('/sw.js', { scope: '/' }).then(function () { return navigator.serviceWorker.ready; })
        .then(function (reg) { if (reg.active) reg.active.postMessage({ type: 'shell', url: location.pathname }); }).catch(function () { });
    }, 1500);
  });
  window.__pt = { state: S, lookup: lookup, loadCart: loadCart }; // handy for debugging in the browser console
})();
