/* TokiBase kiosk runtime. Served at /kiosk/kiosk.js; add it to the app page:
 *   <script src="/kiosk/kiosk.js"></script>
 * It trades the device cookie for an auth token of the service actor, keeps it
 * fresh (80% of the TTL), exposes window.toki.token and writes the JS SDK auth
 * store key (pocketbase_auth, plus pb_auth), draws the connectivity pill (polls
 * /api/kiosk/status every 5 s), the PIN lock overlay with idle auto-lock, and
 * loads /scan/wedge.js when the scanner module is on. Events on document:
 * toki:token, toki:lock, toki:status, toki:scan. On /kiosk/pair it consumes the
 * one-time code in the URL fragment. */
(function (g) {
  'use strict';
  var API = '/api/kiosk', POLL = 5000;

  function refreshDelay(ttlS) { return Math.max(1000, Math.floor(ttlS * 800)); }
  function indicatorOf(st) {
    if (!st) return { state: 'down', text: 'Edge offline' };
    if (st.hub === false) {
      return { state: 'offline', text: 'Hub offline' + (st.pending_changes ? ' - ' + st.pending_changes + ' pending' : '') };
    }
    return { state: 'online', text: 'Online' };
  }
  function authBlob(s) { return JSON.stringify({ token: s.token, record: s.record }); }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { refreshDelay: refreshDelay, indicatorOf: indicatorOf, authBlob: authBlob };
  }
  var d = g.document;
  if (!d || !g.fetch) return;

  var toki = g.toki = { token: '', record: null, locked: false, paired: true, status: null, lock: lock, unlock: unlock };
  var lockAfter = 0, pinRequired = false, refreshT, idleT, pill, overlay, msg, input;

  function emit(n, detail) { d.dispatchEvent(new CustomEvent('toki:' + n, { detail: detail })); }
  function store(s) {
    try {
      if (s) { var b = authBlob(s); localStorage.setItem('pocketbase_auth', b); localStorage.setItem('pb_auth', b); }
      else { localStorage.removeItem('pocketbase_auth'); localStorage.removeItem('pb_auth'); }
    } catch (e) { /* storage may be blocked; window.toki.token still works */ }
  }
  function post(path, body) {
    return fetch(API + path, {
      method: 'POST', credentials: 'same-origin', cache: 'no-store',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {})
    }).then(function (r) { return r.json().catch(function () { return {}; }).then(function (j) { return { status: r.status, body: j }; }); });
  }

  function apply(s) {
    toki.token = s.token; toki.record = s.record; toki.locked = false; toki.paired = true;
    lockAfter = s.lock_after_s || 0; pinRequired = !!s.pin_required;
    store(s); hideOverlay(); emit('token', s.token);
    clearTimeout(refreshT);
    refreshT = setTimeout(session, refreshDelay(s.ttl_s));
    resetIdle();
  }
  function session() {
    return post('/session').then(function (r) {
      if (r.status === 200) return apply(r.body);
      if (r.status === 423) { pinRequired = true; return showLock(); }
      toki.paired = false; clearToken(); draw();
    }).catch(function () { clearTimeout(refreshT); refreshT = setTimeout(session, POLL); });
  }
  function clearToken() { toki.token = ''; toki.record = null; store(null); clearTimeout(refreshT); }

  function lock() {
    if (!pinRequired) return Promise.resolve(false);
    var auth = toki.token;
    clearToken(); showLock();
    return fetch(API + '/lock', { method: 'POST', credentials: 'same-origin', headers: auth ? { Authorization: auth } : {} })
      .then(function () { return true; }, function () { return true; });
  }
  function note(t) { if (msg) msg.textContent = t; }
  function unlock(pin) {
    return post('/unlock', { pin: pin }).then(function (r) {
      if (r.status === 200) { apply(r.body); return true; }
      note(r.status === 429 ? 'Too many attempts. Wait and try again.' : 'Wrong PIN');
      return false;
    }, function () { note('Edge unreachable'); return false; });
  }

  function el(tag, css, parent) {
    var e = d.createElement(tag); e.style.cssText = css; (parent || d.body).appendChild(e); return e;
  }
  function showLock() {
    toki.locked = true; clearToken(); clearTimeout(idleT);
    if (!overlay) {
      overlay = el('div', 'position:fixed;inset:0;z-index:2147483646;background:#111;color:#fff;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:12px;font:20px system-ui');
      el('div', 'font-size:28px', overlay).textContent = 'Locked';
      input = el('input', 'font:28px monospace;text-align:center;width:9em;padding:8px', overlay);
      input.type = 'password'; input.inputMode = 'numeric'; input.autocomplete = 'off';
      msg = el('div', 'min-height:1.4em;color:#f88', overlay);
      var go = el('button', 'font:20px system-ui;padding:8px 28px', overlay); go.textContent = 'Unlock';
      var submit = function () { var p = input.value; input.value = ''; unlock(p); };
      go.onclick = submit;
      input.onkeydown = function (e) { if (e.key === 'Enter') submit(); };
    }
    overlay.style.display = 'flex'; msg.textContent = ''; input.value = ''; input.focus();
    emit('lock', true);
  }
  function hideOverlay() { if (overlay && overlay.style.display !== 'none') { overlay.style.display = 'none'; emit('lock', false); } }
  function resetIdle() {
    clearTimeout(idleT);
    if (lockAfter > 0 && pinRequired && !toki.locked) idleT = setTimeout(lock, lockAfter * 1000);
  }
  ['pointerdown', 'keydown', 'touchstart', 'mousemove'].forEach(function (n) { d.addEventListener(n, resetIdle, true); });

  function draw() {
    if (!d.body) return;
    var i = indicatorOf(toki.status), c = { down: '#c0392b', offline: '#d68910', online: '#1e8449' }[i.state];
    if (!toki.paired) { i = { text: 'Not paired' }; c = '#7f8c8d'; }
    if (!pill) pill = el('div', 'position:fixed;right:8px;bottom:8px;z-index:2147483645;padding:4px 10px;border-radius:12px;color:#fff;font:13px system-ui;pointer-events:none;opacity:.9');
    pill.style.background = c; pill.textContent = i.text;
  }
  function poll() {
    fetch(API + '/status', { credentials: 'same-origin', cache: 'no-store' })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (s) { toki.status = s; }, function () { toki.status = null; })
      .then(function () { draw(); emit('status', toki.status); });
  }

  function loadWedge() {
    var s = d.createElement('script'); s.src = '/scan/wedge.js';
    s.onload = function () {
      if (g.TokiScan) g.TokiScan.start({ token: function () { return toki.token; }, onScan: function (r) { emit('scan', r); } });
    };
    d.head.appendChild(s);
  }

  function pair() {
    var code = (g.location.hash || '').slice(1), out = d.getElementById('toki-pair');
    try { g.history.replaceState(null, '', g.location.pathname); } catch (e) { /* ignore */ }
    if (!code) { if (out) out.textContent = 'Missing pairing code.'; return; }
    post('/pair', { code: code }).then(function (r) {
      if (r.status === 200) { g.location.replace('/'); return; }
      if (out) out.textContent = 'Pairing failed: ' + ((r.body && r.body.message) || r.status);
    }, function () { if (out) out.textContent = 'Edge unreachable'; });
  }

  function start() {
    if (g.location.pathname === '/kiosk/pair') return pair();
    draw(); session(); poll(); setInterval(poll, POLL); loadWedge();
  }
  if (d.readyState === 'loading') d.addEventListener('DOMContentLoaded', start); else start();
})(typeof window !== 'undefined' ? window : globalThis);
