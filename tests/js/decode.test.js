'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const D = require('../../web/shopper/decode.js');
const { render, rng, randomEAN13 } = require('./synth.js');

function rate(n, mk, check) {
  const r = rng(12345);
  let ok = 0;
  for (let i = 0; i < n; i++) {
    const code = randomEAN13(r);
    const img = render(code, mk(i, r));
    const res = D.decodeImage(img.rgba, img.w, img.h);
    if (res && res.code === (check ? check(code) : code)) ok++;
  }
  return ok / n;
}

test('clean barcodes at several module sizes decode 100%', () => {
  for (const m of [2, 3, 4, 5]) assert.equal(rate(40, (i) => ({ module: m, seed: i + 1 })), 1, 'module ' + m);
});

test('UPC-A (leading zero EAN-13) decodes', () => {
  const res = (c) => { const img = render(c, {}); return D.decodeImage(img.rgba, img.w, img.h); };
  assert.equal(res('0036000291452').code, '0036000291452');
});

test('EAN-8 decodes', () => {
  const img = render('96385074', { module: 4 });
  assert.equal(D.decodeImage(img.rgba, img.w, img.h).code, '96385074');
});

test('upside-down barcode decodes', () => {
  assert.ok(rate(40, (i) => ({ angle: 180, seed: i + 1, module: 3 })) >= 0.97);
});

test('barcode held sideways decodes via columns', () => {
  assert.ok(rate(40, (i) => ({ angle: 90, seed: i + 1, module: 3 })) >= 0.95);
  assert.ok(rate(40, (i) => ({ angle: -90, seed: i + 1, module: 3 })) >= 0.95);
});

test('tilted barcodes (hand shake) decode', () => {
  for (const a of [4, 8, -8, 12]) { const r = rate(40, (i) => ({ angle: a, seed: i + 1, module: 3 })); assert.ok(r >= 0.9, `angle ${a}: ${r}`); }
});

test('blurry (out of focus) images decode', () => {
  const r1 = rate(60, (i) => ({ blur: 1, module: 3, seed: i + 1 }));
  const r2 = rate(60, (i) => ({ blur: 2, module: 4, seed: i + 1 }));
  assert.ok(r1 >= 0.97, 'blur1 ' + r1);   // sigma ~1 px on 3 px modules
  assert.ok(r2 >= 0.95, 'blur2 ' + r2);   // sigma ~2 px on 4 px modules (very soft focus)
});

test('noisy low-light sensor images decode', () => {
  const r = rate(40, (i) => ({ noise: 14, module: 3, seed: i + 1 }));
  assert.ok(r >= 0.9, 'noise ' + r);
});

test('uneven lighting gradient decodes', () => {
  assert.ok(rate(40, (i) => ({ gradient: 0.5, module: 3, seed: i + 1 })) >= 0.95);
});

test('realistic mix: slight blur + noise + tilt + gradient', () => {
  const r = rate(60, (i, g) => ({ blur: 1, noise: 8, angle: (g() - 0.5) * 10, gradient: 0.3, module: 3, seed: i + 1, dx: (g() - 0.5) * 80, dy: (g() - 0.5) * 60 }));
  assert.ok(r >= 0.9, 'mix ' + r);
});

test('NEVER returns a wrong code: false positives on noise, text-like stripes and cropped codes', () => {
  const g = rng(99);
  let wrong = 0;
  for (let i = 0; i < 400; i++) { // pure noise
    const w = 320, h = 240, a = new Uint8ClampedArray(w * h * 4);
    for (let j = 0; j < a.length; j += 4) { const v = g() * 255; a[j] = a[j + 1] = a[j + 2] = v; a[j + 3] = 255; }
    if (D.decodeImage(a, w, h)) wrong++;
  }
  for (let i = 0; i < 200; i++) { // random bar patterns that merely look barcode-ish
    const w = 400, h = 100, a = new Uint8ClampedArray(w * h * 4);
    let x = 0, col = 255;
    while (x < w) { const run = 2 + Math.floor(g() * 7); for (let k = 0; k < run && x < w; k++, x++) for (let y = 0; y < h; y++) { const p = (y * w + x) * 4; a[p] = a[p + 1] = a[p + 2] = col; a[p + 3] = 255; } col = 255 - col; }
    if (D.decodeImage(a, w, h)) wrong++;
  }
  assert.equal(wrong, 0);
});

test('damaged or half-visible barcodes give the right code or nothing, never a different one', () => {
  const g = rng(7);
  let wrong = 0, right = 0;
  for (let i = 0; i < 300; i++) {
    const code = randomEAN13(g);
    const img = render(code, { module: 3, seed: i + 1, noise: 6 });
    // scratch: overwrite a vertical stripe 2-9 px wide somewhere across the code with random grey
    const x0 = 160 + Math.floor(g() * 300), wd = 2 + Math.floor(g() * 8), grey = Math.floor(g() * 255);
    for (let y = 0; y < img.h; y++) for (let x = x0; x < x0 + wd; x++) { const p = (y * img.w + x) * 4; img.rgba[p] = img.rgba[p + 1] = img.rgba[p + 2] = grey; }
    const res = D.decodeImage(img.rgba, img.w, img.h);
    if (res) { if (res.code === code) right++; else wrong++; }
  }
  assert.equal(wrong, 0, 'wrong codes returned: ' + wrong);
  console.log(`      damaged codes: ${right}/300 still read correctly, 0 misread`);
});

test('a barcode cut off at the frame edge returns null, not a guess', () => {
  const g = rng(5);
  for (let i = 0; i < 60; i++) {
    const img = render(randomEAN13(g), { module: 3, seed: i + 1, dx: 210 + g() * 60 }); // right part falls outside the frame
    assert.equal(D.decodeImage(img.rgba, img.w, img.h), null);
  }
});

test('a flat grey frame and tiny frames do not throw', () => {
  const a = new Uint8ClampedArray(10 * 10 * 4).fill(128);
  assert.equal(D.decodeImage(a, 10, 10), null);
  const b = new Uint8ClampedArray(640 * 480 * 4).fill(255);
  assert.equal(D.decodeImage(b, 640, 480), null);
});

test('speed: a 640x480 frame decodes in well under 15 ms (slow-phone budget leaves headroom)', () => {
  const img = render('5901234123457', { module: 3, blur: 1, noise: 6 });
  const miss = render('5901234123457', { module: 3, dx: 400 }); // worst case: scans every row + column and finds nothing
  const t0 = process.hrtime.bigint();
  for (let i = 0; i < 20; i++) D.decodeImage(img.rgba, img.w, img.h);
  const hit = Number(process.hrtime.bigint() - t0) / 20e6;
  const t1 = process.hrtime.bigint();
  for (let i = 0; i < 20; i++) D.decodeImage(miss.rgba, miss.w, miss.h);
  const worst = Number(process.hrtime.bigint() - t1) / 20e6;
  console.log(`      decode time: hit ${hit.toFixed(2)} ms, worst-case miss ${worst.toFixed(2)} ms (desktop CPU)`);
  assert.ok(hit < 15 && worst < 40);
});
