'use strict';
// Builds a fake "camera" video: an MJPEG file showing a (slightly degraded)
// barcode, which Chromium can play as a webcam via --use-file-for-fake-video-capture.
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { render } = require('../js/synth.js');

const DIR = path.join(__dirname, 'fixtures');
fs.mkdirSync(DIR, { recursive: true });

function ppm(img) {
  const { rgba, w, h } = img, out = Buffer.alloc(w * h * 3);
  for (let i = 0, j = 0; i < w * h; i++) { out[j++] = rgba[i * 4]; out[j++] = rgba[i * 4 + 1]; out[j++] = rgba[i * 4 + 2]; }
  return Buffer.concat([Buffer.from(`P6\n${w} ${h}\n255\n`), out]);
}

// opts are synth.render options (module px, blur, noise, angle ...)
function barcodeVideo(name, code, opts = {}) {
  const file = path.join(DIR, name + '.mjpeg');
  if (fs.existsSync(file)) return file;
  const frames = [];
  for (let i = 0; i < 12; i++) {                       // a few frames with different noise, like a live camera
    const p = path.join(DIR, `${name}-${i}`);
    fs.writeFileSync(p + '.ppm', ppm(render(code, { module: 3, blur: 1, noise: 7, angle: 2, gradient: 0.25, ...opts, seed: 100 + i })));
    execFileSync('convert', [p + '.ppm', '-quality', '88', p + '.jpg']);
    frames.push(fs.readFileSync(p + '.jpg'));
    fs.rmSync(p + '.ppm'); fs.rmSync(p + '.jpg');
  }
  const all = [];
  for (let r = 0; r < 10; r++) all.push(...frames);   // ~120 frames
  fs.writeFileSync(file, Buffer.concat(all));
  return file;
}

module.exports = { barcodeVideo };
