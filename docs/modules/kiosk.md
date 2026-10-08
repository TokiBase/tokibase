# Module `kiosk`

Pairing and sessions for a locked-down browser on an edge device (a Pi or mini PC that boots into Chromium kiosk mode), with an offline indicator and a PIN lock. Package `modules/kiosk`, plan in `docs/EDGE_MODULES_PLAN.md` section 4. Additive: no change to any PocketBase endpoint.

- Opt in with env `TOKI_KIOSK=on` (default off). Then the collection, the routes and the `toki kiosk` command exist.
- Build tag `no_kiosk` compiles the module out (the `nano` profile does). Setting `TOKI_KIOSK=on` on such a binary is refused at boot by the stubbed-module guard.
- Non-goals: no window manager, no browser launcher, no OS image (see "Chromium kiosk unit" below, which is ops documentation), no UI framework (serve the app with `--publicDir`).
- The PIN is a UI gate against a walk-up customer, not a security boundary against someone with a shell or network access to the API.

## Data model

`_kiosk_devices` is a system collection with `null` rules (superusers only). It may be synced pull-only to provision a fleet once `modules/sync` admits system collections.

| Field | Notes |
| --- | --- |
| `name` | unique, `A-Z a-z 0-9 _ . -`, 1 to 64 characters |
| `token_hash` | hex SHA-256 of the 32-byte device token (the token itself is only in the browser cookie) |
| `pairing_hash`, `pairing_expires` | hex SHA-256 of the one-time pairing code and its expiry (15 minutes by default) |
| `auth_collection`, `auth_record` | the service actor, required. Never a superuser, never a system collection (`_agents`, ...); it must exist (checked when the actor is set and on every session, so `revoke`, `rotate` and `set-pin` still work after the actor was deleted) |
| `node_id` | optional. When set and sync is on, sessions are refused on a node with another id |
| `bind_ip` | optional client IP or CIDR; every device route refuses other addresses. It compares the address PocketBase resolves (`RealIP`): behind a reverse proxy on the same host set `Settings.TrustedProxy.Headers`, otherwise every client looks like the proxy (`127.0.0.1`) and `bind_ip` means nothing |
| `pin_hash` | bcrypt of the PIN (4 to 64 characters); empty = the device cannot be locked |
| `locked` | the device is locked: `session` answers 423 until `unlock` |
| `ttl_hours` | session lifetime, default 12 (`TOKI_KIOSK_SESSION_HOURS` changes the default) |
| `lock_after_s` | idle seconds before `kiosk.js` locks the screen (0 = never) |
| `pin_fail_count`, `pin_lockouts`, `pin_locked_until` | the PIN brake, persisted so a restart does not reset it |
| `last_seen` | updated at most once a minute |

Two plain tables (not collections) hold the revocation state: `_kiosk_sessions` (`device`, `sid`, `issued`, `expires`: every session id issued to a device, expired rows are purged) and `_kiosk_gen` (`device`, `gen`: the revocation generation).

## Flow

