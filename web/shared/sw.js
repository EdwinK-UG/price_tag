/* Service worker: makes the shopper app open instantly and work offline.
 * It never touches /api/ (prices are always fetched fresh) or the admin app. */
'use strict';
var V = 'pt-shell-v1';
var PRE = ['/static/shopper/app.css', '/static/shopper/app.js', '/static/shopper/code.js', '/static/shopper/scanner.js', '/static/shopper/decode.js', '/static/shared/icon.svg'];
var SHELL = '/s/_shell';   // every store serves the same HTML shell, so one cached copy serves them all

self.addEventListener('install', function (e) {
  e.waitUntil(caches.open(V).then(function (c) { return c.addAll(PRE); }).then(function () { return self.skipWaiting(); }));
});
self.addEventListener('activate', function (e) {
  e.waitUntil(caches.keys().then(function (ks) { return Promise.all(ks.filter(function (k) { return k !== V; }).map(function (k) { return caches.delete(k); })); }).then(function () { return self.clients.claim(); }));
});
// The first visit happens before the worker controls the page, so the shell was
// never fetched through it. The page tells the worker which URL is the shell.
self.addEventListener('message', function (e) {
  var d = e.data;
  if (!d || d.type !== 'shell' || !/^\/s\/[^/]+\/$/.test(d.url)) return;
  e.waitUntil(caches.open(V).then(function (c) {
    return fetch(d.url, { credentials: 'omit' }).then(function (res) { if (res.ok) return c.put(SHELL, res); });
  }).catch(function () { }));
});
self.addEventListener('fetch', function (e) {
  var req = e.request;
  if (req.method !== 'GET') return;
  var u = new URL(req.url);
  if (u.origin !== location.origin) return;
  var p = u.pathname;
  if (p.indexOf('/api/') === 0 || p === '/sw.js') return;
  if (/^\/s\/[^/]+\/$/.test(p)) { e.respondWith(shell(req)); return; }                       // shopper page only (not /admin/)
  if (p.indexOf('/static/') === 0 && p.indexOf('/static/admin/') !== 0) { e.respondWith(swr(req)); return; }
});

// cached page now, refreshed in the background
function shell(req) {
  return caches.open(V).then(function (c) {
    return c.match(SHELL).then(function (hit) {
      var net = fetch(req).then(function (res) { if (res.ok) c.put(SHELL, res.clone()); return res; });
      if (hit) { net.catch(function () { }); return hit; }
      return net;
    });
  }).catch(function () { return new Response('You are offline and this page was never opened before.', { status: 503, headers: { 'Content-Type': 'text/plain' } }); });
}
function swr(req) {
  return caches.open(V).then(function (c) {
    return c.match(req).then(function (hit) {
      var net = fetch(req).then(function (res) { if (res.ok) c.put(req, res.clone()); return res; });
      if (hit) { net.catch(function () { }); return hit; }
      return net;
    });
  });
}
