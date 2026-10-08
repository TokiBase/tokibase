// node --test modules/kiosk/ : the helpers of kiosk.js and its syntax.
const test = require('node:test');
const assert = require('node:assert');
const { refreshDelay, indicatorOf, authBlob } = require('./kiosk.js');

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
  assert.deepStrictEqual(b, { token: 't', record: { id: 'x' } });
});
