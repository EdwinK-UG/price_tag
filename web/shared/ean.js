/* EAN-13 / UPC-A encoder: used to draw printable test barcodes and to generate
   decoder test images. */
(function (root, factory) {
  var m = factory();
  if (typeof module === 'object' && module.exports) module.exports = m; else root.PTEAN = m;
})(this, function () {
  'use strict';
  var L = ['0001101', '0011001', '0010011', '0111101', '0100011', '0110001', '0101111', '0111011', '0110111', '0001011'];
  var G = ['0100111', '0110011', '0011011', '0100001', '0011101', '0111001', '0000101', '0010001', '0001001', '0010111'];
  var R = ['1110010', '1100110', '1101100', '1000010', '1011100', '1001110', '1010000', '1000100', '1001000', '1110100'];
  var PARITY = ['LLLLLL', 'LLGLGG', 'LLGGLG', 'LLGGGL', 'LGLLGG', 'LGGLLG', 'LGGGLL', 'LGLGLG', 'LGLGGL', 'LGGLGL'];

  // 13 digits (string) -> array of 95 modules (1 = bar)
  function ean13(code) {
    if (!/^[0-9]{13}$/.test(code)) throw new Error('need 13 digits');
    var p = PARITY[+code.charAt(0)], bits = '101', i;
    for (i = 1; i <= 6; i++) bits += (p.charAt(i - 1) === 'L' ? L : G)[+code.charAt(i)];
    bits += '01010';
    for (i = 7; i <= 12; i++) bits += R[+code.charAt(i)];
    bits += '101';
    var out = new Array(bits.length);
    for (i = 0; i < bits.length; i++) out[i] = bits.charAt(i) === '1' ? 1 : 0;
    return out;
  }

  // 8 digits -> 67 modules
  function ean8(code) {
    if (!/^[0-9]{8}$/.test(code)) throw new Error('need 8 digits');
    var bits = '101', i;
    for (i = 0; i < 4; i++) bits += L[+code.charAt(i)];
    bits += '01010';
    for (i = 4; i < 8; i++) bits += R[+code.charAt(i)];
    bits += '101';
    var out = [];
    for (i = 0; i < bits.length; i++) out.push(bits.charAt(i) === '1' ? 1 : 0);
    return out;
  }

  // SVG markup string (no scripts; safe to insert via a parsed fragment)
  function svg(code, opts) {
    opts = opts || {};
    var mods = code.length === 8 ? ean8(code) : ean13(code), m = opts.module || 2, h = opts.height || 70, q = 10 * m;
    var w = mods.length * m + q * 2, rects = '', i = 0;
    while (i < mods.length) {
      if (mods[i]) { var j = i; while (j < mods.length && mods[j]) j++; rects += 'M' + (q + i * m) + ' 0h' + (j - i) * m + 'v' + h + 'h-' + (j - i) * m + 'z'; i = j; } else i++;
    }
    return { width: w, height: h, path: rects };
  }

  return { ean13: ean13, ean8: ean8, svg: svg };
});
