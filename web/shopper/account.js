/* Shopper account (lazy-loaded). One Price Tag account works in every shop.
 * A shop sees nothing about the shopper until they tap "Join" in that shop;
 * receiving offers is a separate choice, off by default. Plain ES2017. */
(function () {
  'use strict';
  var CH = { sms: 'SMS', whatsapp: 'WhatsApp', email: 'E-mail' }, CHs = { sms: 'SMS', whatsapp: 'WhatsApp', email: 'e-mail' }; // label / in a sentence

  function mount(host) {
    var h = host.h, st = { shopper: null, member: null, channels: {}, shop: '' }, pending = null, busy = false, err = '';

    function api(method, path, body) {
      var init = { method: method, headers: {}, credentials: 'same-origin' };
      if (method !== 'GET') { init.headers['X-PT-CSRF'] = '1'; init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body || {}); }
      return fetch(host.api + 'account' + path, init).then(function (r) {
        return r.text().then(function (t) {
          var j = null; try { j = t ? JSON.parse(t) : null; } catch (e) { }
          if (!r.ok) { var e2 = new Error((j && j.message) || 'Something went wrong (' + r.status + ')'); e2.status = r.status; e2.code = j && j.error; throw e2; }
          return j;
        });
      });
    }
    function remember() {
      try { localStorage.setItem('pt:signed', st.shopper ? '1' : '0'); } catch (e) { } // one account for every shop
      host.ls.set('member', st.member ? '1' : '0');
      host.onChange();
    }
    function refresh() { return api('GET', '').then(function (j) { st = j; remember(); return j; }); }
    function fail(e) { busy = false; err = (e && e.message) || 'Something went wrong. Check your connection.'; render(); }
    function title(t) { return h('h2', { id: 'sheetTitle', class: 'acc-title', text: t }); }
    function note(t) { return h('p', { class: 'sub', text: t }); }
    function errBox() { return err ? h('p', { class: 'acc-err', role: 'alert', text: err }) : null; }
    function btn(text, fn, ghost) { return h('button', { class: 'btn' + (ghost ? ' ghost' : ''), type: 'button', text: text, disabled: busy, onclick: fn }); }
    function contactOf(s) { return s.channel === 'email' ? s.email : (s.phone || s.email); }

    // 1. enter a phone number or e-mail
    function viewStart(adding) {
      var input = h('input', { class: 'acc-in', type: 'text', inputmode: 'email', autocomplete: 'tel email', placeholder: '0772 123456 or you@example.com', 'aria-label': 'Phone number or e-mail' });
      var via = 'sms', viaBox = h('div', { class: 'acc-via', role: 'radiogroup', 'aria-label': 'Send the code by', hidden: true });
      function paintVia() {
        viaBox.textContent = '';
        ['sms', 'whatsapp'].forEach(function (c) {
          if (!st.channels[c]) return;
          var r = h('input', { type: 'radio', name: 'via', value: c, checked: via === c });
          r.addEventListener('change', function () { via = c; });
          viaBox.appendChild(h('label', null, [r, ' Send the code by ' + CH[c]]));
        });
      }
      if (!st.channels.sms && st.channels.whatsapp) via = 'whatsapp';
      paintVia();
      input.addEventListener('input', function () { viaBox.hidden = input.value.indexOf('@') >= 0 || !/\d/.test(input.value) || !(st.channels.sms || st.channels.whatsapp); });
      var phoneOK = st.channels.sms || st.channels.whatsapp;
      return [title(adding ? 'Add a phone number or e-mail' : 'Sign in or create an account'),
        note(adding ? 'We will send a code to confirm it is yours.' : 'One Price Tag account works in every shop. We send you a code; there is no password to remember.'),
        phoneOK ? null : note('This shop can only send codes by e-mail at the moment.'),
        errBox(), input, viaBox,
        btn(busy ? 'Sending…' : 'Send code', function () {
          busy = true; err = '';  render();
          api('POST', '/start', { contact: input.value, via: via }).then(function (j) { busy = false; pending = { contact: j.contact.value, to: j.to, via: j.contact.kind === 'email' ? 'email' : via }; render(); }, fail);
        })];
    }
    // 2. enter the code
    function viewCode() {
      var code = h('input', { class: 'acc-in acc-code', type: 'text', inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: '6', placeholder: '6-digit code', 'aria-label': 'Code' });
      return [title('Enter your code'), note('We sent a 6-digit code by ' + CHs[pending.via] + ' to ' + pending.to + '. It expires in 10 minutes.'), errBox(), code,
        btn(busy ? 'Checking…' : 'Continue', function () {
          busy = true; err = ''; render();
          api('POST', '/verify', { contact: pending.contact, code: code.value }).then(function (j) {
            busy = false; pending = null;
            return refresh().then(function () { stage = j.created || !st.shopper.name ? 'profile' : (st.member ? 'home' : 'join'); render(); });
          }, fail);
        }),
        btn('Use a different number or e-mail', function () { pending = null; err = ''; render(); }, true)];
    }
    // 3. name + how shops should contact you
    function viewProfile() {
      var s = st.shopper, name = h('input', { class: 'acc-in', type: 'text', maxlength: '60', autocomplete: 'name', placeholder: 'Your name (optional)', 'aria-label': 'Your name', value: s.name || '' });
      name.value = s.name || '';
      var ch = s.channel, opts = h('div', { class: 'acc-via', role: 'radiogroup', 'aria-label': 'How shops may message you' });
      ['sms', 'whatsapp', 'email'].forEach(function (c) {
        var has = c === 'email' ? !!s.email : !!s.phone;
        var r = h('input', { type: 'radio', name: 'ch', value: c, checked: ch === c, disabled: !has });
        r.addEventListener('change', function () { ch = c; });
        opts.appendChild(h('label', { class: has ? '' : 'off' }, [r, ' ' + CH[c] + (has ? '' : c === 'email' ? ' (add an e-mail first)' : ' (add a phone number first)')]));
      });
      return [title('Your details'), errBox(), name, h('p', { class: 'acc-lbl', text: 'If you choose to get offers from a shop, send them by:' }), opts,
        btn('Save', function () {
          busy = true; err = ''; render();
          api('POST', '/profile', { name: name.value, channel: ch }).then(function () { busy = false; return refresh(); }).then(function () { stage = st.member ? 'home' : 'join'; render(); }, fail);
        }),
        btn(s.email && s.phone ? 'Cancel' : (s.email ? 'Add a phone number' : 'Add an e-mail address'), function () { if (s.email && s.phone) { stage = 'home'; render(); } else { stage = 'add'; render(); } }, true)];
    }
    // 4. consent to share with THIS shop
    function viewJoin() {
      var s = st.shopper, offers = h('input', { type: 'checkbox', id: 'accOffers' });
      return [title('Join ' + st.shop + '?'),
        h('p', { class: 'acc-share', text: st.shop + ' will see your name' + (s.name ? ' (' + s.name + ')' : '') + ' and ' + contactOf(s) + '. Other shops will not.' }),
        h('label', { class: 'acc-chk', for: 'accOffers' }, [offers, ' Send me ' + st.shop + '\'s offers by ' + CHs[s.channel] + ' (you can stop any time)']),
        errBox(),
        btn('Join ' + st.shop, function () {
          busy = true; err = ''; render();
          api('POST', '/join', { marketing: offers.checked }).then(function () { busy = false; return refresh(); }).then(function () { stage = 'home'; render(); host.toast('You joined ' + st.shop); }, fail);
        }),
        btn('Not now', function () { stage = 'home'; render(); }, true)];
    }
    // 5. signed in overview
    var shops = null;
    function viewHome() {
      var s = st.shopper, m = st.member, nodes = [title(s.name ? 'Hello, ' + s.name : 'Your account')];
      nodes.push(h('dl', { class: 'acc-dl' }, [
        h('dt', { text: 'Phone' }), h('dd', { text: s.phone || 'not added' }),
        h('dt', { text: 'E-mail' }), h('dd', { text: s.email || 'not added' }),
        h('dt', { text: 'Messages by' }), h('dd', { text: CH[s.channel] || '-' })]));
      nodes.push(errBox());
      if (m) {
        var t = h('input', { type: 'checkbox', id: 'accMk', checked: m.marketing });
        t.addEventListener('change', function () { api('POST', '/marketing', { on: t.checked }).then(refresh).then(function () { host.toast(t.checked ? 'Offers turned on' : 'Offers turned off'); }, fail); });
        nodes.push(h('section', { class: 'acc-shop' }, [h('h3', { text: st.shop }), note('You are a member. ' + st.shop + ' can see your name and ' + contactOf(s) + '.'),
          h('label', { class: 'acc-chk', for: 'accMk' }, [t, ' Get offers by ' + CHs[s.channel]]),
          btn('Leave ' + st.shop, function () { api('POST', '/leave').then(refresh).then(function () { render(); host.toast('You left ' + st.shop + '. It can no longer see your details.'); }, fail); }, true)]));
      } else nodes.push(h('section', { class: 'acc-shop' }, [h('h3', { text: st.shop }), note('You have not joined this shop. It cannot see anything about you.'), btn('Join ' + st.shop, function () { stage = 'join'; render(); })]));
      var list = h('ul', { class: 'acc-shops' });
      if (shops === null) api('GET', '/shops').then(function (j) { shops = j.shops; render(); }, function () { });
      (shops || []).forEach(function (x) { list.appendChild(h('li', null, [h('b', { text: x.name }), h('small', { text: x.marketing ? ' · offers on' : ' · offers off' })])); });
      if (shops && shops.length) nodes.push(h('h3', { class: 'acc-h', text: 'Shops that can see your details' }), list);
      nodes.push(btn('Edit details', function () { stage = 'profile'; render(); }, true));
      if (!s.phone || !s.email) nodes.push(btn(s.phone ? 'Add an e-mail address' : 'Add a phone number', function () { stage = 'add'; render(); }, true));
      nodes.push(btn('Sign out', function () { api('POST', '/logout').then(refresh).then(function () { shops = null; stage = 'start'; host.closeSheet(); host.toast('Signed out'); }, fail); }, true));
      nodes.push(confirmDel ? h('div', { class: 'acc-del', role: 'alert' }, [note('Delete your account? Every shop loses access to your details. This cannot be undone.'),
        btn('Yes, delete my account', function () { api('POST', '/delete').then(refresh).then(function () { confirmDel = false; shops = null; stage = 'start'; host.closeSheet(); host.toast('Account deleted'); }, fail); }),
        btn('Cancel', function () { confirmDel = false; render(); }, true)])
        : h('button', { class: 'link acc-del-link', type: 'button', text: 'Delete my account', onclick: function () { confirmDel = true; render(); } }));
      return nodes;
    }

    var stage = 'start', confirmDel = false;
    function render() {
      var nodes;
      if (!st.shopper) nodes = pending ? viewCode() : viewStart(false);
      else if (pending) nodes = viewCode();
      else if (stage === 'add') nodes = viewStart(true);
      else if (stage === 'profile') nodes = viewProfile();
      else if (stage === 'join' && !st.member) nodes = viewJoin();
      else nodes = viewHome();
      host.sheet(h('div', { class: 'acc' }, nodes));
      var f = document.querySelector('.acc .acc-in'); if (f && !busy) f.focus();
    }
    function open() {
      host.openSheet(); err = ''; pending = null; confirmDel = false;
      host.sheet(h('div', { class: 'big' }, h('div', { class: 'spin', role: 'status', 'aria-label': 'Loading' })));
      refresh().then(function () { stage = st.shopper ? (st.member ? 'home' : 'join') : 'start'; shops = null; render(); }, fail);
    }
    return { open: open };
  }
  window.PTAccount = { mount: mount };
})();
