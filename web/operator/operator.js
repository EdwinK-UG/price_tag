/* Operator console: approve shop registrations, set each shop's SMS / WhatsApp
 * monthly allowance (the shop pays), and see outgoing messages. */
(function () {
  'use strict';
  var app = document.getElementById('app'), KEY = 'pt_operator_token';
  function h(tag, a, kids) {
    var e = document.createElement(tag), k, v;
    if (a) for (k in a) { v = a[k]; if (v == null || v === false) continue; if (k === 'class') e.className = v; else if (k === 'text') e.textContent = v; else if (k === 'value') e.value = v; else if (k.slice(0, 2) === 'on') e.addEventListener(k.slice(2), v); else e.setAttribute(k, v === true ? '' : v); }
    if (kids != null) [].concat(kids).forEach(function (c) { if (c != null && c !== false) e.appendChild(typeof c === 'object' ? c : document.createTextNode(c)); });
    return e;
  }
  function token() { try { return sessionStorage.getItem(KEY) || ''; } catch (e) { return ''; } }
  function toast(m) { var t = document.getElementById('toast'); t.textContent = m; t.hidden = false; clearTimeout(toast.t); toast.t = setTimeout(function () { t.hidden = true; }, 3500); }
  function api(method, path, body) {
    var init = { method: method, headers: { Authorization: 'Bearer ' + token() } };
    if (body !== undefined) { init.headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(body); }
    return fetch('/api/v1/operator/' + path, init).then(function (r) {
      return r.text().then(function (t) {
        var j = null; try { j = t ? JSON.parse(t) : null; } catch (e) { }
        if (r.status === 401 || r.status === 404 && !j) { gate('That operator token was not accepted.'); throw new Error('unauthorized'); }
        if (!r.ok) throw new Error((j && j.message) || 'Request failed (' + r.status + ')');
        return j;
      });
    });
  }
  function when(u) { var d = new Date(u * 1000); return d.toLocaleDateString() + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); }

  function gate(msg) {
    try { sessionStorage.removeItem(KEY); } catch (e) { }
    app.textContent = '';
    var inp = h('input', { type: 'password', id: 'tok', autocomplete: 'off', placeholder: 'Operator token' });
    app.appendChild(h('div', { class: 'login' }, [h('h2', { text: 'Price Tag operator' }), msg ? h('p', { class: 'msg bad', text: msg }) : null,
      h('form', { class: 'card', onsubmit: function (e) { e.preventDefault(); try { sessionStorage.setItem(KEY, inp.value.trim()); } catch (x) { } start(); } },
        [h('label', { for: 'tok', text: 'Operator token (PRICETAG_OPERATOR_TOKEN on the server)' }), inp, h('div', { class: 'bar' }, h('button', { class: 'btn primary', type: 'submit', text: 'Open' }))])]));
    inp.focus();
  }

  var view = 'applications', main;
  function start() {
    if (!token()) { gate(); return; }
    api('GET', 'status').then(function (st) {
      app.textContent = '';
      var nav = h('nav', { 'aria-label': 'Sections' });
      [['applications', 'Registrations'], ['shops', 'Shops & message allowance'], ['outbox', 'Messages']].forEach(function (v) {
        nav.appendChild(h('a', { href: '#', 'aria-current': view === v[0] ? 'page' : null, text: v[1], onclick: function (e) { e.preventDefault(); view = v[0]; start(); } }));
      });
      main = h('main');
      var dev = Object.keys(st.delivering).filter(function (k) { return /development/.test(st.delivering[k]); });
      app.appendChild(h('header', { class: 'top' }, [h('h1', { text: 'Price Tag operator' }), h('button', { type: 'button', text: 'Lock', onclick: function () { gate(); } })]));
      app.appendChild(nav); app.appendChild(main);
      if (dev.length) main.appendChild(h('p', { class: 'msg warn', text: 'Development mode for ' + dev.join(', ') + ': those messages are NOT delivered. They are shown under Messages so you can test (including sign-in codes).' }));
      ({ applications: viewApps, shops: viewShops, outbox: viewOutbox })[view](st);
    }, function () { });
  }

  function viewApps() {
    main.appendChild(h('h2', { text: 'Shop registrations' }));
    main.appendChild(h('p', { class: 'sub', text: 'Shops register at ' + location.origin + '/signup. Approving creates the shop and e-mails the owner a link to set their password.' }));
    var list = h('div', { class: 'list' }); main.appendChild(list);
    api('GET', 'applications').then(function (j) {
      if (!j.applications.length) list.appendChild(h('p', { class: 'sub', text: 'No registrations yet.' }));
      j.applications.forEach(function (a) {
        var slug = h('input', { type: 'text', value: a.slug, 'aria-label': 'Shop address' }), out = h('div');
        var acts = a.status !== 'pending' ? h('p', { class: 'sub', text: a.status === 'approved' ? 'Approved as /s/' + a.store_slug + '/' : 'Rejected ' + (a.decision_note || '') }) :
          h('div', { class: 'bar' }, [h('label', { text: 'Address' }), slug,
            h('button', { class: 'btn primary small', type: 'button', text: 'Approve', onclick: function () {
              api('POST', 'applications/' + a.id + '/approve', { slug: slug.value }).then(function (r) {
                out.textContent = '';
                out.appendChild(h('p', { class: 'msg ok', text: 'Shop created. The owner was e-mailed a set-up link. You can also send it yourself (e.g. by WhatsApp):' }));
                out.appendChild(h('div', { class: 'copy', text: r.setup_url }));
                toast('Approved ' + a.shop_name);
              }, function (e) { toast(e.message); });
            } }),
            h('button', { class: 'btn small danger', type: 'button', text: 'Reject', onclick: function () { var n = prompt('Reason (optional, for your records):') || ''; api('POST', 'applications/' + a.id + '/reject', { note: n }).then(start, function (e) { toast(e.message); }); } })]);
        list.appendChild(h('div', { class: 'item' }, [h('div', { class: 't' }, [h('b', { text: a.shop_name + (a.town ? ' · ' + a.town : '') }),
          h('small', { text: a.owner_name + ' · ' + a.owner_email + (a.owner_phone ? ' · ' + a.owner_phone : '') + ' · ' + when(a.created_at) })]),
          h('span', { class: 'chip ' + (a.status === 'pending' ? 'scheduled' : a.status === 'approved' ? 'live' : ''), text: a.status }),
          h('div', { class: 'acts' }, [a.note ? h('p', { class: 'sub', text: a.note }) : null, acts, out])]));
      });
    }, function () { });
  }

  function viewShops() {
    main.appendChild(h('h2', { text: 'Shops and message allowance' }));
    main.appendChild(h('p', { class: 'sub', text: 'Each shop pays for its SMS and WhatsApp messages (sign-in codes sent from its page, and its offers). Set a monthly allowance per shop; 0 switches the channel off for that shop. E-mail is free.' }));
    var list = h('div', { class: 'list' }); main.appendChild(list);
    api('GET', 'shops').then(function (j) {
      j.shops.forEach(function (s) {
        var u = s.usage, sms = h('input', { type: 'number', min: '0', value: u.sms_cap, 'aria-label': 'SMS per month for ' + s.name }), wa = h('input', { type: 'number', min: '0', value: u.wa_cap, 'aria-label': 'WhatsApp per month for ' + s.name });
        list.appendChild(h('div', { class: 'item' }, [h('div', { class: 't' }, [h('b', { text: s.name }), h('small', { text: '/s/' + s.slug + '/ · ' + u.month + ': ' + u.sms_used + ' SMS, ' + u.wa_used + ' WhatsApp, ' + u.email_sent + ' e-mails' })]),
          h('a', { class: 'btn small', href: '/s/' + s.slug + '/admin/', target: '_blank', rel: 'noopener', text: 'Shop admin' }),
          h('div', { class: 'acts' }, [h('label', { text: 'SMS / month' }), sms, h('label', { text: 'WhatsApp / month' }), wa,
            h('button', { class: 'btn small primary', type: 'button', text: 'Save', onclick: function () {
              api('PUT', 'shops/' + s.slug + '/limits', { sms_month_cap: Number(sms.value) || 0, wa_month_cap: Number(wa.value) || 0 }).then(function () { toast('Saved for ' + s.name); }, function (e) { toast(e.message); });
            } })])]));
      });
    }, function () { });
  }

  function viewOutbox(st) {
    main.appendChild(h('h2', { text: 'Messages' }));
    var devBox = h('div'); main.appendChild(devBox);
    if (st.dev_messages && st.dev_messages.length) {
      devBox.appendChild(h('h3', { text: 'Recorded in development mode (not delivered)' }));
      var t = h('table', null, [h('tr', null, [h('th', { text: 'When' }), h('th', { text: 'To' }), h('th', { text: 'By' }), h('th', { text: 'Message' })])]);
      st.dev_messages.forEach(function (m) { t.appendChild(h('tr', null, [h('td', { text: when(m.at) }), h('td', { text: m.to }), h('td', { text: m.channel }), h('td', { class: 'devmsg', text: (m.subject ? m.subject + ': ' : '') + m.text })])); });
      devBox.appendChild(h('div', { class: 'card' }, t));
    }
    main.appendChild(h('div', { class: 'bar' }, h('button', { class: 'btn small', type: 'button', text: 'Refresh', onclick: start })));
    var box = h('div', { class: 'card' }); main.appendChild(h('h3', { text: 'Outbox (delivery status)' })); main.appendChild(box);
    api('GET', 'outbox').then(function (j) {
      var t = h('table', null, [h('tr', null, [h('th', { text: 'When' }), h('th', { text: 'Shop' }), h('th', { text: 'Kind' }), h('th', { text: 'To' }), h('th', { text: 'By' }), h('th', { text: 'Status' })])]);
      j.outbox.forEach(function (o) { t.appendChild(h('tr', null, [h('td', { text: when(o.created_at) }), h('td', { text: o.slug || '-' }), h('td', { text: o.kind }), h('td', { text: o.to }), h('td', { text: o.channel }), h('td', { text: o.status + (o.last_error ? ': ' + o.last_error : '') })])); });
      box.appendChild(t);
    }, function () { });
  }

  start();
})();
