/* Barcode normalisation. Mirrors internal/barcode (Go); both are tested against
   the same table so a scan and the server always agree on what a code is. */
(function (root, factory) {
  var m = factory();
  if (typeof module === 'object' && module.exports) module.exports = m; else root.PTCode = m;
})(this, function () {
  'use strict';

  function allDigits(s) { return /^[0-9]+$/.test(s); }

  // GS1 check digit for GTIN-8/12/13/14.
  function checkDigitValid(d) {
    var n = d.length, sum = 0;
    if (n < 2 || !allDigits(d)) return false;
    for (var i = 0; i < n - 1; i++) {
      var v = d.charCodeAt(n - 2 - i) - 48;
      sum += (i % 2 === 0) ? 3 * v : v;
    }
    return (10 - sum % 10) % 10 === d.charCodeAt(n - 1) - 48;
  }

  // -> { ok:true, code } | { ok:false, error }   error: empty|long|chars|check
  function canonical(raw) {
    var s = String(raw == null ? '' : raw).replace(/\s+/g, '');
    if (!s) return { ok: false, error: 'empty' };
    if (s.length > 32) return { ok: false, error: 'long' };
    if (allDigits(s)) {
      var n = s.length;
      if (n === 8 || n === 12 || n === 13 || n === 14) {
        if (!checkDigitValid(s)) return { ok: false, error: 'check' };
        return { ok: true, code: new Array(15 - n).join('0') + s };
      }
      return { ok: true, code: s };
    }
    s = s.toUpperCase();
    if (!/^[A-Z0-9._-]+$/.test(s)) return { ok: false, error: 'chars' };
    return { ok: true, code: s };
  }

  // Canonical 14-digit GTIN back to the number printed on the pack.
  function display(c) {
    if (c.length === 14 && allDigits(c)) {
      if (c.slice(0, 6) === '000000') return c.slice(6);
      if (c.charAt(0) === '0') return c.slice(1);
    }
    return c;
  }

  // 8-digit UPC-E -> 12-digit UPC-A (the form the price list uses).
  function upceToUpca(e) {
    if (!/^[01][0-9]{7}$/.test(e)) return null;
    var n = e.charAt(0), x = e.slice(1, 7), c = e.charAt(7), last = x.charAt(5), body;
    if ('012'.indexOf(last) >= 0) body = n + x.slice(0, 2) + last + '0000' + x.slice(2, 5);
    else if (last === '3') body = n + x.slice(0, 3) + '00000' + x.slice(3, 5);
    else if (last === '4') body = n + x.slice(0, 4) + '00000' + x.charAt(4);
    else body = n + x.slice(0, 5) + '0000' + last;
    return body + c;
  }

  return { checkDigitValid: checkDigitValid, canonical: canonical, display: display, upceToUpca: upceToUpca };
});