1. `toki kiosk provision --name gate-1 --actor gate_devices/abc [--pin 1234] [--lock-after 120] [--bind-ip 127.0.0.1]` creates the row and prints `http://127.0.0.1:8090/kiosk/pair#<code>`. The code is in the fragment, so it never reaches a log. `--url` changes the base.
2. Open that URL once in the kiosk browser. `kiosk.js` reads the fragment, removes it from the address bar and posts it to `POST /api/kiosk/pair`. The code is single use (one conditional `UPDATE` claims it) and expires. The answer sets `toki_kiosk=<token>; HttpOnly; SameSite=Strict; Path=/api/kiosk` (and `Secure` over HTTPS, also behind a proxy sending `X-Forwarded-Proto: https`).
3. Pairing is accepted only from a loopback peer without proxy headers, unless `TOKI_KIOSK_ALLOW_REMOTE=1`.
4. `POST /api/kiosk/session` (cookie) compares the token hash in constant time, checks `bind_ip`, `node_id` and that the actor still exists, then returns a normal PocketBase auth token of the actor with `ttl_hours` lifetime (`NewStaticAuthToken`, so the SDK's `authRefresh()` returns the same token and never extends it). The token carries the claims `kiosk_dev` and `kiosk_gen`: collection rules, `sessions`, `fieldperm` and sync actor capture behave as for any client. The token is a local token and keeps working while the hub is unreachable.
5. `kiosk.js` refreshes the session at 80% of the TTL.
6. `POST /api/kiosk/lock` marks the device locked and revokes **every** token of the device: it bumps the device generation (a request middleware drops the auth of any token whose `kiosk_gen` is stale, within one request, with or without the `sessions` module) and revokes each recorded `sid` through the `sessions` module when present (closing realtime streams). The `Authorization` token of the request is only used when the auth loader verified it and it belongs to the device actor. `POST /api/kiosk/unlock {"pin":"..."}` checks bcrypt and issues a new session; if no session can be issued the device stays locked. A locked kiosk has no token, so it cannot call the API. `toki kiosk revoke` and `rotate` do the same revocation, also when the actor was deleted. Without the `sessions` module realtime streams opened before the lock stay open until they disconnect.
7. PIN brake, per device and persisted in `_kiosk_devices`: the check, the bcrypt compare and the counter update run under one per-device mutex, so parallel attempts cannot get past it. 5 wrong PINs lock unlock attempts for 60 s, each following lockout doubles (up to 1 h); during a lockout even the right PIN answers 429 with `Retry-After`. A restart keeps the brake. A good PIN resets it. On top of it `pair` (20 per minute) and `unlock` (30 per minute) have a built-in per-IP throttle (429 `rate_limited`) that does not depend on any PocketBase rate-limit rule; you can still add a `kiosk` rule for `session` and `lock`.

The token lifetime is no longer capped when `sessions` is absent: revocation is by generation, not by expiry. `kiosk_dev` and `kiosk_gen` are plain claims signed with the actor key, so they cannot be forged or removed.

## Routes

| Route | Auth | Notes |
| --- | --- | --- |
| `GET /kiosk/kiosk.js` | none | embedded script, about 7 KB, `Cache-Control: max-age=300` |
| `GET /kiosk/pair` | none | page that loads `kiosk.js` and consumes the code |
| `POST /api/kiosk/pair` | code | `{code}`; loopback only unless `TOKI_KIOSK_ALLOW_REMOTE=1` |
| `POST /api/kiosk/session` | device cookie | token of the actor; 423 while locked |
| `GET /api/kiosk/status` | device cookie or any auth record | see below |
| `POST /api/kiosk/lock` | device cookie | 409 when the device has no PIN |
| `POST /api/kiosk/unlock` | device cookie | `{pin}`; 401 wrong PIN, 429 during a lockout |

`pair`, `session`, `lock` and `unlock` use the rate-limit tag `kiosk` (effective only when a rule for it exists; `pair` and `unlock` are throttled per IP regardless). Errors are `{status, message, data:{code}}` with codes `not_paired`, `bad_code`, `remote_pairing`, `bind_ip`, `actor_gone`, `node_mismatch`, `locked`, `no_pin`, `bad_pin`, `pin_locked`, `rate_limited`.

`GET /api/kiosk/status`:

```json
{"edge":true,"hub":false,"pending_changes":7,"last_sync":"2026-10-09T08:00:00Z",
 "printers":[{"name":"counter","state":"ok"}],"time":"2026-10-09T08:05:00Z"}
```

`hub`, `pending_changes` and `last_sync` come from `kernel.SyncStatusOf` and are omitted when sync is off or compiled out. `printers` lists the enabled printers from the health block of the `printer` module (empty when that module is off); the kiosk module does not import it.

## `kiosk.js`

Add `<script src="/kiosk/kiosk.js"></script>` to the app page (served with `--publicDir`). It provides:

- `window.toki.token`, `window.toki.record`, `toki.lock()`, `toki.unlock(pin)`, and document events `toki:token`, `toki:lock`, `toki:lockerror`, `toki:status`, `toki:scan`.
- Error handling: only 401, 403 and 410 from `/session` unpair the browser and drop the token. On 429, 5xx and network errors the token is kept, the pill shows red "Edge down" and the call is retried with backoff (5 s doubling to 60 s). `toki.lock()` shows the lock screen only after the edge answered 200; on a failure the session stays, the pill shows "Lock failed - retrying" and the idle auto-lock retries.
- The JS SDK store: the token is written to `localStorage` keys `pocketbase_auth` (the SDK default) and `pb_auth` (`{token, record, model}`; `model` is for SDKs before 0.26; the record has only id and collection, call `authRefresh()` for the full record), so `new PocketBase()` works unchanged. Locking removes both.
- A corner pill polling `/api/kiosk/status` every 5 s: red "Edge offline" (the fetch failed), amber "Hub offline - N pending", green "Online" (also shown when sync is not running on the node). Grey "Not paired" when the cookie is missing or revoked.
- The PIN overlay, and idle auto-lock after `lock_after_s` seconds without input (only when the device has a PIN).
- It loads `/scan/wedge.js` when present and starts it with the current token; scans arrive as `toki:scan`.

Tests (helpers and the session/lock behavior against a fake DOM and fetch): `node --test modules/kiosk/kiosk.test.js`.

The device cookie is SameSite=Strict, but SameSite is per site, not per origin: another web app on a different port of the same host can still send credentialed requests to `/api/kiosk/lock` (forced lock; the PIN unlocks). Serve only trusted apps on the edge host.

## CLI

```
toki kiosk provision --name N --actor col/id [--pin P] [--bind-ip IP|CIDR] [--node-id ID] [--ttl-hours H] [--lock-after S] [--expires 15m] [--url BASE]
toki kiosk list [--json]
toki kiosk rotate <name> [--expires 15m] [--url BASE]   # drops the cookie, prints a new pairing URL
toki kiosk revoke <name> [--sessions]                   # unpair; --sessions also kills every session of the actor
toki kiosk set-pin <name> (--pin P | --clear)
```

`rotate` and `revoke` end every token issued to that device (generation plus recorded sessions). `--sessions` additionally ends all sessions of the actor, which also hits other devices sharing it and the actor's own logins. The `kiosk` command exists only when the process runs with `TOKI_KIOSK=on`, so use `TOKI_KIOSK=on toki kiosk ...` in a shell.

## Audit

`kiosk.pair` (device, ip) and `kiosk.unlock_fail` (device, ip, failure count, lockout seconds) go to the audit log when the `audit` module is on.

## Env

| Variable | Default | Meaning |
| --- | --- | --- |
| `TOKI_KIOSK` | off | `on` enables the module |
| `TOKI_KIOSK_ALLOW_REMOTE` | off | allow `pair` from a non-loopback peer |
| `TOKI_KIOSK_SESSION_HOURS` | 12 | default session lifetime of devices without `ttl_hours` (1 to 720) |

## Chromium kiosk unit (ops)

The binary does not launch a browser. On a Pi with a desktop session (or cage/labwc), a systemd user unit is enough:

```ini
# ~/.config/systemd/user/kiosk-browser.service
[Unit]
Description=Chromium kiosk
After=graphical-session.target

[Service]
ExecStart=/usr/bin/chromium --kiosk --noerrdialogs --disable-infobars --disable-session-crashed-bubble \
  --no-first-run --disable-translate --disable-pinch --overscroll-history-navigation=0 \
  --check-for-update-interval=31536000 --user-data-dir=/var/lib/kiosk/chromium \
  http://127.0.0.1:8090/
Restart=always
RestartSec=3

[Install]
WantedBy=graphical-session.target
```

Notes: keep `--user-data-dir` on a persistent path, because the device cookie lives in that profile; pair once with `chromium --user-data-dir=/var/lib/kiosk/chromium --app='<pairing URL>'` (the one-time code is then visible in the process list for its lifetime, 15 minutes by default; use a short `--expires`). Run `toki serve --publicDir /opt/app` as its own system unit with `Environment=TOKI_KIOSK=on`. Pair from the device itself so the loopback rule holds; otherwise set `TOKI_KIOSK_ALLOW_REMOTE=1` for the pairing and unset it again. Add `bind_ip` for a LAN-reachable edge (and configure trusted proxy headers when a proxy fronts it, see `bind_ip` above). Use HTTPS (`docs/modules/tlscheck.md` lists the checks) before exposing the edge beyond loopback.
