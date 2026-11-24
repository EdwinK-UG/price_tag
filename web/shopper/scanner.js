/* Camera scanning. Loaded only when the shopper taps "Scan".
 *
 * Uses the browser's own BarcodeDetector where it exists (Chrome on most
 * Android phones: fast, free, no download). Otherwise it falls back to the
 * small decoder in decode.js. The scan loop adapts its pace to the phone: a
 * slow handset simply scans fewer frames per second instead of freezing the UI.
 */
(function () {
  'use strict';
  var C = window.PTCode, D = window.PTDecode;
  var NATIVE = ['ean_13', 'ean_8', 'upc_a', 'upc_e'];

  function camError(e) {
    var n = e && e.name, m;
    if (n === 'NotAllowedError' || n === 'PermissionDeniedError') m = 'Camera access is blocked. Allow the camera for this site in your browser settings, or type the number.';
    else if (n === 'NotFoundError' || n === 'DevicesNotFoundError') m = 'No camera was found. You can type the number instead.';
    else if (n === 'NotReadableError' || n === 'TrackStartError') m = 'The camera is being used by another app. Close it and try again.';
    else if (n === 'SecurityError') m = 'The camera needs a secure (https) connection. You can type the number instead.';
    else m = 'The camera could not start. You can type the number instead.';
    var err = new Error(m); err.name = n; return err;
  }

  function getStream() {
    var md = navigator.mediaDevices;
    if (!md || !md.getUserMedia) {
      return Promise.reject(new Error(window.isSecureContext === false ? 'The camera needs a secure (https) connection. You can type the number instead.' : 'This browser cannot use the camera. You can type the number instead.'));
    }
    var wanted = { video: { facingMode: { ideal: 'environment' }, width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 24, max: 30 } }, audio: false };
    return md.getUserMedia(wanted).catch(function (e) {
      if (e && /NotAllowed|PermissionDenied|Security|NotFound|NotReadable/.test(e.name)) throw e;
      return md.getUserMedia({ video: true, audio: false }); // over-constrained on an odd camera
    }).catch(function (e) { throw camError(e); });
  }

  function start(video, o) {
    return getStream().then(function (stream) {
      var track = stream.getVideoTracks()[0], caps = {}, running = true, timer, detector = null, t0 = Date.now(), hinted = 0;
      var canvas = document.createElement('canvas'), ctx = null;
      try { caps = track.getCapabilities ? track.getCapabilities() : {}; } catch (e) { }
      try { if (caps.focusMode && caps.focusMode.indexOf('continuous') >= 0) track.applyConstraints({ advanced: [{ focusMode: 'continuous' }] }); } catch (e) { }
      video.srcObject = stream; video.muted = true; video.setAttribute('playsinline', '');
      var played = video.play();
      if (played && played.catch) played.catch(function () { });
      if (o.onCaps) o.onCaps({ torch: !!caps.torch, zoom: !!caps.zoom });

      function stop() {
        running = false; clearTimeout(timer);
        try { stream.getTracks().forEach(function (t) { t.stop(); }); } catch (e) { }
        video.srcObject = null;
      }

      function accept(raw, fmt) {
        if (fmt === 'upc_e' && raw.length === 8) raw = C.upceToUpca(raw) || raw;
        var c = C.canonical(raw);
        if (!c.ok) return false;                    // misread (bad check digit): keep scanning
        stop(); o.onCode(raw, fmt); return true;
      }

      function soft() {
        var vw = video.videoWidth, vh = video.videoHeight;
        if (!vw || !vh) return null;
        var s = Math.min(1, 720 / Math.max(vw, vh)), w = Math.round(vw * s), hh = Math.round(vh * s);
        if (canvas.width !== w || canvas.height !== hh) { canvas.width = w; canvas.height = hh; ctx = null; }
        ctx = ctx || canvas.getContext('2d', { willReadFrequently: true });
        ctx.drawImage(video, 0, 0, w, hh);
        return D.decodeImage(ctx.getImageData(0, 0, w, hh).data, w, hh);
      }

      function hint() {
        var t = Date.now() - t0;
        if (t > 15000 && hinted < 2) { hinted = 2; o.onStatus('Not working? Close this and type the number printed under the barcode.'); }
        else if (t > 6000 && hinted < 1) { hinted = 1; o.onStatus('Hold the phone 10-20 cm away, keep the barcode inside the box and well lit.'); }
      }

      function loop() {
        if (!running) return;
        var t = performance.now();
        if (video.readyState < 2) { timer = setTimeout(loop, 120); return; }
        var p;
        try { p = detector ? detector.detect(video).then(function (r) { return r && r.length ? { code: r[0].rawValue, format: r[0].format } : null; }) : Promise.resolve(soft()); }
        catch (e) { p = Promise.resolve(null); }
        p.then(function (hit) {
          if (!running) return;
          if (hit && accept(hit.code, hit.format)) return;
          next(t);
        }, function () { detector = null; next(t); }); // native detector failed once: use the fallback from now on
      }
      function next(t) {
        hint();
        var cost = performance.now() - t;
        timer = setTimeout(loop, Math.max(70, Math.min(350, cost * 1.6))); // a slow phone scans less often, never janks
      }

      var ctl = {
        stop: stop,
        torch: function () { ctl.on = !ctl.on; try { track.applyConstraints({ advanced: [{ torch: ctl.on }] }); } catch (e) { } },
        zoom: function () {
          var z = caps.zoom; if (!z) return;
          var cur = (track.getSettings && track.getSettings().zoom) || z.min, nx = cur * 1.6;
          if (nx > z.max) nx = z.min;
          try { track.applyConstraints({ advanced: [{ zoom: nx }] }); } catch (e) { }
        }
      };

      o.onStatus('Point the camera at the barcode');
      if (window.BarcodeDetector) {
        var gs = BarcodeDetector.getSupportedFormats ? BarcodeDetector.getSupportedFormats() : Promise.resolve(NATIVE);
        gs.then(function (f) {
          var use = NATIVE.filter(function (x) { return f.indexOf(x) >= 0; });
          if (use.length) detector = new BarcodeDetector({ formats: use });
        }).catch(function () { }).then(loop);
      } else loop();
      return ctl;
    });
  }

  window.PTScan = { start: start };
})();
