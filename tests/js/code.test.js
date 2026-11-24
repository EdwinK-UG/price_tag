'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const C = require('../../web/shopper/code.js');

// The same table as internal/barcode/barcode_test.go: client and server must agree.
test('check digits', () => {
  for (const g of ['4006381333931', '036000291452', '96385074', '00012345678905', '5901234123457']) assert.ok(C.checkDigitValid(g), g);
  for (const b of ['4006381333932', '036000291453', '96385075', '5901234123458', '1', '', '12a4']) assert.ok(!C.checkDigitValid(b), b);
});

test('canonical matches the Go implementation', () => {
  const ok = { '4006381333931': '04006381333931', ' 4006 3813 33931 ': '04006381333931', '036000291452': '00036000291452',
    '0036000291452': '00036000291452', '96385074': '00000096385074', '4011': '4011', 'sugar-1kg': 'SUGAR-1KG' };
  for (const [i, o] of Object.entries(ok)) assert.deepEqual(C.canonical(i), { ok: true, code: o }, i);
  assert.equal(C.canonical('4006381333932').error, 'check');
  assert.equal(C.canonical('').error, 'empty');
  assert.equal(C.canonical('   ').error, 'empty');
  assert.equal(C.canonical('a b/c').error, 'chars');
  assert.equal(C.canonical('1'.repeat(33)).error, 'long');
});

test('display', () => {
  const t = { '04006381333931': '4006381333931', '00036000291452': '0036000291452', '00000096385074': '96385074', '14006381333938': '14006381333938', 'SUGAR-1KG': 'SUGAR-1KG', '4011': '4011' };
  for (const [i, o] of Object.entries(t)) assert.equal(C.display(i), o, i);
});

test('UPC-E expansion', () => {
  // classic examples: 01234565 -> 012345000065 ; 04252614 -> 042100005264
  assert.equal(C.upceToUpca('01234565'), '012345000065');
  assert.equal(C.upceToUpca('04252614'), '042100005264');
  assert.ok(C.checkDigitValid(C.upceToUpca('01234565')));
  assert.ok(C.checkDigitValid(C.upceToUpca('04252614')));
  assert.equal(C.upceToUpca('9123456'), null);
});
