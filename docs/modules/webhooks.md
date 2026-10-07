# Module `webhooks`

Outbound webhooks for record, collection and auth events: HMAC-signed HTTP POSTs with retries, backoff, dead-letter, replay and a CLI. Package `modules/webhooks`. No REST contract change for existing endpoints.

- Enabled by default: `tokibase.go` calls `webhooks.Register(app)` and adds the `webhooks` command.
- Disable with env `TOKI_WEBHOOKS=off` (also `false`, `0`, `disabled`). Then no hooks, no worker, no CLI command.
- Delivery runs inside the `serve` process (workers start on `OnServe`). Events produced by other processes (CLI, `pb_hooks`-less Go code) are queued in the same table and sent by the server.

## Configuration

Collection `_webhooks` in `data.db` (created on bootstrap, superuser-only rules, so it is reachable through the normal collections API and the Admin UI). The `secret` field is `hidden`: it never appears in API responses (it is stored as-is in phase 1, not encrypted).

| Field | Notes |
| --- | --- |
| `name` | unique |
| `url` | absolute `http(s)` URL (validated on save) |
| `secret` | HMAC key, hidden, required, at least 16 characters (validated on save, also through the admin API) |
| `events` | JSON list: exact names or patterns `record.*`, `collection.*`, `auth.*`, `*` |
| `collections` | JSON list of collection names; empty = all collections |
| `enabled` | bool; disabled webhooks get no new deliveries, queued ones wait |
| `headers` | JSON map of extra request headers (the `X-Toki-*`, `Content-Type` and `User-Agent` headers always win). `Host`, `Content-Length`, `Transfer-Encoding`, `Connection` and `X-Toki-*` names and values with CR/LF are rejected. Values usually hold bearer tokens: `toki webhooks list --json` masks them, but the superuser API returns them |
| `timeout_ms` | default 10000, clamped to 100..30000 |
| `max_attempts` | default 8 |

The config is cached in memory, invalidated on any `_webhooks` record event of this process, with a 5 s TTL. Once the cache is older than 5 s, event capture re-reads the webhooks from the database before choosing the deliveries, so a webhook added or enabled by the CLI or another node is seen within at most 5 s (events captured in that window by this process are not delivered to it; there is no backfill). If the reload fails, the stale cache is used and retried on the next event; with no cache at all the event is logged as dropped. Secrets are stored in clear in the main database (and its backups).

## Events

| Event | Fires on | `collection` / `record_id` |
| --- | --- | --- |
| `record.create` / `record.update` / `record.delete` | kernel hooks `OnRecordAfter{Create,Update,Delete}Success` (after commit; REST, Go code, CLI alike) | record collection / record id |
| `collection.create` / `collection.update` / `collection.delete` | `OnCollectionAfter{Create,Update,Delete}Success` | collection name / collection id |
| `auth.login` | `OnRecordAuthRequest` with a non-empty auth method (password, OTP, OAuth2); not token refresh or impersonation | auth collection / auth record id |
| `ping` | `toki webhooks test` only | empty |

Collections whose name starts with `_` (system collections, including `_webhooks` and `_superusers`) never produce record, collection or `auth.login` events.

## Payload

```json
{
  "id": "k3j9x0a2b7c1d4e",
  "event": "record.update",
  "created": "2026-10-07 04:02:51.552Z",
  "collection": "orders",
  "record_id": "efu69b4ihx4sfb2",
  "seq": 42,
  "data": { "id": "efu69b4ihx4sfb2", "collectionName": "orders", "status": "paid" },
  "old": { "status": "new" },
  "changed": ["status"]
}
```

- `seq` is a per-webhook counter (also sent as header `X-Toki-Seq`), allocated when the event is captured. It lets receivers detect gaps and reorder, but delivery order is NOT guaranteed (retries run on independent timers). Ping events carry no `seq`.
- Payloads above 256 KB (`TOKI_WEBHOOK_MAX_PAYLOAD_BYTES`) are replaced by a marker: `"truncated": true`, `data` reduced to `{"id": <record id>}`, no `old`; fetch the record from the API.
- `data` is the record's public export: hidden fields, `password` and `tokenKey` are never included; auth `email` follows `emailVisibility`. `expand` data is included when present. Collection events carry the collection JSON with secret-looking keys (`*secret*`, `*password*`, `token`, `apiKey`, ...) removed.
- `old` and `changed` exist for `record.update` only: old values of the changed fields that are visible in the public export of both the previous and the new state (so an auth `email` appears in `old`/`changed` only while `emailVisibility` is true; `password` and `tokenKey` never). When the record object had no pristine copy (created and updated through the same Go object) they are omitted.
- `auth.login` data: `{id, method, ip}`; the token is never sent.
- `id` identifies the event; `X-Toki-Delivery` identifies the delivery (one per webhook), use it for idempotency on retries.

## Signature

Headers on every request: `Content-Type: application/json`, `User-Agent: TokiBase-Webhooks/1`, `X-Toki-Event`, `X-Toki-Delivery`, `X-Toki-Timestamp` (unix seconds), `X-Toki-Signature: v1=<hex hmac-sha256(secret, timestamp + "." + body)>`. Success is any 2xx.

Verify against the raw body (not re-serialized JSON), compare in constant time and reject old timestamps (replays) with a window of about 5 minutes.

Node:

