(function () {
  'use strict';
  var slug = location.pathname.split('/')[2], NS = 'http://www.w3.org/2000/svg';
  document.getElementById('print').addEventListener('click', function () { window.print(); });
  fetch('/api/v1/s/' + encodeURIComponent(slug) + '/sample?n=24').then(function (r) { return r.json(); }).then(function (d) {
    document.getElementById('t').textContent = 'Test barcodes: ' + d.name;
    var grid = document.getElementById('grid'), n = 0;
    d.items.forEach(function (it) {
      var code = it.code;
      if (!/^[0-9]{13}$|^[0-9]{8}$/.test(code)) return;
      var g = window.PTEAN.svg(code, { module: 3, height: 90 });
      var svg = document.createElementNS(NS, 'svg');
      svg.setAttribute('viewBox', '0 0 ' + g.width + ' ' + (g.height + 24));
      svg.setAttribute('role', 'img'); svg.setAttribute('aria-label', 'Barcode ' + code);
      var bg = document.createElementNS(NS, 'rect'); bg.setAttribute('width', g.width); bg.setAttribute('height', g.height + 24); bg.setAttribute('fill', '#fff'); svg.appendChild(bg);
      var path = document.createElementNS(NS, 'path'); path.setAttribute('d', g.path); path.setAttribute('transform', 'translate(0,4)'); path.setAttribute('fill', '#000'); svg.appendChild(path);
      var tx = document.createElementNS(NS, 'text'); tx.setAttribute('x', g.width / 2); tx.setAttribute('y', g.height + 20); tx.setAttribute('text-anchor', 'middle'); tx.setAttribute('font-size', '15'); tx.setAttribute('font-family', 'monospace'); tx.textContent = code; svg.appendChild(tx);
      var fig = document.createElement('figure'), cap = document.createElement('figcaption'), b = document.createElement('b');
      b.textContent = it.name + (it.size ? ' ' + it.size : ''); cap.appendChild(b);
      var sm = document.createElement('code'); sm.textContent = 'shelf price (live): see the app'; cap.appendChild(sm);
      fig.appendChild(svg); fig.appendChild(cap); grid.appendChild(fig); n++;
    });
    document.getElementById('count').textContent = n + ' products';
  }).catch(function () { document.getElementById('count').textContent = 'Could not load products.'; });
})();
