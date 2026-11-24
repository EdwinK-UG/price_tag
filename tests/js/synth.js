'use strict';
// Synthetic camera frames for decoder tests: a barcode drawn on a luminance
// canvas, then degraded like a real phone camera (blur, noise, tilt, uneven light).
const EAN = require('../../web/shared/ean.js');

function rng(seed) { // mulberry32
  let a = seed >>> 0;
  return () => { a = (a + 0x6D2B79F5) >>> 0; let t = a; t = Math.imul(t ^ (t >>> 15), t | 1); t ^= t + Math.imul(t ^ (t >>> 7), t | 61); return ((t ^ (t >>> 14)) >>> 0) / 4294967296; };
}
function gauss(r) { return (r() + r() + r() + r() - 2) * 1.7320508; } // ~N(0,1)

function boxBlur(src, w, h, rad) {
  if (rad <= 0) return src;
  const tmp = new Float32Array(src.length), out = new Float32Array(src.length), n = 2 * rad + 1;
  for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) { let s = 0; for (let k = -rad; k <= rad; k++) s += src[y * w + Math.min(w - 1, Math.max(0, x + k))]; tmp[y * w + x] = s / n; }
  for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) { let s = 0; for (let k = -rad; k <= rad; k++) s += tmp[Math.min(h - 1, Math.max(0, y + k)) * w + x]; out[y * w + x] = s / n; }
  return out;
}

// opts: module px, w,h, angle (deg), blur (box radius, applied twice), noise (sigma), gradient (0..1), dx,dy offset, barH, invert
function render(code, o = {}) {
  const w = o.w || 640, h = o.h || 480, m = o.module || 3, seed = o.seed || 1;
  const r = rng(seed);
  const mods = code.length === 8 ? EAN.ean8(code) : EAN.ean13(code);
  const bw = mods.length * m, barH = o.barH || Math.round(h * 0.38);
  const cx = w / 2 + (o.dx || 0), cy = h / 2 + (o.dy || 0), ang = (o.angle || 0) * Math.PI / 180;
  const cos = Math.cos(ang), sin = Math.sin(ang);
  let img = new Float32Array(w * h);
  for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) {
    // inverse-rotate into barcode space
    const rx = (x - cx) * cos + (y - cy) * sin, ry = -(x - cx) * sin + (y - cy) * cos;
    let v = 232;
    if (Math.abs(ry) < barH / 2 && rx >= -bw / 2 && rx < bw / 2) {
      const idx = Math.floor((rx + bw / 2) / m);
      if (mods[idx]) v = 28;
    }
    img[y * w + x] = v;
  }
  for (let i = 0; i < (o.blur ? 2 : 0); i++) img = boxBlur(img, w, h, o.blur);
  const rgba = new Uint8ClampedArray(w * h * 4);
  for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) {
    let v = img[y * w + x];
    if (o.gradient) v *= 1 - o.gradient * (x / w);
    if (o.noise) v += gauss(r) * o.noise;
    const i = (y * w + x) * 4;
    rgba[i] = rgba[i + 1] = rgba[i + 2] = v; rgba[i + 3] = 255;
  }
  return { rgba, w, h };
}

function randomEAN13(r) {
  let body = '';
  for (let i = 0; i < 12; i++) body += Math.floor(r() * 10);
  let s = 0; for (let i = 0; i < 12; i++) s += (i % 2 ? 3 : 1) * +body[i];
  return body + ((10 - s % 10) % 10);
}

module.exports = { render, rng, randomEAN13, gauss };
