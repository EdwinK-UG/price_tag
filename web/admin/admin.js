/* Shop admin: prices, offers, import, team, integrations, activity, settings.
 * Plain JS, no build step. All server text is inserted with textContent. */
(function () {
  'use strict';
  var slug = location.pathname.split('/')[2];
  var API = '/api/v1/s/' + encodeURIComponent(slug) + '/admin/';
  var C = window.PTCode;
  var app = document.getElementById('app');
  var S = { user: null, settings: null, timer: null };

  // ---- helpers ------------------------------------------------------------------------
  function h(tag, a, kids) {
    var e = document.createElement(tag), k, v;
    if (a) for (k in a) {
      v = a[k];
      if (v == null || v === false) continue;
      if (k === 'class') e.className = v; else if (k === 'text') e.textContent = v; else if (k === 'value') e.value = v;
      else if (k.slice(0, 2) === 'on') e.addEventListener(k.slice(2), v); else e.setAttribute(k, v === true ? '' : v);
    }
    if (kids != null) [].concat(kids).forEach(function (c) { if (c != null && c !== false) e.appendChild(typeof c === 'object' ? c : document.createTextNode(c)); });
    return e;
  }
  function pad(n) { return (n < 10 ? '0' : '') + n; }
  function toast(msg) {
    var t = document.getElementById('toast'); t.textContent = msg; t.hidden = false;
    clearTimeout(toast.t); toast.t = setTimeout(function () { t.hidden = true; }, 3600);
  }
  function api(method, path, body, o) {
    o = o || {};
    var init = { method: method, headers: {}, credentials: 'same-origin' };
    if (method !== 'GET') init.headers['X-PT-CSRF'] = '1';
    if (o.raw != null) { init.body = o.raw; init.headers['Content-Type'] = o.type || 'text/csv'; }
    else if (body !== undefined && body !== null) { init.body = JSON.stringify(body); init.headers['Content-Type'] = 'application/json'; }
    return fetch(API + path, init).then(function (res) {
      if (o.blob && res.ok) return res.blob();
      return res.text().then(function (t) {
        var j = null; try { j = t ? JSON.parse(t) : null; } catch (e) { }
        if (res.status === 401 && path !== 'login' && !o.quiet) { S.user = null; showLogin('Your session ended. Please sign in again.'); }
        if (!res.ok) { var err = new Error((j && j.message) || 'Something went wrong (' + res.status + ')'); err.status = res.status; err.code = j && j.error; throw err; }
        return j;
      });
    });
  }
  function fail(e) { if (e && e.status !== 401) toast(e.message || 'Something went wrong'); }

  function dec() { return S.settings ? S.settings.decimals | 0 : 0; }
  function fmt(v) {
    var d = dec(), s = String(Math.abs(v)), f = '';
    if (d) { while (s.length <= d) s = '0' + s; f = '.' + s.slice(-d); s = s.slice(0, -d); }
    return s.replace(/\B(?=(\d{3})+(?!\d))/g, ',') + f;
  }
  function parseAmount(text) {
    var d = dec(), s = String(text).replace(/[,\s ]/g, '').replace(/^[A-Za-z]+|[A-Za-z]+$/g, '');
    if (!/^\d+(\.\d+)?$/.test(s)) return null;
    var p = s.split('.'), frac = p[1] || '';
    if (frac.length > d) { if (/[1-9]/.test(frac.slice(d))) return null; frac = frac.slice(0, d); }
    frac += new Array(d - frac.length + 1).join('0');
    var n = Number(p[0] + frac);
    return Number.isSafeInteger(n) ? n : null;
  }
  function tz() { return S.settings ? S.settings.tz_offset | 0 : 180; }
  function toInput(unix) { var d = new Date((unix + tz() * 60) * 1000); return d.getUTCFullYear() + '-' + pad(d.getUTCMonth() + 1) + '-' + pad(d.getUTCDate()) + 'T' + pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes()); }
  function fromInput(str) { var m = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d)/.exec(str || ''); return m ? Math.floor(Date.UTC(+m[1], m[2] - 1, +m[3], +m[4], +m[5]) / 1000 - tz() * 60) : 0; }
  var MON = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  function when(unix) { var d = new Date((unix + tz() * 60) * 1000); return d.getUTCDate() + ' ' + MON[d.getUTCMonth()] + ' ' + d.getUTCFullYear() + ', ' + pad(d.getUTCHours()) + ':' + pad(d.getUTCMinutes()); }
  function now() { return Math.floor(Date.now() / 1000); }
  function field(label, input, hint) { return h('div', null, [h('label', null, [label, hint ? h('small', { text: ' ' + hint }) : null]), input]); }
  function debounce(fn, ms) { var t; return function () { var a = arguments, self = this; clearTimeout(t); t = setTimeout(function () { fn.apply(self, a); }, ms); }; }

  function modal(title, body, actions) {
    var m = h('div', { class: 'modal', role: 'dialog', 'aria-modal': 'true', onclick: function (e) { if (e.target === m) close(); } }, h('div', null, [h('h2', { text: title }), body, h('div', { class: 'bar' }, actions || [])]));
    function close() { m.remove(); document.removeEventListener('keydown', esc); }
    function esc(e) { if (e.key === 'Escape') close(); }
    document.addEventListener('keydown', esc);
    document.body.appendChild(m);
    var f = m.querySelector('input,select,textarea,button'); if (f) f.focus();
    m.close = close;
    return m;
  }
  function confirmBox(title, text, okLabel, danger) {
    return new Promise(function (resolve) {
      var m = modal(title, h('p', { class: 'sub', text: text }), [
        h('button', { class: 'btn' + (danger ? ' danger' : ' primary'), type: 'button', text: okLabel, onclick: function () { m.close(); resolve(true); } }),
        h('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: function () { m.close(); resolve(false); } })]);
    });
  }

  // ---- login ------------------------------------------------------------------------------
  function showLogin(note) {
    clearInterval(S.timer);
    app.textContent = '';
    var err = h('p', { class: 'msg bad', hidden: !note, text: note || '', role: 'alert' });
    var email = h('input', { type: 'email', autocomplete: 'username', required: true, id: 'email' });
    var pass = h('input', { type: 'password', autocomplete: 'current-password', required: true, id: 'pass' });
    var btn = h('button', { class: 'btn primary', type: 'submit', text: 'Sign in' });
    var form = h('form', { class: 'card', onsubmit: function (e) {
      e.preventDefault(); btn.disabled = true; err.hidden = true;
      api('POST', 'login', { email: email.value, password: pass.value }).then(function () { start(); }, function (x) { err.textContent = x.status === 429 ? 'Too many attempts. Wait a minute and try again.' : x.message; err.hidden = false; btn.disabled = false; pass.value = ''; pass.focus(); });
    } }, [err, field('E-mail', email), field('Password', pass), h('div', { class: 'bar' }, h('span', null)), btn]);
    app.appendChild(h('div', { class: 'login' }, [h('h2', { text: 'Shop admin' }), h('p', { class: 'sub', text: 'Sign in to manage prices and offers.' }), form,
      h('p', { class: 'sub', text: 'Forgot your password? Ask your shop owner, or the person who set up Price Tag for you, to reset it.' })]));
    email.focus();
  }

  // ---- shell + routing ------------------------------------------------------------------------
  var ROUTES = [['prices', 'Prices'], ['offers', 'Offers'], ['import', 'Import'], ['activity', 'Activity'], ['team', 'Team', 'owner'], ['keys', 'Integrations', 'owner'], ['settings', 'Settings']];
  var viewBox, navBox, viewerBox, nameBox;
  function start() {
    api('GET', 'me', null, { quiet: true }).then(function (me) {
      S.user = me.user; S.settings = me.settings;
      app.textContent = '';
      nameBox = h('h1', { text: S.settings.name });
      viewerBox = h('span', { class: 'viewers' });
      app.appendChild(h('header', { class: 'top' }, [nameBox, viewerBox,
        h('a', { href: '/s/' + encodeURIComponent(slug) + '/', target: '_blank', rel: 'noopener', text: 'Open shop page' }),
        h('button', { type: 'button', text: 'Sign out (' + S.user.name + ')', onclick: function () { api('POST', 'logout').then(function () { showLogin(); }, function () { showLogin(); }); } })]));
      navBox = h('nav', { 'aria-label': 'Sections' });
      ROUTES.forEach(function (r) { if (r[2] && S.user.role !== r[2]) return; navBox.appendChild(h('a', { href: '#' + r[0], 'data-r': r[0], text: r[1] })); });
      viewBox = h('main');
      app.appendChild(navBox); app.appendChild(viewBox);
      pollStats(); clearInterval(S.timer); S.timer = setInterval(function () { if (!document.hidden) pollStats(); }, 20000);
      route();
    }, function (e) { if (e.status === 401) showLogin(); else showLogin(e.message); });
  }
  function pollStats() { api('GET', 'stats').then(function (s) { if (viewerBox) viewerBox.textContent = s.live_viewers + ' shopper' + (s.live_viewers === 1 ? '' : 's') + ' connected now'; }, function () { }); }
  function route() {
    if (!S.user) return;
    var hash = location.hash.slice(1) || 'prices', parts = hash.split('/'), name = parts[0];
    [].forEach.call(navBox.children, function (a) { if (a.getAttribute('data-r') === (name === 'offer' ? 'offers' : name)) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current'); });
    viewBox.textContent = '';
    var views = { prices: viewPrices, offers: viewOffers, offer: function (b) { viewOfferEditor(b, parts[1]); }, import: viewImport, activity: viewActivity, team: viewTeam, keys: viewKeys, settings: viewSettings };
    (views[name] || viewPrices)(viewBox);
    window.scrollTo(0, 0);
  }
  window.addEventListener('hashchange', route);

  // ---- prices ------------------------------------------------------------------------------------
  function viewPrices(box) {
    var st = { q: '', offset: 0, total: 0 }, PAGE = 50;
    var list = h('div', { class: 'list' }), more = h('button', { class: 'btn', type: 'button', text: 'Show more', hidden: true }), count = h('p', { class: 'sub' });
    var search = h('input', { type: 'search', placeholder: 'Search by name, barcode or category', 'aria-label': 'Search products' });
    function load(reset) {
      if (reset) { st.offset = 0; list.textContent = ''; }
      api('GET', 'products?limit=' + PAGE + '&offset=' + st.offset + '&q=' + encodeURIComponent(st.q)).then(function (r) {
        st.total = r.total; r.items.forEach(function (p) { list.appendChild(productRow(p)); }); st.offset += r.items.length;
        count.textContent = r.total.toLocaleString() + ' product' + (r.total === 1 ? '' : 's') + (st.q ? ' match' : ' in your price list');
        more.hidden = st.offset >= r.total;
        if (!r.total) list.appendChild(h('p', { class: 'sub', text: st.q ? 'Nothing matches that search.' : 'Your price list is empty. Add a product, or import your price file from the Import tab.' }));
      }, fail);
    }
    search.addEventListener('input', debounce(function () { st.q = search.value.trim(); load(true); }, 250));
    more.addEventListener('click', function () { load(false); });
    box.appendChild(h('h2', { text: 'Prices' }));
    box.appendChild(h('p', { class: 'sub', text: 'Tap a price to change it. Shoppers see the new price on their phones within a second.' }));
    box.appendChild(h('div', { class: 'bar' }, [h('div', { class: 'grow' }, search), h('button', { class: 'btn primary', type: 'button', text: 'Add product', onclick: function () { productDialog(null, function () { load(true); }); } })]));
    box.appendChild(count); box.appendChild(list); box.appendChild(h('div', { class: 'bar' }, more));
    load(true);
  }

  function productRow(p) {
    var row = h('div', { class: 'item' });
    function paint() {
      row.textContent = '';
      row.appendChild(h('div', { class: 't' }, [h('b', { text: p.name + (p.size ? ' · ' + p.size : '') }), h('small', { text: p.display + (p.category ? ' · ' + p.category : '') })]));
      var price = h('div', { class: 'price' }, [h('button', { type: 'button', 'aria-label': 'Change price of ' + p.name, text: fmt(p.price), onclick: edit }),
        p.promo_price != null ? h('span', { class: 'deal', text: 'Offer: ' + fmt(p.promo_price) }) : null]);
      row.appendChild(price);
      row.appendChild(h('div', { class: 'acts' }, [
        h('button', { class: 'btn small', type: 'button', text: 'Edit details', onclick: function () { productDialog(p, function (np) { if (np) { Object.assign(p, np); paint(); } }); } }),
        h('button', { class: 'btn small', type: 'button', text: 'History', onclick: function () { historyDialog(p); } }),
        h('button', { class: 'btn small danger', type: 'button', text: 'Remove', onclick: function () {
          confirmBox('Remove ' + p.name + '?', 'It will disappear from the price list and shoppers will no longer see it. You can add it again later.', 'Remove', true).then(function (ok) {
            if (ok) api('DELETE', 'products/' + encodeURIComponent(p.code)).then(function () { row.remove(); toast('Removed.'); }, fail);
          });
        } })]));
    }
    function edit() {
      var input = h('input', { type: 'text', inputmode: 'decimal', value: fmt(p.price).replace(/,/g, ''), 'aria-label': 'New price' });
      var cell = row.querySelector('.price');
      cell.textContent = '';
      var save = h('button', { class: 'btn primary small', type: 'button', text: 'Save' }), cancel = h('button', { class: 'btn small', type: 'button', text: 'Cancel', onclick: paint });
      cell.appendChild(h('div', { class: 'edit' }, [input, save, cancel]));
      input.focus(); input.select();
      function commit() {
        var v = parseAmount(input.value);
        if (v == null) { toast('Enter the price as a number, for example 5500'); input.focus(); return; }
        if (v === p.price) { paint(); return; }
        save.disabled = true;
        api('PUT', 'products/' + encodeURIComponent(p.code) + '/price', { price: v }).then(function () {
          p.price = v; paint(); row.classList.add('flash');
          api('GET', 'stats').then(function (s) { toast('Price saved. ' + s.live_viewers + ' phone' + (s.live_viewers === 1 ? ' is' : 's are') + ' connected and see it now.'); viewerBox.textContent = s.live_viewers + ' shoppers connected now'; }, function () { toast('Price saved.'); });
        }, function (e) { save.disabled = false; fail(e); });
      }
      save.addEventListener('click', commit);
      input.addEventListener('keydown', function (e) { if (e.key === 'Enter') { e.preventDefault(); commit(); } else if (e.key === 'Escape') paint(); });
    }
    paint();
    return row;
  }

  function productDialog(p, done) {
    var isNew = !p, errBox = h('p', { class: 'msg bad', hidden: true, role: 'alert' });
    var code = h('input', { type: 'text', value: p ? p.display : '', readonly: !isNew, inputmode: 'numeric', autocomplete: 'off', placeholder: 'Scan or type the number under the barcode' });
    var name = h('input', { type: 'text', value: p ? p.name : '', maxlength: 200 });
    var size = h('input', { type: 'text', value: p ? p.size : '', maxlength: 40, placeholder: 'for example 1 kg' });
    var cat = h('input', { type: 'text', value: p ? p.category : '', maxlength: 60, placeholder: 'for example Pantry' });
    var price = h('input', { type: 'text', inputmode: 'decimal', value: p ? fmt(p.price).replace(/,/g, '') : '' });
    var codeErr = h('p', { class: 'err', hidden: true });
    code.addEventListener('input', function () {
      var c = code.value.trim() ? C.canonical(code.value) : { ok: true };
      codeErr.hidden = c.ok; if (!c.ok) codeErr.textContent = c.error === 'check' ? 'The last digit does not match: check the number against the barcode.' : 'This code is not valid.';
    });
    var save = h('button', { class: 'btn primary', type: 'button', text: isNew ? 'Add product' : 'Save changes' });
    var m = modal(isNew ? 'Add a product' : 'Edit product', h('div', null, [errBox, field('Barcode number', code), codeErr, field('Name', name), h('div', { class: 'grid2' }, [field('Size', size), field('Category', cat)]), field('Price (' + S.settings.currency + ')', price)]),
      [save, h('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: function () { m.close(); } })]);
    save.addEventListener('click', function () {
      var v = parseAmount(price.value);
      if (v == null) { errBox.textContent = 'Enter the price as a number, for example 5500.'; errBox.hidden = false; return; }
      save.disabled = true; errBox.hidden = true;
      api('PUT', 'products/' + encodeURIComponent(isNew ? code.value.trim() : p.code), { name: name.value, size: size.value, category: cat.value, price: v }).then(function (r) {
        m.close(); toast(isNew ? 'Product added.' : 'Saved.');
        done(isNew ? null : { name: name.value.trim(), size: size.value.trim(), category: cat.value.trim(), price: v });
      }, function (e) { errBox.textContent = e.message; errBox.hidden = false; save.disabled = false; });
    });
  }

  function historyDialog(p) {
    var body = h('div', null, h('p', { class: 'sub', text: 'Loading…' }));
    var m = modal('Price history: ' + p.name, body, [h('button', { class: 'btn', type: 'button', text: 'Close', onclick: function () { m.close(); } })]);
    api('GET', 'history?kind=product&ref=' + encodeURIComponent(p.code) + '&limit=30').then(function (r) {
      body.textContent = '';
      if (!r.entries.length) { body.appendChild(h('p', { class: 'sub', text: 'No changes recorded yet.' })); return; }
      var t = h('table', null, [h('tr', null, [h('th', { text: 'When' }), h('th', { text: 'What' }), h('th', { text: 'By' })])]);
      r.entries.forEach(function (e) {
        t.appendChild(h('tr', null, [h('td', { text: when(e.at) }), h('td', { text: e.summary + (e.old_price != null && e.new_price != null ? ': ' + fmt(e.old_price) + ' → ' + fmt(e.new_price) : e.new_price != null ? ': ' + fmt(e.new_price) : '') }), h('td', { text: e.actor })]));
      });
      body.appendChild(t);
    }, fail);
  }

  // ---- offers ----------------------------------------------------------------------------------------
  function viewOffers(box) {
    box.appendChild(h('h2', { text: 'Offers' }));
    box.appendChild(h('p', { class: 'sub', text: 'Live offers appear on your shop page home screen and on each product\'s price. They start and end by themselves.' }));
    box.appendChild(h('div', { class: 'bar' }, h('a', { class: 'btn primary', href: '#offer/new', text: 'New offer' })));
    var list = h('div', { class: 'list' }); box.appendChild(list);
    api('GET', 'promos').then(function (r) {
      if (!r.promos.length) list.appendChild(h('p', { class: 'sub', text: 'No offers yet. Create one to feature products on your shop page.' }));
      r.promos.forEach(function (p) {
        var row = h('div', { class: 'item' }, [
          h('div', { class: 't' }, [h('b', { text: p.title }), h('small', { text: when(p.starts_at) + ' to ' + when(p.ends_at) + ' · ' + p.item_count + ' product' + (p.item_count === 1 ? '' : 's') })]),
          h('span', { class: 'chip ' + p.status, text: { live: 'Live', scheduled: 'Scheduled', ended: 'Ended', disabled: 'Switched off' }[p.status] }),
          h('div', { class: 'acts' }, [
            h('a', { class: 'btn small', href: '#offer/' + p.id, text: 'Edit' }),
            p.status === 'live' ? h('button', { class: 'btn small', type: 'button', text: 'End now', onclick: function () { endNow(p); } }) : null,
            h('button', { class: 'btn small danger', type: 'button', text: 'Delete', onclick: function () {
              confirmBox('Delete "' + p.title + '"?', 'Prices go back to normal straight away.', 'Delete', true).then(function (ok) { if (ok) api('DELETE', 'promos/' + p.id).then(function () { row.remove(); toast('Offer deleted.'); }, fail); });
            } })])]);
        list.appendChild(row);
      });
    }, fail);
  }
  function endNow(p) {
    api('GET', 'promos/' + p.id).then(function (full) {
      var body = { title: full.title, text: full.text, starts_at: full.starts_at, ends_at: Math.max(full.starts_at + 1, now()), enabled: full.enabled, items: full.items.map(function (i) { return { code: i.code, price: i.price }; }) };
      return api('PUT', 'promos/' + p.id, body);
    }).then(function () { toast('Offer ended. Prices are back to normal.'); route(); }, fail);
  }

  function viewOfferEditor(box, id) {
    var isNew = id === 'new', offer = { title: '', text: '', starts_at: now() - (now() % 60), ends_at: now() - (now() % 60) + 7 * 86400, enabled: true, items: [] };
    box.appendChild(h('p', null, h('a', { href: '#offers', text: '← All offers' })));
    box.appendChild(h('h2', { text: isNew ? 'New offer' : 'Edit offer' }));
    function build() {
      var errBox = h('p', { class: 'msg bad', hidden: true, role: 'alert' });
      var title = h('input', { type: 'text', value: offer.title, maxlength: 80, placeholder: 'for example Weekend specials' });
      var text = h('textarea', { maxlength: 300, placeholder: 'Optional. A short line shoppers will see under the title.' }); text.value = offer.text;
      var from = h('input', { type: 'datetime-local', value: toInput(offer.starts_at) }), to = h('input', { type: 'datetime-local', value: toInput(offer.ends_at) });
      var enabled = h('input', { type: 'checkbox' }); enabled.checked = offer.enabled;
      var itemsBox = h('div', { class: 'list' }), summary = h('p', { class: 'sub' });
      var items = offer.items.map(function (i) { return { code: i.code, display: i.display, name: i.name, size: i.size, base: i.base_price, price: i.price }; });
      function paintItems() {
        itemsBox.textContent = '';
        summary.textContent = items.length ? items.length + ' product' + (items.length === 1 ? '' : 's') + ' in this offer' : 'No products yet: this will show as an announcement only.';
        items.forEach(function (it, idx) {
          var inp = h('input', { type: 'text', inputmode: 'decimal', value: String(fmt(it.price)).replace(/,/g, ''), 'aria-label': 'Offer price for ' + it.name });
          var off = h('small', { class: 'sub', text: '' });
          function upd() { var v = parseAmount(inp.value); if (v != null) it.price = v; off.textContent = v == null ? 'enter a number' : (v >= it.base ? 'must be lower than ' + fmt(it.base) : '−' + Math.round((1 - v / it.base) * 100) + '%'); off.className = v != null && v < it.base ? 'sub' : 'err'; }
          inp.addEventListener('input', upd); upd();
          itemsBox.appendChild(h('div', { class: 'item' }, [h('div', { class: 't' }, [h('b', { text: it.name + (it.size ? ' · ' + it.size : '') }), h('small', { text: 'Normal price ' + fmt(it.base) })]),
            h('div', { class: 'edit' }, [inp, h('button', { class: 'btn small danger', type: 'button', text: 'Remove', onclick: function () { items.splice(idx, 1); paintItems(); } })]), h('div', { class: 'acts' }, off)]));
        });
      }
      var q = h('input', { type: 'search', placeholder: 'Search products to add', 'aria-label': 'Search products to add' }), results = h('div', { class: 'list', hidden: true });
      q.addEventListener('input', debounce(function () {
        var s = q.value.trim(); results.textContent = '';
        if (s.length < 2) { results.hidden = true; return; }
        api('GET', 'products?limit=8&q=' + encodeURIComponent(s)).then(function (r) {
          results.hidden = false;
          if (!r.items.length) results.appendChild(h('p', { class: 'sub', text: 'No matching products.' }));
          r.items.forEach(function (p) {
            var added = items.some(function (i) { return i.code === p.code; });
            results.appendChild(h('div', { class: 'item' }, [h('div', { class: 't' }, [h('b', { text: p.name + (p.size ? ' · ' + p.size : '') }), h('small', { text: p.display + ' · ' + fmt(p.price) })]),
              h('button', { class: 'btn small', type: 'button', disabled: added, text: added ? 'Added' : 'Add', onclick: function () {
                var d = dec() ? 1 : 50, np = Math.max(0, Math.round(p.price * 0.9 / d) * d); if (np >= p.price) np = Math.max(0, p.price - 1);
                items.push({ code: p.code, display: p.display, name: p.name, size: p.size, base: p.price, price: np }); paintItems(); q.value = ''; results.hidden = true; q.focus();
              } })]));
          });
        }, fail);
      }, 250));
      var pct = h('input', { type: 'number', min: '1', max: '90', placeholder: '10', 'aria-label': 'Percent off' });
      var save = h('button', { class: 'btn primary', type: 'button', text: isNew ? 'Create offer' : 'Save offer' });
      save.addEventListener('click', function () {
        errBox.hidden = true;
        var body = { title: title.value, text: text.value, starts_at: fromInput(from.value), ends_at: fromInput(to.value), enabled: enabled.checked, items: items.map(function (i) { return { code: i.code, price: i.price }; }) };
        save.disabled = true;
        api(isNew ? 'POST' : 'PUT', isNew ? 'promos' : 'promos/' + id, body).then(function () { toast('Offer saved. It is live for shoppers when its start time arrives.'); location.hash = '#offers'; },
          function (e) { errBox.textContent = e.message; errBox.hidden = false; save.disabled = false; window.scrollTo(0, 0); });
      });
      var card = h('div', { class: 'card' }, [errBox, field('Offer title', title), field('Description', text, '(optional)'),
        h('div', { class: 'grid2' }, [field('Starts', from, '(shop time)'), field('Ends', to, '(shop time)')]),
        h('label', null, [enabled, ' Switched on']),
        h('h3', { text: 'Products in this offer' }), field('Add a product', q), results, summary, itemsBox,
        h('div', { class: 'bar' }, [h('span', { text: 'Take' }), h('div', null, pct), h('span', { text: '% off everything above' }), h('button', { class: 'btn small', type: 'button', text: 'Apply', onclick: function () {
          var p = Number(pct.value); if (!(p > 0 && p < 100)) { toast('Enter a percentage between 1 and 99'); return; }
          var d = dec() ? 1 : 50; items.forEach(function (it) { var np = Math.round(it.base * (1 - p / 100) / d) * d; it.price = np >= it.base ? it.base - 1 : Math.max(0, np); }); paintItems();
        } })]),
        h('div', { class: 'bar' }, [save, h('a', { class: 'btn', href: '#offers', text: 'Cancel' })])]);
      box.appendChild(card); paintItems();
    }
    if (isNew) build(); else api('GET', 'promos/' + encodeURIComponent(id)).then(function (p) { offer = p; build(); }, function (e) { fail(e); location.hash = '#offers'; });
  }

  // ---- import / export -----------------------------------------------------------------------------------
  function viewImport(box) {
    box.appendChild(h('h2', { text: 'Import & export' }));
    box.appendChild(h('p', { class: 'sub', text: 'Load your whole price list from a spreadsheet (CSV). Existing products are updated by barcode, new ones are added, nothing is deleted.' }));
    var file = h('input', { type: 'file', accept: '.csv,text/csv,text/plain' }), out = h('div'), applyBtn = h('button', { class: 'btn primary', type: 'button', text: 'Apply these changes', hidden: true });
    var chosen = null;
    function report(r, applied) {
      out.textContent = '';
      var s = h('div', { class: 'stats' }, [stat(r.created, 'new'), stat(r.updated + r.restored, 'changed'), stat(r.unchanged, 'unchanged'), stat(r.errors.length, 'problems')]);
      out.appendChild(h('p', { class: 'msg ' + (applied ? 'ok' : 'warn'), text: applied ? 'Done. Changes are live for shoppers now.' : 'Preview only: nothing has been saved yet.' }));
      out.appendChild(s);
      if (r.errors.length) {
        var t = h('table', null, [h('tr', null, [h('th', { text: 'Row' }), h('th', { text: 'Barcode' }), h('th', { text: 'Problem' })])]);
        r.errors.slice(0, 50).forEach(function (e) { t.appendChild(h('tr', null, [h('td', { text: String(e.row) }), h('td', { text: e.code || '' }), h('td', { text: e.message })])); });
        out.appendChild(h('div', { class: 'card' }, [h('b', { text: 'Rows that were skipped' + (r.errors.length > 50 ? ' (first 50)' : '') }), t]));
      }
    }
    function stat(n, l) { return h('div', { class: 'stat' }, [h('b', { text: n.toLocaleString() }), h('span', { text: l })]); }
    function run(dry) {
      if (!chosen) return;
      api('POST', 'import' + (dry ? '?dry_run=1' : ''), null, { raw: chosen, type: 'text/csv' }).then(function (r) {
        report(r, !dry); applyBtn.hidden = !dry || (r.created + r.updated + r.restored === 0);
        if (!dry) { pollStats(); file.value = ''; chosen = null; }
      }, function (e) { out.textContent = ''; out.appendChild(h('p', { class: 'msg bad', text: e.message })); applyBtn.hidden = true; });
    }
    file.addEventListener('change', function () { chosen = file.files[0] || null; applyBtn.hidden = true; if (chosen) run(true); });
    applyBtn.addEventListener('click', function () { applyBtn.disabled = true; run(false); setTimeout(function () { applyBtn.disabled = false; }, 800); });
    box.appendChild(h('div', { class: 'card' }, [h('h3', { text: '1. Choose your price file' }), file, h('p', { class: 'sub', text: 'Needs a barcode, a name and a price column. Headers such as "Barcode", "Item Description" and "Selling Price" are recognised. Prices like 12,500 or UGX 12500 are fine.' }),
      h('button', { class: 'btn small', type: 'button', text: 'Download a template', onclick: function () { download(new Blob(['barcode,name,size,category,price\n6001234567893,Sugar,1 kg,Pantry,5500\n'], { type: 'text/csv' }), 'price-template.csv'); } }),
      h('h3', { text: '2. Check the preview' }), out, applyBtn]));
    var exp = [h('button', { class: 'btn', type: 'button', text: 'Download price list (CSV)', onclick: function () { api('GET', 'export.csv', null, { blob: true }).then(function (b) { download(b, slug + '-prices.csv'); }, fail); } })];
    if (S.user.role === 'owner') exp.push(h('button', { class: 'btn', type: 'button', text: 'Download everything (database file)', onclick: function () { api('GET', 'export.db', null, { blob: true }).then(function (b) { download(b, slug + '.db'); }, fail); } }));
    box.appendChild(h('div', { class: 'card' }, [h('h3', { text: 'Your data is yours' }), h('p', { class: 'sub', text: 'Download your prices at any time. The database file contains everything this shop has stored (products, offers, history, team) and opens with any SQLite tool.' }), h('div', { class: 'bar' }, exp)]));
  }
  function download(blob, name) { var a = h('a', { href: URL.createObjectURL(blob), download: name }); document.body.appendChild(a); a.click(); setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000); }

  // ---- activity -------------------------------------------------------------------------------------------------
  function viewActivity(box) {
    box.appendChild(h('h2', { text: 'Activity' })); box.appendChild(h('p', { class: 'sub', text: 'Who changed what, and when.' }));
    var kind = h('select', { 'aria-label': 'Type of activity' }, [['', 'Everything'], ['product', 'Prices & products'], ['promo', 'Offers'], ['import', 'Imports'], ['user', 'Team'], ['token', 'Integrations'], ['settings', 'Settings']].map(function (k) { return h('option', { value: k[0], text: k[1] }); }));
    var list = h('div', { class: 'list' }), more = h('button', { class: 'btn', type: 'button', text: 'Show more', hidden: true }), before = 0;
    function load(reset) {
      if (reset) { before = 0; list.textContent = ''; }
      api('GET', 'history?limit=40&kind=' + encodeURIComponent(kind.value) + (before ? '&before=' + before : '')).then(function (r) {
        r.entries.forEach(function (e) {
          list.appendChild(h('div', { class: 'item' }, [h('div', { class: 't' }, [h('b', { text: e.summary + (e.old_price != null && e.new_price != null ? ': ' + fmt(e.old_price) + ' → ' + fmt(e.new_price) : '') }), h('small', { text: when(e.at) + ' · ' + e.actor })])]));
          before = e.id;
        });
        more.hidden = r.entries.length < 40;
        if (!list.children.length) list.appendChild(h('p', { class: 'sub', text: 'Nothing here yet.' }));
      }, fail);
    }
    kind.addEventListener('change', function () { load(true); }); more.addEventListener('click', function () { load(false); });
    box.appendChild(h('div', { class: 'bar' }, kind)); box.appendChild(list); box.appendChild(h('div', { class: 'bar' }, more)); load(true);
  }

  // ---- team ---------------------------------------------------------------------------------------------------------
  function viewTeam(box) {
    box.appendChild(h('h2', { text: 'Team' }));
    box.appendChild(h('p', { class: 'sub', text: 'Owners can do everything. Editors can change prices, products and offers.' }));
    var list = h('div', { class: 'list' });
    function load() {
      api('GET', 'users').then(function (r) {
        list.textContent = '';
        r.users.forEach(function (u) {
          list.appendChild(h('div', { class: 'item' }, [h('div', { class: 't' }, [h('b', { text: u.name + (u.id === S.user.id ? ' (you)' : '') }), h('small', { text: u.email + ' · ' + u.role + (u.disabled ? ' · switched off' : '') })]),
            h('span', { class: 'chip ' + (u.disabled ? '' : 'live'), text: u.disabled ? 'Off' : 'Active' }),
            h('div', { class: 'acts' }, [
              h('button', { class: 'btn small', type: 'button', text: u.disabled ? 'Switch on' : 'Switch off', onclick: function () { api('PATCH', 'users/' + u.id, { disabled: !u.disabled }).then(load, fail); } }),
              h('button', { class: 'btn small', type: 'button', text: u.role === 'owner' ? 'Make editor' : 'Make owner', onclick: function () { api('PATCH', 'users/' + u.id, { role: u.role === 'owner' ? 'editor' : 'owner' }).then(load, fail); } }),
              h('button', { class: 'btn small', type: 'button', text: 'Set new password', onclick: function () { newPassword(u); } })])]));
        });
      }, fail);
    }
    function newPassword(u) {
      var pw = h('input', { type: 'password', autocomplete: 'new-password', placeholder: 'At least 8 characters' }), err = h('p', { class: 'msg bad', hidden: true });
      var m = modal('New password for ' + u.name, h('div', null, [err, field('New password', pw), h('p', { class: 'sub', text: 'They will be signed out everywhere.' })]), [
        h('button', { class: 'btn primary', type: 'button', text: 'Save', onclick: function () { api('PATCH', 'users/' + u.id, { password: pw.value }).then(function () { m.close(); toast('Password changed.'); }, function (e) { err.textContent = e.message; err.hidden = false; }); } }),
        h('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: function () { m.close(); } })]);
    }
    var email = h('input', { type: 'email' }), name = h('input', { type: 'text', maxlength: 80 }), role = h('select', null, [h('option', { value: 'editor', text: 'Editor' }), h('option', { value: 'owner', text: 'Owner' })]), pw = h('input', { type: 'text', placeholder: 'Leave empty to generate one', autocomplete: 'off' }), err = h('p', { class: 'msg bad', hidden: true }), note = h('p', { class: 'msg ok', hidden: true });
    var add = h('button', { class: 'btn primary', type: 'button', text: 'Add team member', onclick: function () {
      err.hidden = note.hidden = true;
      api('POST', 'users', { email: email.value, name: name.value, role: role.value, password: pw.value }).then(function (r) {
        note.textContent = 'Added ' + r.user.name + '.' + (r.password ? ' Their password is: ' + r.password + ' (shown once; ask them to change it).' : ''); note.hidden = false;
        email.value = name.value = pw.value = ''; load();
      }, function (e) { err.textContent = e.message; err.hidden = false; });
    } });
    box.appendChild(list);
    box.appendChild(h('div', { class: 'card' }, [h('h3', { text: 'Add a team member' }), err, note, h('div', { class: 'grid2' }, [field('Name', name), field('E-mail', email)]), h('div', { class: 'grid2' }, [field('Role', role), field('Password', pw)]), h('div', { class: 'bar' }, add)]));
    load();
  }

  // ---- integrations ----------------------------------------------------------------------------------------------------
  function viewKeys(box) {
    var origin = location.origin, url = origin + '/api/v1/s/' + encodeURIComponent(slug) + '/ingest';
    box.appendChild(h('h2', { text: 'Connect your till or back-office system' }));
    box.appendChild(h('p', { class: 'sub', text: 'An integration key lets your POS or ERP send price changes here automatically. Keys can only update products. They cannot read your team, export data or sign in.' }));
    var list = h('div', { class: 'list' }), shown = h('div');
    function load() {
      api('GET', 'tokens').then(function (r) {
        list.textContent = '';
        if (!r.tokens.length) list.appendChild(h('p', { class: 'sub', text: 'No keys yet.' }));
        r.tokens.forEach(function (t) {
          list.appendChild(h('div', { class: 'item' }, [h('div', { class: 't' }, [h('b', { text: t.name }), h('small', { text: 'Created ' + when(t.created_at) + (t.last_used ? ' · last used ' + when(t.last_used) : ' · never used') })]),
            h('span', { class: 'chip ' + (t.revoked ? '' : 'live'), text: t.revoked ? 'Revoked' : 'Active' }),
            t.revoked ? null : h('div', { class: 'acts' }, h('button', { class: 'btn small danger', type: 'button', text: 'Revoke', onclick: function () { confirmBox('Revoke "' + t.name + '"?', 'Anything using this key will stop working immediately.', 'Revoke', true).then(function (ok) { if (ok) api('DELETE', 'tokens/' + t.id).then(load, fail); }); } }))]));
        });
      }, fail);
    }
    var nm = h('input', { type: 'text', maxlength: 60, placeholder: 'for example Nightly price export' });
    box.appendChild(list);
    box.appendChild(h('div', { class: 'card' }, [h('h3', { text: 'Create a key' }), field('Name', nm), h('div', { class: 'bar' }, h('button', { class: 'btn primary', type: 'button', text: 'Create key', onclick: function () {
      api('POST', 'tokens', { name: nm.value }).then(function (r) {
        nm.value = ''; shown.textContent = '';
        shown.appendChild(h('div', { class: 'card' }, [h('p', { class: 'msg warn', text: 'Copy this key now. For your safety it is shown only once.' }), h('div', { class: 'copy', text: r.token }),
          h('h3', { text: 'Send your price file (CSV or JSON)' }), h('div', { class: 'copy', text: 'curl -X POST -H "Authorization: Bearer ' + r.token + '" -H "Content-Type: text/csv" --data-binary @prices.csv ' + url }),
          h('p', { class: 'sub', text: 'Send the full file as often as you like (for example every night, or after every price change). Unchanged rows are ignored, so only real changes reach shoppers. Add ?dry_run=1 to the address to test without saving.' })]));
        load();
      }, fail);
    } }))]));
    box.appendChild(shown); load();
  }

  // ---- settings ------------------------------------------------------------------------------------------------------------
  var TZ = [[-480, 'UTC−8'], [-300, 'UTC−5'], [0, 'UTC (London, Accra)'], [60, 'UTC+1 (Lagos)'], [120, 'UTC+2 (Kigali, Lusaka, Johannesburg)'], [180, 'UTC+3 (Uganda, Kenya, Tanzania)'], [240, 'UTC+4'], [330, 'UTC+5:30'], [480, 'UTC+8']];
  function viewSettings(box) {
    var set = S.settings, owner = S.user.role === 'owner';
    box.appendChild(h('h2', { text: 'Settings' }));
    var err = h('p', { class: 'msg bad', hidden: true });
    var name = h('input', { type: 'text', value: set.name, maxlength: 80, disabled: !owner }), ann = h('textarea', { maxlength: 200, disabled: !owner, placeholder: 'Optional. For example: Open 8am to 10pm. Free parking.' }); ann.value = set.announcement || '';
    var tzs = h('select', { disabled: !owner }, TZ.map(function (z) { return h('option', { value: String(z[0]), text: z[1], selected: z[0] === set.tz_offset }); }));
    var cur = h('input', { type: 'text', value: set.currency, maxlength: 3, disabled: true }), decs = h('input', { type: 'number', value: set.decimals, disabled: true });
    box.appendChild(h('div', { class: 'card' }, [h('h3', { text: 'Shop details' }), err, field('Shop name', name), field('Announcement', ann, '(shown at the top of your shop page)'), field('Time zone', tzs),
      h('div', { class: 'grid2' }, [field('Currency', cur, '(fixed once you have products)'), field('Decimal places', decs)]),
      owner ? h('div', { class: 'bar' }, h('button', { class: 'btn primary', type: 'button', text: 'Save', onclick: function () {
        err.hidden = true;
        api('PUT', 'settings', { name: name.value, currency: set.currency, decimals: set.decimals, tz_offset: Number(tzs.value), announcement: ann.value }).then(function (s) { S.settings = s; nameBox.textContent = s.name; toast('Saved.'); }, function (e) { err.textContent = e.message; err.hidden = false; });
      } })) : h('p', { class: 'sub', text: 'Only an owner can change these.' })]));
    // shop link & QR poster
    var link = location.origin + '/s/' + encodeURIComponent(slug) + '/', qrBox = h('div', { class: 'poster' });
    function qrSvg(into) {
      var qr = qrcode(0, 'M'); qr.addData(link); qr.make();
      var doc = new DOMParser().parseFromString(qr.createSvgTag({ cellSize: 6, margin: 2, scalable: true }), 'image/svg+xml');
      into.appendChild(document.importNode(doc.documentElement, true));
    }
    qrSvg(qrBox);
    box.appendChild(h('div', { class: 'card' }, [h('h3', { text: 'Your shop link and entrance poster' }), h('p', { class: 'sub', text: 'Shoppers scan this code with their phone camera to open your live price checker. Print it for the entrance, the shelves and the tills.' }), h('div', { class: 'copy', text: link }), qrBox,
      h('div', { class: 'bar' }, h('button', { class: 'btn', type: 'button', text: 'Print poster', onclick: printPoster }))]));
    function printPoster() {
      var area = document.getElementById('printarea') || document.body.appendChild(h('div', { id: 'printarea' }));
      area.textContent = '';
      area.appendChild(h('h1', { text: set.name })); area.appendChild(h('p', { text: 'Scan any product to see its price. Scan this code to start.' })); qrSvg(area); area.appendChild(h('p', { text: link }));
      window.print();
    }
    // password
    var cp = h('input', { type: 'password', autocomplete: 'current-password' }), np = h('input', { type: 'password', autocomplete: 'new-password', placeholder: 'At least 8 characters' }), perr = h('p', { class: 'msg bad', hidden: true });
    box.appendChild(h('div', { class: 'card' }, [h('h3', { text: 'Change my password' }), perr, field('Current password', cp), field('New password', np), h('div', { class: 'bar' }, h('button', { class: 'btn', type: 'button', text: 'Change password', onclick: function () {
      perr.hidden = true;
      api('POST', 'password', { current: cp.value, new: np.value }).then(function () { cp.value = np.value = ''; toast('Password changed.'); }, function (e) { perr.textContent = e.message; perr.hidden = false; });
    } }))]));
  }

  start(); // shows the login form when there is no session
})();