```js
const crypto = require("crypto");

function verify(rawBody, headers, secret, toleranceSec = 300) {
  const ts = headers["x-toki-timestamp"];
  if (Math.abs(Date.now() / 1000 - Number(ts)) > toleranceSec) return false;
  const expected = "v1=" + crypto.createHmac("sha256", secret).update(ts + "." + rawBody).digest("hex");
  const got = headers["x-toki-signature"] || "";
  return got.length === expected.length &&
    crypto.timingSafeEqual(Buffer.from(got), Buffer.from(expected));
}
```

Python:

```python
import hashlib, hmac, time

def verify(raw_body: bytes, headers: dict, secret: str, tolerance: int = 300) -> bool:
    ts = headers["X-Toki-Timestamp"]
    if abs(time.time() - int(ts)) > tolerance:
        return False
    expected = "v1=" + hmac.new(secret.encode(), ts.encode() + b"." + raw_body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(headers.get("X-Toki-Signature", ""), expected)
```

## Deliveries, retries, dead-letter

Table `_webhook_deliveries` in `auxiliary.db` (`CREATE TABLE IF NOT EXISTS`, same approach as `_audit`): `id, webhook, event, collection, record, payload, attempt, state, next_at, last_status, last_error, response_ms, created, updated, seq` (plus `_webhook_seq`, the per-webhook counter; older tables are migrated in place).

| State | Meaning |
| --- | --- |
| `queued` | not tried yet (or replayed), due at `next_at` |
| `failed` | last attempt failed, retry scheduled at `next_at` |
| `delivered` | a 2xx was received |
| `dead` | `max_attempts` reached, a 3xx response (not retried), the webhook was disabled or deleted, or a failed `ping` |

A pool of workers (env `TOKI_WEBHOOK_WORKERS`, default 4) polls due rows every second (and immediately after a new event). A claim pushes `next_at` 2 minutes ahead, so a crashed worker's row becomes due again. Backoff after the n-th failed attempt: `10 s * 2^(n-1)`, capped at 1 h, with +-20 % jitter (10 s, 20 s, 40 s, ...). When a delivery turns `dead` the log gets `webhook.dead` and, if the audit module is on, an `_audit` entry (`webhooks.SetAuditSink`, wired in `tokibase.go`). `last_error` holds the transport error or `HTTP <code>: <first 4 KB of the body>`.

Shutdown: in-flight deliveries are interrupted and given back (`next_at` = now, attempt count unchanged); a deploy never charges an attempt or dead-letters a delivery.

Deliveries of a disabled webhook are not retried: when claimed they turn `dead` (`webhook disabled ...`, no attempt charged, no audit entry) and can be replayed after enabling it.

Delivered rows are pruned after 7 days (`TOKI_WEBHOOK_RETENTION_HOURS`); `failed` and `dead` rows (and their payloads, which may hold personal data) after 30 days (`TOKI_WEBHOOK_DEAD_RETENTION_HOURS`). Delivery is at-least-once: receivers must dedupe on `X-Toki-Delivery`/`id`.

## SSRF rules

- Every outgoing connection is checked after DNS resolution (at dial time, so DNS rebinding does not help). Loopback, private (RFC 1918, `fc00::/7`), link-local (incl. `169.254.169.254`), CGNAT `100.64.0.0/10`, `0.0.0.0/8`, `240.0.0.0/4`, NAT64 `64:ff9b::/96` and `64:ff9b:1::/48`, 6to4 `2002::/16`, Teredo `2001::/32`, `::/96`, site-local `fec0::/10`, unspecified and multicast addresses are refused; the delivery fails with `webhook target resolves to a private ...`.
- Override for development or intranet receivers: `TOKI_WEBHOOK_ALLOW_PRIVATE=1`.
- Redirects are never followed (a 3xx is a terminal failure: the delivery turns `dead` at once, no retries). `HTTP(S)_PROXY` is ignored (a proxy would bypass the guard).
- Response bodies are read up to 4 KB only.

## CLI

```
toki webhooks list [--json]
toki webhooks add --name N --url U --events a,b [--secret S] [--collections x,y]   # secret generated and printed once when omitted
toki webhooks rm <name>
toki webhooks test <name>                       # synchronous ping, prints status, exit 1 on failure
toki webhooks deliveries [--state queued|failed|delivered|dead] [--limit 50] [--json]
toki webhooks replay <delivery-id> [--now] [--force]   # re-queue a dead/failed delivery (attempt restarts at 0); --now delivers once from this process; --force also replays delivered/in-flight rows (duplicate)
toki webhooks replay --dead                     # re-queue every dead delivery
```

`list --json` never prints secrets and masks header values (`***`). Prefer the generated secret: `--secret` ends up in shell history. Payloads and per-delivery timeouts: see above. Default worker count is 4 (a slow receiver can still occupy workers up to its timeout, at most 30 s; there is no per-webhook concurrency cap).

## Implementation note: moving to the kernel job queue

The delivery queue is a private table and worker pool because the kernel `JobQueue` (`modules/jobs`) is not available yet. All delivery logic lives in one function, `deliver(ctx, app, deliveryID)`; the worker only claims a row and calls it. Moving to the job queue means enqueuing a job `webhooks.deliver{deliveryID}` in `enqueue`, registering `deliver` as its handler, and dropping `claimNext`, `worker` and the retry scheduling in `finish` in favor of the queue's retry/backoff. The `_webhook_deliveries` table stays as the delivery log.

## Not covered (phase 1)

Secrets are stored in clear (hidden from the API only), no per-webhook rate limit or concurrency cap, no ordering guarantee between events (use `seq`), no batching, events from `pb_data` restores or direct SQL are not seen, cluster-wide claim coordination (one writing process per `pb_data`).
