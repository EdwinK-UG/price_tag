/* A small 1-D barcode decoder for EAN-13, UPC-A and EAN-8.
 *
 * It exists so that phones WITHOUT the browser's built-in BarcodeDetector
 * (older Android, iOS Safari, Huawei without Google services) can still scan,
 * without downloading a 400 KB WebAssembly decoder. It works on luminance lines:
 *   1. locate bar edges with sub-pixel precision (two methods: crossings of the
 *      local mid-level for sharp images, slope peaks for blurry ones),
 *   2. find the guard bars, match each digit's 4 runs against the L/G/R tables
 *      with ZXing-style variance matching, and verify the GS1 check digit,
 *   3. sanity-check the layout (quiet zones, guard widths vs digit widths),
 *   4. only accept a code that at least two different lines agree on.
 * Pure noise and random stripes therefore never produce a result, and a wrong
 * price is far worse than a slow scan.
 */
(function (root, factory) {
  var m = factory(root.PTCode || (typeof require === 'function' ? require('./code.js') : null));
  if (typeof module === 'object' && module.exports) module.exports = m; else root.PTDecode = m;
})(this, function (Code) {
  'use strict';

  var LP = [[3, 2, 1, 1], [2, 2, 2, 1], [2, 1, 2, 2], [1, 4, 1, 1], [1, 1, 3, 2], [1, 2, 3, 1], [1, 1, 1, 4], [1, 3, 1, 2], [1, 2, 1, 3], [3, 1, 1, 2]];
  var PATS = LP.concat(LP.map(function (p) { return [p[3], p[2], p[1], p[0]]; })); // 0-9 = L, 10-19 = G
  var FIRST = [0x00, 0x0B, 0x0D, 0x0E, 0x13, 0x19, 0x1C, 0x15, 0x16, 0x1A];        // parity pattern -> leading digit
  var MAX_AVG = 0.48, MAX_ONE = 0.7;
  var ONES3 = [1, 1, 1], ONES5 = [1, 1, 1, 1, 1];

  function variance(c, o, n, pat) {
    var total = 0, plen = 0, i;
    for (i = 0; i < n; i++) { total += c[o + i]; plen += pat[i]; }
    if (total < plen) return Infinity;
    var unit = total / plen, maxOne = MAX_ONE * unit, tv = 0;
    for (i = 0; i < n; i++) {
      var d = Math.abs(c[o + i] - pat[i] * unit);
      if (d > maxOne) return Infinity;
      tv += d;
    }
    return tv / total;
  }

  function digitAt(c, o, allowG) {
    var best = -1, bv = MAX_AVG, n = allowG ? 20 : 10;
    for (var d = 0; d < n; d++) {
      var v = variance(c, o, 4, PATS[d]);
      if (v < bv) { bv = v; best = d; }
    }
    return best;
  }

  // Layout sanity: every guard bar about as wide as one module of the whole
  // symbol, and white space around it. Random patterns rarely satisfy this.
  function layoutOK(c, s, count, modules) {
    var total = 0, i;
    for (i = s; i < s + count; i++) total += c[i];
    var unit = total / modules;
    var guards = [s, s + 1, s + 2, s + count - 3, s + count - 2, s + count - 1];
    for (i = 0; i < guards.length; i++) { var w = c[guards[i]] / unit; if (w < 0.5 || w > 1.7) return false; }
    var m0 = s + (count === 59 ? 27 : 19);             // centre guard follows 3 + 6*4 (or 4*4) runs
    for (i = 0; i < 5; i++) { var wm = c[m0 + i] / unit; if (wm < 0.5 || wm > 1.7) return false; }
    if (s > 1 && c[s - 1] < unit * 2) return false;                       // quiet zone before
    var after = c[s + count];
    if (after !== undefined && after < unit * 1.5 && s + count < c.length - 1) return false; // and after
    return true;
  }

  function tryEAN13(c, s) {
    if (s + 59 > c.length) return null;
    var o = s + 3, digits = [], parity = 0, i;
    for (i = 0; i < 6; i++, o += 4) {
      var d = digitAt(c, o, true);
      if (d < 0) return null;
      if (d >= 10) { parity |= 1 << (5 - i); d -= 10; }
      digits.push(d);
    }
    if (variance(c, o, 5, ONES5) >= MAX_AVG) return null;
    o += 5;
    for (i = 0; i < 6; i++, o += 4) {
      var e = digitAt(c, o, false);
      if (e < 0) return null;
      digits.push(e);
    }
    if (variance(c, o, 3, ONES3) >= MAX_AVG) return null;
    var first = -1;
    for (i = 0; i < 10; i++) if (FIRST[i] === parity) { first = i; break; }
    if (first < 0) return null;
    var code = first + digits.join('');
    if (!Code.checkDigitValid(code) || !layoutOK(c, s, 59, 95)) return null;
    return { code: code, format: 'ean_13' };
  }

  function tryEAN8(c, s) {
    if (s + 43 > c.length) return null;
    var o = s + 3, digits = [], i;
    for (i = 0; i < 4; i++, o += 4) {
      var d = digitAt(c, o, false);
      if (d < 0) return null;
      digits.push(d);
    }
    if (variance(c, o, 5, ONES5) >= MAX_AVG) return null;
    o += 5;
    for (i = 0; i < 4; i++, o += 4) {
      var e = digitAt(c, o, false);
      if (e < 0) return null;
      digits.push(e);
    }
    if (variance(c, o, 3, ONES3) >= MAX_AVG) return null;
    var code = digits.join('');
    if (!Code.checkDigitValid(code) || !layoutOK(c, s, 43, 67)) return null;
    return { code: code, format: 'ean_8' };
  }

  // runs: widths, runs[0] is white (quiet zone), black runs at odd indices.
  function decodeRuns(runs) {
    var n = runs.length;
    for (var s = 1; s + 43 <= n; s += 2) {
      if (variance(runs, s, 3, ONES3) >= MAX_AVG) continue;
      var r = tryEAN13(runs, s) || tryEAN8(runs, s);
      if (r) return r;
    }
    return null;
  }

  // ---- edge location --------------------------------------------------------------

  var BLOCK = 8;
  function smooth(lum) {
    var n = lum.length, sm = new Float32Array(n), i;
    sm[0] = lum[0]; sm[n - 1] = lum[n - 1];
    for (i = 1; i < n - 1; i++) sm[i] = (lum[i - 1] + 2 * lum[i] + lum[i + 1]) / 4;
    return sm;
  }

  // per-block local mid-level and contrast range (window of +-5 blocks)
  function localLevels(sm) {
    var n = sm.length, nb = Math.ceil(n / BLOCK), bmin = new Float32Array(nb), bmax = new Float32Array(nb), b, i;
    for (b = 0; b < nb; b++) {
      var lo = 255, hi = 0, end = Math.min(n, (b + 1) * BLOCK);
      for (i = b * BLOCK; i < end; i++) { if (sm[i] < lo) lo = sm[i]; if (sm[i] > hi) hi = sm[i]; }
      bmin[b] = lo; bmax[b] = hi;
    }
    var bmid = new Float32Array(nb), brange = new Float32Array(nb);
    for (b = 0; b < nb; b++) {
      var l2 = 255, h2 = 0;
      for (var k = Math.max(0, b - 5); k <= Math.min(nb - 1, b + 5); k++) { if (bmin[k] < l2) l2 = bmin[k]; if (bmax[k] > h2) h2 = bmax[k]; }
      bmid[b] = (l2 + h2) / 2; brange[b] = h2 - l2;
    }
    return { mid: bmid, range: brange, nb: nb };
  }

  function at(lv, arr, i) { // linear interpolation between block centres
    var f = (i - BLOCK / 2) / BLOCK, b0 = Math.max(0, Math.min(lv.nb - 1, Math.floor(f))), b1 = Math.min(lv.nb - 1, b0 + 1), t = Math.max(0, Math.min(1, f - b0));
    return arr[b0] + (arr[b1] - arr[b0]) * t;
  }

  function toRuns(edges, firstIsFall, n) {
    // edges alternate; a falling edge (light -> dark) starts a bar.
    if (edges.length < 8) return null;
    var runs = [], i;
    if (firstIsFall) runs.push(edges[0]); else { runs.push(0); runs.push(edges[0]); }
    for (i = 1; i < edges.length; i++) runs.push(Math.max(0.01, edges[i] - edges[i - 1]));
    runs.push(Math.max(0.01, n - edges[edges.length - 1]));
    var endsBlack = (edges.length % 2 === 1) === firstIsFall;
    return { runs: runs, endsBlack: endsBlack };
  }

  // Method A: where the signal crosses the local mid-level (hysteresis against noise).
  function runsByLevel(sm) {
    var n = sm.length, lv = localLevels(sm), edges = [], dark = false, lastCross = 0, prevD = 0, first = null, i;
    for (i = 0; i < n; i++) {
      var mid = at(lv, lv.mid, i), rng = at(lv, lv.range, i), d = sm[i] - mid, band = Math.max(0.12 * rng, 6);
      if (i > 0 && ((prevD < 0) !== (d < 0)) && prevD !== d) lastCross = (i - 1) + prevD / (prevD - d);
      prevD = d;
      if (rng < 36) { if (dark) { edges.push(lastCross); dark = false; } continue; }
      if (!dark && d < -band) { edges.push(i > 0 ? lastCross : 0); if (first === null) first = true; dark = true; }
      else if (dark && d > band) { edges.push(lastCross); if (first === null) first = false; dark = false; }
    }
    return toRuns(edges, first !== false, n);
  }

  // Method B: positions of the steepest slope between light and dark. Survives
  // blur better, because narrow bars lose height but keep their inflection points.
  function runsBySlope(sm) {
    var n = sm.length, lv = localLevels(sm), g = new Float32Array(n), i;
    for (i = 1; i < n - 1; i++) g[i] = sm[i + 1] - sm[i - 1];
    var peaks = [];
    for (i = 2; i < n - 2; i++) {
      var a = Math.abs(g[i]);
      if (a < Math.max(8, 0.1 * at(lv, lv.range, i)) || a < Math.abs(g[i - 1]) || a <= Math.abs(g[i + 1])) continue;
      var l = Math.abs(g[i - 1]), r = Math.abs(g[i + 1]), den = l - 2 * a + r;
      peaks.push({ pos: i + (den < 0 ? 0.5 * (l - r) / den : 0), fall: g[i] < 0, mag: a });
    }
    var kept = [];
    for (i = 0; i < peaks.length; i++) {          // edges must alternate fall/rise
      var p = peaks[i], last = kept[kept.length - 1];
      if (last && last.fall === p.fall) { if (p.mag > last.mag) kept[kept.length - 1] = p; } else kept.push(p);
    }
    if (kept.length < 8) return null;
    return toRuns(kept.map(function (p) { return p.pos; }), kept[0].fall, n);
  }

  // lum: Uint8 array of one line. Returns { code, format } or null.
  function decodeLine(lum) {
    if (lum.length < 40) return null;
    var sm = smooth(lum), methods = [runsByLevel, runsBySlope];
    for (var m = 0; m < methods.length; m++) {
      var lr = methods[m](sm);
      if (!lr) continue;
      var r = decodeRuns(lr.runs);
      if (r) return r;
      // Upside-down barcode: read the same line backwards (must again start white).
      var rev = lr.runs.slice().reverse();
      if (lr.endsBlack) rev.unshift(0);
      r = decodeRuns(rev);
      if (r) return r;
    }
    return null;
  }

  function rowLum(rgba, w, y, out) {
    var o = y * w * 4 + 1; // green channel: best contrast/cost on phone cameras
    for (var x = 0; x < w; x++, o += 4) out[x] = rgba[o];
    return out;
  }
  function colLum(rgba, w, h, x, out) {
    var o = x * 4 + 1;
    for (var y = 0; y < h; y++, o += w * 4) out[y] = rgba[o];
    return out;
  }

  // Decode a frame. Rows through the middle first, then columns (barcode held
  // sideways). A code is returned only when `agree` separate lines read it.
  function decodeImage(rgba, w, h, opts) {
    opts = opts || {};
    var rows = opts.rows || 16, agree = opts.agree || 2, seen = {}, k;
    var buf = new Uint8Array(Math.max(w, h));
    var order = [0];
    for (k = 1; k < rows; k++) order.push(k % 2 ? (k + 1) >> 1 : -(k >> 1));
    function pass(count, size, extent, getLine) {
      var span = Math.min(size * 0.6, size - 2), step = span / rows;
      for (var i = 0; i < order.length; i++) {
        var p = Math.round(size / 2 + order[i] * step);
        if (p < 0 || p >= size) continue;
        var r = decodeLine(getLine(p));
        if (r) {
          var e = seen[r.code] || (seen[r.code] = { n: 0, r: r });
          if (++e.n >= agree) return e.r;
        }
      }
      return null;
    }
    var hit = pass(rows, h, w, function (y) { return rowLum(rgba, w, y, buf.subarray(0, w)); });
    if (hit || opts.vertical === false) return hit;
    return pass(rows, w, h, function (x) { return colLum(rgba, w, h, x, buf.subarray(0, h)); });
  }

  return { decodeLine: decodeLine, decodeImage: decodeImage, decodeRuns: decodeRuns };
});
