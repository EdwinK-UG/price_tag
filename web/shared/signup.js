(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var touched = false;
  function slugify(s) { return s.toLowerCase().normalize('NFKD').replace(/[̀-ͯ]/g, '').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40); }
  function paintURL() { var v = $('slug').value; $('url').textContent = v ? 'Your shop page will be ' + location.origin + '/s/' + v + '/' : ''; }
  $('shop').addEventListener('input', function () { if (!touched) { $('slug').value = slugify($('shop').value); paintURL(); } });
  $('slug').addEventListener('input', function () { touched = true; $('slug').value = slugify($('slug').value); paintURL(); });
  $('f').addEventListener('submit', function (e) {
    e.preventDefault();
    var err = $('err'); err.hidden = true; $('go').disabled = true;
    var body = { shop_name: $('shop').value, slug: $('slug').value, owner_name: $('name').value, owner_email: $('email').value, owner_phone: $('phone').value, town: $('town').value, note: $('note').value, website: $('website').value };
    fetch('/api/v1/signup', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-PT-CSRF': '1' }, body: JSON.stringify(body) })
      .then(function (r) { return r.json().catch(function () { return {}; }).then(function (j) { if (!r.ok) throw new Error(j.message || 'Could not send. Try again.'); }); })
      .then(function () { $('f').hidden = true; $('done').hidden = false; window.scrollTo(0, 0); },
        function (e2) { err.textContent = e2.message; err.hidden = false; $('go').disabled = false; });
  });
})();
