// node --test modules/kiosk/ : the helpers of kiosk.js and its syntax.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const vm = require('node:vm');
const { refreshDelay, indicatorOf, authBlob, dropsToken, retryDelay } = require('./kiosk.js');

test('refresh happens at 80% of the TTL', () => {
  assert.strictEqual(refreshDelay(43200), 34560000);
  assert.strictEqual(refreshDelay(0), 1000);
});

test('indicator has three states', () => {
  assert.deepStrictEqual(indicatorOf(null), { state: 'down', text: 'Edge offline' });
  assert.deepStrictEqual(indicatorOf({ edge: true, hub: false, pending_changes: 3 }), { state: 'offline', text: 'Hub offline - 3 pending' });
  assert.deepStrictEqual(indicatorOf({ edge: true, hub: false, pending_changes: 0 }), { state: 'offline', text: 'Hub offline' });
  assert.strictEqual(indicatorOf({ edge: true, hub: true }).state, 'online');
  assert.strictEqual(indicatorOf({ edge: true }).state, 'online'); // no sync: hub is omitted
});

test('SDK auth store blob', () => {
  const b = JSON.parse(authBlob({ token: 't', record: { id: 'x' }, ttl_s: 5 }));
  assert.deepStrictEqual(b, { token: 't', record: { id: 'x' }, model: { id: 'x' } });
});

test('only 401/403/410 drop the token; retries back off', () => {
  for (const s of [401, 403, 410]) assert.strictEqual(dropsToken(s), true);
  for (const s of [200, 423, 429, 500, 502, 503, 0]) assert.strictEqual(dropsToken(s), false);
  assert.strictEqual(retryDelay(0), 5000);
  assert.ok(retryDelay(3) > retryDelay(1));
  assert.ok(retryDelay(50) <= 80000);
});

// run kiosk.js in a vm with a fake DOM, fetch and timers
function boot(routes) {
  const store = {}, timers = [], log = [];
  const el = () => ({ style: {}, appendChild() {}, focus() {}, set textContent(v) { this._t = v; }, get textContent() { return this._t; } });
  const doc = {
    readyState: 'complete', body: el(), head: el(),
    createElement: el, getElementById: () => null, addEventListener() {}, dispatchEvent() {},
  };
  const sb = {
    document: doc, CustomEvent: function (n, o) { this.type = n; this.detail = o && o.detail; },
    location: { pathname: '/', hash: '', replace() {} }, history: { replaceState() {} },
    localStorage: { setItem: (k, v) => { store[k] = v; }, removeItem: (k) => { delete store[k]; } },
    setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; }, clearTimeout() {}, setInterval() { return 0; },
    fetch: (url, opt) => {
      log.push((opt && opt.method || 'GET') + ' ' + url);
      const h = routes[url];
      const r = typeof h === 'function' ? h() : h;
      if (r instanceof Error) return Promise.reject(r);
      return Promise.resolve({ status: r.status, ok: r.status < 300, json: () => Promise.resolve(r.body || {}) });
    },
  };
  sb.window = sb; sb.globalThis = sb;
  vm.createContext(sb);
  vm.runInContext(fs.readFileSync(__dirname + '/kiosk.js', 'utf8'), sb);
  return { sb, store, timers, log };
}
const tick = () => new Promise((r) => setImmediate(r));
const SESSION = { status: 200, body: { token: 'tok1', ttl_s: 100, record: { id: 'a' }, pin_required: true, lock_after_s: 0 } };
const status200 = { status: 200, body: { edge: true } };

test('session: 503 and 429 keep the token and retry; 401 drops it', async () => {
  let session = SESSION;
  const c = boot({ '/api/kiosk/session': () => session, '/api/kiosk/status': status200, '/scan/wedge.js': status200 });
  await tick();
  assert.strictEqual(c.sb.toki.token, 'tok1');
  for (const st of [503, 429]) {
    session = { status: st };
    const before = c.timers.length;
    c.timers[c.timers.length - 1].fn(); // the refresh timer
    await tick();
    assert.strictEqual(c.sb.toki.token, 'tok1', 'token kept on ' + st);
    assert.ok(c.store.pocketbase_auth, 'SDK store kept on ' + st);
    assert.strictEqual(c.sb.toki.paired, true);
    assert.strictEqual(c.sb.toki.down, true);
    assert.ok(c.timers.length > before, 'a retry was scheduled');
  }
  session = { status: 401 };
  c.timers[c.timers.length - 1].fn();
  await tick();
  assert.strictEqual(c.sb.toki.token, '');
  assert.strictEqual(c.sb.toki.paired, false);
  assert.strictEqual(c.store.pocketbase_auth, undefined);
});

test('session: a network error keeps the token', async () => {
  let session = SESSION;
  const c = boot({ '/api/kiosk/session': () => session, '/api/kiosk/status': status200 });
  await tick();
  session = new Error('offline');
  c.timers[c.timers.length - 1].fn();
  await tick();
  assert.strictEqual(c.sb.toki.token, 'tok1');
  assert.strictEqual(c.sb.toki.down, true);
});

test('lock: shown only after the edge confirmed it', async () => {
  let lockRes = { status: 503 };
  const c = boot({ '/api/kiosk/session': SESSION, '/api/kiosk/status': status200, '/api/kiosk/lock': () => lockRes });
  await tick();
  assert.strictEqual(await c.sb.toki.lock(), false);
  assert.strictEqual(c.sb.toki.locked, false, 'not locked on a failed POST');
  assert.strictEqual(c.sb.toki.token, 'tok1', 'session kept');
  assert.ok(c.store.pocketbase_auth);
  assert.strictEqual(c.sb.toki.lockError, true);
  lockRes = new Error('down');
  assert.strictEqual(await c.sb.toki.lock(), false);
  assert.strictEqual(c.sb.toki.locked, false);
  lockRes = { status: 200, body: { locked: true } };
  assert.strictEqual(await c.sb.toki.lock(), true);
  assert.strictEqual(c.sb.toki.locked, true);
  assert.strictEqual(c.sb.toki.token, '');
  assert.strictEqual(c.store.pocketbase_auth, undefined);
});

test('store(): blocked localStorage does not break the token', async () => {
  const c = boot({ '/api/kiosk/session': SESSION, '/api/kiosk/status': status200 });
  await tick();
  c.sb.localStorage.setItem = () => { throw new Error('blocked'); };
  c.timers[c.timers.length - 1].fn();
  await tick();
  assert.strictEqual(c.sb.toki.token, 'tok1');
});
