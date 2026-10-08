/* TokiBase scanner wedge: captures keyboard-wedge barcode scanner bursts in the
 * browser and posts them to POST /api/scan. Served at /scan/wedge.js.
 *   const stop = TokiScan.start({ token: () => pb.authStore.token, scanner: 'door',
 *                                 onScan: (r) => console.log(r.code) });
 * A burst is a run of keys with at most maxGap ms between them, at least minLen
 * characters, ended by Enter; ordinary typing is left alone. */
(function (g) {
  var session = Math.random().toString(36).slice(2, 8), n = 0;
  g.TokiScan = {
    start: function (o) {
      o = o || {};
      var base = o.url || '', minLen = o.minLen || 4, maxGap = o.maxGap || 35;
      var buf = '', last = 0, timer;
      function send(code) {
        var seq = session + '-' + (++n), tries = 0;
        (function go() {
          fetch(base + '/api/scan', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', Authorization: o.token ? o.token() : '' },
            body: JSON.stringify({ scanner: o.scanner, code: code, client_seq: seq })
          }).then(function (r) {
            return r.json().then(function (j) { if (r.ok) { o.onScan && o.onScan(j); } else { o.onError && o.onError(j); } });
          }).catch(function (e) {
            if (++tries < 4) { timer = setTimeout(go, 300 * tries); } else { o.onError && o.onError(e); }
          });
        })();
      }
      function onKey(e) {
        var t = e.timeStamp || Date.now();
        if (t - last > maxGap) buf = '';
        last = t;
        if (e.key === 'Enter') {
          var code = buf; buf = '';
          if (code.length >= minLen) { e.preventDefault(); send(code); }
        } else if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
          buf += e.key;
        }
      }
      g.addEventListener('keydown', onKey, true);
      return function () { g.removeEventListener('keydown', onKey, true); clearTimeout(timer); };
    }
  };
})(window);
