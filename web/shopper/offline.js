/* Offline price list (opt-in) and the shop menu. Lazy-loaded: shoppers who
 * never save prices for offline use never download this code. Plain ES2017. */
(function () {
  'use strict';
  function mount(x) {
    // In offline-catalogue mode a completed sync leaves the phone's copy equal to the
    // shop's list at version x.S.v. Push that into what the shopper can see: the cart
    // (repriced, with "price changed" markers), the open item and the recents.
    function afterCatalogSync() {
      var v = x.S.v || 0;
      x.renderRecent();
      if (x.S.open) {
        var oc = x.S.open;
        x.cache.get(oc).then(function (it) {
          if (x.S.open !== oc) return;
          if (it) { it.chk = Math.max(it.chk || 0, v); x.renderItem(it); }
          else x.message('No longer listed', 'This product was just removed from the shop\'s price list. Ask staff for the price.');
        });
      }
      var codes = x.cartCodes();
      if (!codes.length) return;
      Promise.all(codes.map(function (c) { return x.cache.get(c); })).then(function (arr) {
        var items = [], gone = [];
        arr.forEach(function (it, i) {
          if (it) items.push(Object.assign({}, it, { chk: Math.max(it.chk || 0, v) }));
          else gone.push(codes[i]);
        });
        x.withCart(function (c) { c.onEvents(items, gone, v); });
      });
    }

    // ---- offline catalogue (opt-in) -------------------------------------------------------------------------------------
    function syncCatalog(fresh, onProgress, sinceV) {
      var from = sinceV != null ? sinceV : x.S.v;
      var since = fresh || from == null ? '' : '?since=' + from, n = 0, hdr = null, batch = [], buf = '', dec = window.TextDecoder ? new TextDecoder() : null;
      function flush() { var b = batch; batch = []; return x.cache.put(b); }
      function line(l) {
        if (!l) return Promise.resolve();
        var o = JSON.parse(l);
        if (o.code) { o.chk = hdr.v; o.at = Date.now(); batch.push(o); n++; if (onProgress && n % 200 === 0) onProgress(n, hdr.total || 0); return batch.length >= 500 ? flush() : Promise.resolve(); }
        if (o.gone) return x.cache.del(o.gone);
        if (o.end) { hdr.done = true; return Promise.resolve(); }
        if (o.v != null) { hdr = o; return o.full ? x.cache.clear() : Promise.resolve(); }
        return Promise.resolve();
      }
      return x.getRaw('catalog' + since, {}, 300000).then(function (res) {
        if (!res.ok) throw new Error('catalog ' + res.status);
        var chain = Promise.resolve();
        var done = function () { return chain.then(flush).then(function () { if (!hdr || !hdr.done) throw new Error('download was cut short'); x.S.full = true; x.S.v = hdr.v; x.S.fresh = hdr.v; x.saveMeta(); }); };
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
      x.openSheet(); x.S.cartOpen = false; x.clearSheetItem(); x.S.open = null;
      var prog = x.h('progress', { max: '100', value: '0', hidden: true }), status = x.h('p', { class: 'sub', text: x.S.full ? 'Prices are saved on this phone for use without a connection.' : 'Save the whole price list on this phone to scan with no signal. Best done on Wi-Fi (about 1 MB per 20,000 products).' });
      var dl = x.h('button', { class: 'btn', type: 'button', text: x.S.full ? 'Update saved prices' : 'Save prices for offline use', onclick: function () {
        dl.disabled = true; prog.hidden = false; status.textContent = 'Downloading…';
        syncCatalog(!x.S.full, function (n, t) { prog.max = t || n; prog.value = n; status.textContent = 'Saving ' + n.toLocaleString() + (t ? ' of ' + t.toLocaleString() : '') + ' products…'; })
          .then(function () { x.loadCart().catch(function () { }); status.textContent = 'Done. Prices now work without a connection.'; prog.hidden = true; dl.disabled = false; dl.textContent = 'Update saved prices'; x.renderRecent(); })
          .catch(function () { status.textContent = 'The download did not finish. Try again on a better connection.'; prog.hidden = true; dl.disabled = false; });
      } });
      var rm = x.h('button', { class: 'btn ghost', type: 'button', text: 'Forget everything saved on this phone', onclick: function () {
        x.cache.clear().then(function () { ['recent', 'v', 'fresh', 'full', 'home', 'etag', 'cart'].forEach(x.ls.del); x.clearCart(); x.S.full = false; x.S.v = null; x.S.fresh = 0; x.S.home = null; x.S.etag = null; x.renderHome(); x.renderRecent(); x.disconnect(); x.connect(); x.loadHome(); x.toast('Saved prices removed'); x.closeSheet(); });
      } });
      x.sheet(x.h('div', { class: 'menu' }, [x.h('h2', { id: 'sheetTitle', text: x.store().name }), status, prog, dl, rm]));
    }

    return { syncCatalog: syncCatalog, afterCatalogSync: afterCatalogSync, openMenu: openMenu };
  }
  window.PTOffline = { mount: mount };
})();
