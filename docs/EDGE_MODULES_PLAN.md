# Edge modules plan (kiosk, printer, scanner, device-cert)

Status: plan, 2026-10-08. Produced for the `edge` profile (parking gates, kiosks, signage, POS). Implementation tracked in docs/ARCHITECTURE.md Phase 3.


This plan comes from reading the docs and code listed in the brief. Nothing was built, run or modified.

## 0. Findings that shape the plan

1. **Module conventions.** Every module has these pieces:
   - `//go:build !no_x` real files.
   - `stub.go` and `stub_marker.go` under `no_x`.
   - `marker.go` calling `kernel.RegisterModuleMarker(name, collections, envs, stubbed)`.
   - Registration in `tokibase.go` behind `x.Enabled()`.
   - `NewCommand` for the CLI.
   - A `docs/modules/x.md`.
   - Modules never import each other. They talk through `kernel/*.go` interfaces such as `kernel.Jobs(app)` and `kernel.OnSyncConflictFor`. Shared helpers go in `internal/` (precedent: `internal/netguard`).
2. **`jobs` is usable as is.** `kernel.Jobs(app).Register/Enqueue` supports `Unique`, `Delay`, `MaxAttempts` and `MaxRuntime`. It is at-least-once and backoff is fixed (`5s*2^n`, cap 1h). Print handlers must be idempotent. Paper-out handling must not depend on the backoff curve.
3. **`netguard` is the wrong tool for printers.** It blocks private, loopback and link-local IPs, and printers live on the LAN. A printer dialer needs its own policy. The address comes only from the superuser-owned `_printers` row, never from request input. Allowed CIDRs default to RFC1918 plus loopback. Link-local and the 169.254.169.254 metadata address stay blocked.
4. **WASM cannot drive hardware, and edge excludes it.** The edge profile has `no_wasm` (the runtime costs about 2.8 MB). WASI has no serial, USB or raw-socket host API (`toki/1` only has `http_allow`). Drivers therefore stay in Go. WASM and the app only own business logic (section 6).
5. **`device-cert` is about 40% done.**
   - **Done:** an Ed25519 node identity bound to the node id. A hub-signed JWS device cert (EdDSA, 365 d, serial, renewed by the handshake when under 30 d remain). A signed handshake, revocation, hub TLS pinning, and keys from file or env.
   - **Gaps:**
     - **G1.** The cert is a JWS, not X.509. No TLS stack or browser can use it.
     - **G2.** The edge's own HTTP is plain on the LAN (`tlscheck` only warns). A tablet or gate controller on the LAN gets no encryption or server authentication.
     - **G3.** There is no way to authenticate a LAN peer, such as a gate controller or a handheld, to the edge.
     - **G4.** The app cannot get a signed statement of "which device am I talking to".
     - **G5.** There is no leaf rotation, no CA rotation, and no offline-safe revocation.
   - **Not a gap:** the node key and enrollment already exist.
6. **Static serving already exists.** `examples/base/main.go` has `--publicDir` and `--indexFallback`. `kiosk` must not re-implement static hosting.
7. **Size.** Edge is 25.5 MiB against a 28 MiB budget, so there is about 2.5 MiB of headroom. The plan targets at most +0.6 MiB for all four modules. That needs no new heavy dependency (section 5).

## 1. Cross-cutting decisions

| Topic | Decision |
| --- | --- |
| Packages | `modules/kiosk`, `modules/printer`, `modules/scanner`, `modules/devicecert`, plus shared `internal/devio` (serial/evdev/tcp I/O) and `internal/escpos` (byte builder). |
| Tags | `no_kiosk`, `no_printer`, `no_scanner`, `no_devicecert`. All four are compiled into `solo`, `team`, `cluster` and `edge`. Add all four to the `nano` line in `profiles.txt`: nano is the mobile/embed profile with no local hardware or LAN server. |
| Runtime switch | Opt-in by env, default off: `TOKI_KIOSK`, `TOKI_PRINTER`, `TOKI_SCANNER`, `TOKI_DEVICECERT` (`on`, or `off`/unset). Collections and routes exist only when `on`. The module markers list these envs, so the stubbed-module boot guard keeps working. |
| Collections | System collections (`_` prefix, `System=true`), all rules `null` (superuser only), as with `_webhooks`. Authenticated service actors use dedicated routes, not collection rules. |
| Sync | Config collections (`_printers`, `_print_templates`, `_scanners`, `_kiosk_devices`) are pull-only via `_sync_policies` so the hub provisions fleets. Runtime collections (`_print_jobs`, `_scan_events`) are local-only, never synced. Reason: a hub row applied on the spoke must never trigger physical printing. Enqueue happens only from local request or hook paths and skips `IsSyncReplica`, as webhooks and wasm already do. |
| Actor model | Kiosk, print and scan routes run as the node's service actor (SYNC_DESIGN 1.6 point 6). That is a low-privilege auth record such as `gate_devices/abc`. Never a superuser. |
| Audit | Reuse the existing `SetAuditSink` pattern. Actions: `print.job`, `print.dead`, `kiosk.pair`, `kiosk.unlock_fail`, `devicecert.issue|revoke`. |
| Health | `apis.SetHealthExtra(app, "printer", ...)` and similar for superuser-only status. |
| Kernel additions | Small interfaces in `kernel/`, written in PR 0 (section 9). They let modules reach sync data without importing it. |

## 2. `printer`

**Purpose.** Reliable, offline-tolerant receipt, ticket and QR printing from an edge box to ESC/POS printers. Print jobs are durable and retried.

**Non-goals.**
- No CUPS, PDF or generic OS printing.
- No ZPL/TSPL label languages. Raw passthrough exists for them.
- No fiscal printers.
- No Windows spooler.
- No Bluetooth stack (use a pre-bound `/dev/rfcomm0`).
- No receipt layouts for any business. Layouts are templates owned by the app.

### Data model
- **`_printers`:** `name` (unique), `transport` (`tcp`|`serial`|`file`), `address` (`10.0.0.50:9100`, `/dev/ttyUSB0`, `/dev/usb/lp0`), `baud`, `cols` (32/42/48), `codepage`, `cut`, `drawer`, `qr_native` (bool), `enabled`, `default`, `timeout_ms`.
- **`_print_templates`:** `name`, `version` (int, bumped on save), `body` (DSL below), `printer` (optional default).
- **`_print_jobs`:** the business record, separate from `_jobs`, because `_jobs` rows are purged after 7 days and the app needs status. Fields: `printer`, `template`, `template_version`, `payload` (rendered ESC/POS bytes, base64, at most 64 KB), `state` (`queued`|`printing`|`waiting_paper`|`done`|`failed`|`dead`), `attempts`, `last_error`, `job_id`, `idempotency_key` (unique, nullable), `actor`, `copies`, `printed_at`, `created`.
  - Rendering happens at enqueue, so a later template edit never changes a reprint.
  - The kernel job `print.send` carries only `{"id": ...}`.
  - A daily cron prunes `done` rows older than `TOKI_PRINT_RETENTION_DAYS` (default 14).

### Template DSL
Go `text/template` (stdlib, no new dependency) with a restricted `FuncMap`: `pad`, `money`, `date`, `upper`. Lines starting with `@` are directives:
```
@center
@size 2 2
PARKIR SIMPANG
@size 1 1
@left
Plat: {{.plate}}
Masuk: {{date .in "02 Jan 15:04"}}
@qr {{.ticket_id}}        # GS ( k native; raster fallback
@barcode code128 {{.ticket_id}}
@feed 3
@cut
@drawer
```
Data size and recursion limits apply. There is no I/O function and no `call`.

### Pipeline
1. `POST /api/print` validates auth, the template and the data.
2. It renders bytes with `internal/escpos`.
3. It inserts the `_print_jobs` row and calls `kernel.Jobs(app).Enqueue("print.send", {id}, Unique("print:"+idempotency_key|id), MaxAttempts(20))`.
4. The handler takes a per-printer mutex (so one printer prints one job at a time even with 4 workers), dials, writes the payload, and runs the status check.

**Status and retry.**
- The check sends `DLE EOT 1/2/4` real-time status commands to TCP and serial printers. Offline, cover open, paper end and error bits are read from the reply. `file` transport has no status.
- Network, timeout and offline errors return `error`, so the job is retried by `jobs` backoff.
- Paper-out or cover-open sets the state to `waiting_paper` and re-enqueues the same job with `Delay(10s)` plus the same `Unique` key, then returns nil. This keeps polling every 10 s until paper is loaded and does not burn attempts or hit the 1 h backoff cap.
- Handlers are idempotent per job. A crash after a partial write can duplicate a receipt. This is documented and unavoidable with ESC/POS. `copies` and the `Unique` key stop double enqueue, not double paper.

### Routes
- `POST /api/print`, body `{printer?, template, data, copies?, idempotency_key?}`. Returns `{id, state}`. Auth: `apis.RequireAuth()`. `TOKI_PRINT_AUTH=auth|superuser` (default `auth`). Raw bytes (`{raw_b64}`) are allowed for superusers only.
- `GET /api/print/{id}`: job status.
- `POST /api/print/{id}/retry`: reprint a `failed` or `dead` job.
- `GET /api/print/printers`: name, transport, last-known status, queue depth (no addresses for non-superusers).
- Apply the rate-limit tag `print`.

### Env
- `TOKI_PRINTER=on`
- `TOKI_PRINT_AUTH`
- `TOKI_PRINT_ALLOW_CIDRS` (default `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8`)
- `TOKI_PRINT_RETENTION_DAYS`
- `TOKI_PRINT_MAX_BYTES`

### CLI
```
toki print printers [--json]
toki print test <printer> [--qr]     # direct, bypasses queue, prints a self-test + status
toki print send <printer> <template> --data '{...}'
toki print jobs [--state ...] ; toki print retry <id>
toki print status <printer>
```

### Tests
- **Unit:**
  - `escpos` golden bytes for each directive, codepage mapping, and QR/barcode commands.
  - Template render limits.
  - A fake `io.ReadWriteCloser` with a scriptable `DLE EOT` reply: paper-out gives `waiting_paper`, then done after the reply changes.
  - A per-printer mutex race test.
  - CIDR policy allow and deny.
  - Idempotency key dedupe.
- **Integration:** `net.Listen("tcp", "127.0.0.1:0")` stub printer. It records bytes, can drop mid-write, and can be slow, so retry and dead-letter run against the real `modules/jobs`.
- **e2e:** `tests/e2e/printer.sh` boots the binary with a TCP stub on a throwaway port, posts `/api/print`, and asserts the captured bytes contain the QR payload and the cut command.

## 3. `scanner`

**Purpose.** Turn barcode and QR reads into normalized, de-duplicated scan events that the app consumes by realtime or REST. This covers browser keyboard-wedge, serial and Linux HID (evdev) scanners.

**Non-goals.**
- No camera or computer-vision decoding. That belongs in the app (WebView/Flutter) and posts to `/api/scan`.
- No ticket or plate semantics and no gate logic (an app or WASM rule on a hub).
- No GPIO or barrier control.
- No wireless pairing.

### Data model
- **`_scanners`:**
  - `name`
  - `kind` (`serial`|`evdev`|`web`)
  - `device` (`/dev/ttyACM0`, `/dev/input/by-id/...-event-kbd`)
  - `baud`
  - `terminator`
  - `prefix` and `suffix` (stripped)
  - `min_len`, `max_len`
  - `charset` (regex)
  - `dedupe_ms` (default 1500)
  - `grab` (bool, EVIOCGRAB)
  - `enabled`
  - `layout` (`us`)
- **`_scan_events`:** `scanner`, `code`, `symbology` (best-effort: `qr`|`code128`|`ean13`|`unknown`), `source` (`serial`|`evdev`|`web`), `actor`, `dup_count`, `created`. Local only. A cron prunes rows older than `TOKI_SCAN_RETENTION_HOURS` (default 168).

### Behavior
- **Reader goroutines:** one per enabled `_scanners` row, started on `OnServe`, with reconnect and backoff when the device vanishes (USB unplug). Reader state goes to `apis.SetHealthExtra("scanner", ...)`.
- **Serial reader:** `bufio` split on `terminator` (CR, LF or CRLF), with a hard line cap.
- **evdev reader (Linux):**
  - Reads `input_event` structs. The struct size depends on arch because it embeds `syscall.Timeval`: 16 bytes on 32-bit ARM, 24 bytes on 64-bit. Compute it from `unsafe.Sizeof`, never hard-code.
  - Maps key-down scancodes to ASCII through a built-in US table (~100 entries), with shift state, and ends the code on `KEY_ENTER`.
  - Uses `EVIOCGRAB` to stop the scanner typing into a focused browser or terminal.
  - The process user needs the `input` group. Document the udev rule.
- **Web wedge:** a 40-line JS module in the kiosk bundle (section 4) captures bursts with inter-key gap at most 35 ms, at least `min_len` characters, ending in Enter, so ordinary typing is not captured. It posts to `POST /api/scan`.
- **Dedupe:** an in-memory LRU keyed by `scanner|code` with a `dedupe_ms` window. A duplicate increments `dup_count` on the existing event and emits nothing. A per-request `client_seq` also makes the web path idempotent across retries.
- **Delivery:**
  - Custom realtime topic `@scan` via `app.SubscriptionsBroker()`, the same pattern as `@sync` in `modules/sync/notify.go`. Payload `{id, scanner, code, symbology, ts}`. Unlike `@sync` this carries data, so deliver only to clients with an auth record, and make that a documented `TOKI_SCAN_TOPIC_AUTH=auth|superuser` switch.
  - `GET /api/scan/events?since=<id>` for catch-up after an SSE gap.

### Routes
- `POST /api/scan` `{scanner?, code, client_seq?}` (RequireAuth, rate-limit tag `scan`).
- `GET /api/scan/events`.
- `GET /api/scan/scanners` (status).

### Env
- `TOKI_SCANNER=on`
- `TOKI_SCAN_RETENTION_HOURS`
- `TOKI_SCAN_TOPIC_AUTH`

### CLI
```
toki scan list
toki scan listen <scanner|--device path>   # prints raw codes for setup
toki scan simulate <code>                  # inject for tests
toki scan devices                          # list /dev/input/by-id and /dev/serial/by-id
```

### Tests
- **Unit:** evdev byte fixtures fed through an `io.Reader` (both 16- and 24-byte layouts, shift/caps, grab off), the serial line splitter with a pipe, dedupe window with an injected clock, and the charset and length filters.
- **Integration:** `POST /api/scan` yields one event for a duplicate within the window and two outside it, plus an SSE `@scan` subscriber.
- **e2e:** pty pair as a "serial scanner".

## 4. `kiosk`

**Purpose.** Make a Pi or mini PC boot into a locked-down browser that talks to the local edge with no passwords, shows connectivity state, and can be PIN-locked.

**Non-goals.**
- No window manager, Chromium launcher or kiosk OS image. These go in `docs/` as a systemd unit and `chromium --kiosk` flags, not in the binary.
- No UI framework. The app's UI is served through the existing `--publicDir`.
- The PIN is not a security boundary against someone with network access or a shell. It is a UI gate against a walk-up customer, and see the unlock flow below for what it actually protects.

### Data model
- **`_kiosk_devices`:**
  - `name`
  - `token_hash` (SHA-256 of a 32-byte random device token)
  - `pairing_hash` and `pairing_expires` (one-time)
  - `auth_collection` and `auth_record` (the service actor, required)
  - `node_id` (binds to the sync node when present)
  - `bind_ip` (optional, e.g. `127.0.0.1`)
  - `pin_hash` (bcrypt via the already linked `x/crypto`)
  - `locked` (bool)
  - `ttl_hours` (session lifetime, default 12)
  - `last_seen`

### Flow
1. `toki kiosk provision --name gate-1 --actor gate_devices/abc [--pin 1234]` writes the row and prints a one-time pairing URL: `http://127.0.0.1:8090/kiosk/pair#<code>`. (Fragment, so it never reaches logs.)
2. The kiosk browser opens that URL once. `kiosk.js` posts the code to `POST /api/kiosk/pair`. The server answers `Set-Cookie: toki_kiosk=<device token>; HttpOnly; SameSite=Strict; Path=/api/kiosk`, and `Secure` when the request is HTTPS. The pairing code is single-use.
3. `POST /api/kiosk/session` is authenticated by the device cookie. It checks the token hash (constant time), `bind_ip`, and that the actor still exists. It returns a normal PocketBase auth token for the service actor with `ttl_hours` lifetime (`record.NewAuthToken`, so all rules, `sessions`, `fieldperm` and sync actor capture behave as usual). With sync on, the token also works offline because it is a local token.
4. `kiosk.js` is `go:embed`ded (about 6 KB, vanilla) and served at `/kiosk/kiosk.js`. It does the following:
   - Fetches the session and refreshes at 80% of the TTL.
   - Exposes `window.toki.token` to the app and writes the token into the `pb_auth`-compatible `localStorage` key, so the JS SDK works unchanged.
   - Draws the offline indicator.
   - Runs the scan-wedge capture (section 3).
   - Draws the PIN lock overlay.
5. **Offline indicator.** `GET /api/kiosk/status` is polled every 5 s and returns `{edge:true, hub:bool, pending_changes:N, last_sync, printers:[{name,state}], time}`. `hub` and `pending_changes` come from a small kernel interface, `kernel.SyncStatus(app)`, that sync registers. If sync is absent, those fields are omitted. The indicator shows three states: edge down (fetch failed), hub offline with a count of pending changes, and online.
6. **Lock and PIN.**
   - `POST /api/kiosk/lock` revokes the session on the server (through the `sessions` sid revoke when present) and clears client state.
   - `POST /api/kiosk/unlock {pin}` checks bcrypt, rate-limited in memory (5 failures lock out for 60 s, doubling), then issues a fresh session.
   - Because the token is gone while locked, a locked kiosk cannot call the API at all. The lock is real for the browser session, not cosmetic.
   - Idle auto-lock is `lock_after_s` in config.

### Routes
`/kiosk/kiosk.js`, `/api/kiosk/pair`, `/api/kiosk/session`, `/api/kiosk/status`, `/api/kiosk/lock`, `/api/kiosk/unlock`.

### Env
- `TOKI_KIOSK=on`
- `TOKI_KIOSK_ALLOW_REMOTE` (default off, so `pair` only works from a loopback `RemoteAddr` unless set)
- `TOKI_KIOSK_SESSION_HOURS`

### CLI
```
toki kiosk provision | list | rotate <name> | revoke <name> | set-pin <name>
```

### Tests
- **Unit:** token hashing, pairing single-use and expiry, `bind_ip`, PIN backoff with an injected clock, and a revoked actor.
- **Integration:** pair, session, SDK-style call with the token, lock then call (expect 401), unlock then call (expect 200), `status` with and without a fake `SyncStatus`.
- **JS:** the file has no build step. Add a small `node --test` check if node is present in CI. Otherwise cover it by the e2e curl script.

## 5. Dependencies and size

- **Serial.** Reject `go.bug.st/serial`: its port enumeration is cgo or platform-specific, and it brings Windows and macOS code the edge never uses. Instead, `internal/devio/serial_linux.go` is a ~120-line termios wrapper using `golang.org/x/sys/unix`, which is already in `go.mod` and linked. It does `IoctlGetTermios` and `IoctlSetTermios` with raw mode, the baud constant table, 8N1 and a read timeout via deadline. `serial_other.go` returns `ErrUnsupported`. Windows COM is not supported in v1 (edge targets are Linux Pi and mini PCs).
- **USB printers.** Use the kernel `usblp` driver and write to `/dev/usb/lp0` as a plain file. This needs no libusb and no cgo. USB-serial adapters use `/dev/ttyUSB*`. Raw libusb is out of scope because it needs cgo.
- **QR.** Prefer the printer's native QR (`GS ( k`, zero dependencies). For printers without it, a raster fallback needs a QR encoder. `github.com/skip2/go-qrcode` is already an indirect dependency in `go.mod`. Before using it directly, check with `go mod why` where it comes from and measure the delta with `make edge`. If it is not worth it, write a small QR encoder (about 300 lines) under `internal/escpos`. Do not add a new third-party module.
- **Codepages.** Hand-written tables for CP437, CP858 and WPC1252 (about 128 entries each) instead of `golang.org/x/text/encoding/charmap`. Check first whether charmap is already linked. If it is, use it.
- **X.509 and TLS.** Everything is in the stdlib (`crypto/x509`, `crypto/ecdsa`, `crypto/tls`), which is already linked by `net/http`.
- **Budget estimate.** printer about 180 KB, scanner about 70 KB, kiosk about 60 KB including the JS, devicecert about 120 KB, `internal/*` about 40 KB. Total roughly +0.5 MiB. Edge goes from 25.5 MiB to about 26.0 MiB, under 28. The `profiles.txt` budget stays 28. Each PR records the measured size delta in `docs/PROFILES.md`.

## 6. `device-cert` (the remaining part)

**Purpose.** Give edge-attached devices and the edge itself real X.509 identities, so LAN traffic is encrypted and mutually authenticated, and the app can verify which device it talks to. The existing JWS node cert stays the hub-to-spoke credential and is not replaced.

**Non-goals.**
- No public ACME or Let's Encrypt on the edge (no stable DNS, often offline). Upstream `--https` autocert is unchanged.
- No SCEP, EST or OCSP.
- No browser client certificates for the kiosk (distribution is painful). The kiosk uses the device token from section 4.
- No replacing the sync handshake.

### Design
- **Hub CA.**
  - One ECDSA P-256 CA (not Ed25519: Chromium's support for Ed25519 certificates in TLS is not something to depend on for the kiosk browser). 10-year self-signed root.
  - The private key is stored in `_devicecert_state` (plain table in data.db), AES-GCM wrapped under a key derived by HKDF from the hub sync key (info `toki_devicecert/ca/v1`). A copy of `data.db` without the hub key therefore reveals nothing, consistent with how `deriveSessionSecret` treats the session secret. It follows the hub failover for free, because the hub key does.
  - `toki devicecert ca` prints the root PEM and fingerprint, for installing in the Pi's browser trust store, tablets and so on.
- **Edge server leaf (fixes G2).**
  - The spoke generates a P-256 key locally in `<dataDir>/devicecert_leaf.key` (0600). The hub never sees it.
  - It asks the hub (sync session token auth) via `POST /api/sync/devcert {spki, sans}`. The hub issues a 14-day leaf with `ExtKeyUsage` serverAuth and clientAuth and SANs for the node DNS name `<node_id>.edge.toki.local` plus the reported LAN IPs. It also records the serial in `_device_certs`.
  - The hub endpoint and the spoke renewal call live in `modules/sync` (new `devcert.go`, under the existing `no_sync` tag), because only sync owns the session auth and client. Sync reaches the devicecert module through `kernel.DeviceCerts(app)` (register/lookup provider, section 9), keeping modules independent.
  - Renewal runs from the sync loop after a handshake when the leaf has less than 1/3 of its life left, so a node needs to reach the hub at least once per 14 days. A node offline longer keeps serving with the expired leaf rejected by new clients. Mitigation: `TOKI_DEVICECERT_LEAF_DAYS` up to 90.
- **Edge TLS listener.**
  - `modules/devicecert` starts a second `http.Server` on `TOKI_DEVICECERT_LISTEN` (e.g. `:8443`) on `OnServe`. Its handler is the same router, and `tls.Config.GetCertificate` hot-reloads the leaf. `apis/serve.go` does not need changes, so there is no PocketBase-core fork risk.
  - The existing `tlscheck` warning is suppressed when this listener is on.
- **Client certs for LAN peers (fixes G3).**
  - v1: `toki devicecert issue --name gate-ctrl-1 --days 90` runs on the hub and writes PEM key+cert (and a `.p12` option) for installation on the peer. Rows go in `_device_certs` (`serial`, `name`, `kind` client|server, `node`, `not_after`, `revoked_at`, `route_scope`).
  - The edge verifies with `ClientAuth: VerifyClientCertIfGiven` against the CA pool and checks the serial against a local deny list. The deny list comes from `_device_certs`, pulled to edges with a sync pull-only policy (an online edge learns revocations; an offline one relies on the short validity).
  - A valid client cert grants a route allowlist only (`/api/scan`, `/api/print`, `/api/kiosk/status`, per `route_scope`) with no token needed. It does not make the caller a user or the service actor. Mapping a cert to an actor is v1.1.
  - v1.1: the hub issues an intermediate CA to the edge (pathlen 0, name constraints), so an offline edge can issue peer certificates itself.
- **Signed identity for the app (fixes G4).**
  - `GET /api/device/identity` returns `{node_id, hub_id, cert (JWS), leaf_fp}`.
  - `POST /api/device/attest {nonce}` returns `Ed25519(node_key, "toki-attest/v1|node|nonce|ts")` plus the cert. The app verifies the JWS against the hub public key it already trusts (the same primitive as `proto.VerifyCert`), so it knows which enrolled device answered. It needs `kernel.NodeIdentity` (section 9). No key material leaves the process.
- **Rotation and revocation (fixes G5).**
  - Leaves are short-lived.
  - `toki devicecert rotate-ca` adds a new CA while keeping the old one trusted for an overlap (both are in the pool, and the newest signs). Peers get the new root through an install step; document it.
  - `toki devicecert revoke <serial|name>` marks the row, which sync pulls to edges. It also revokes the node leaf when the node is revoked (`toki sync revoke` calls the provider).

### Data and env
- Tables: `_devicecert_state` (CA key blob, CA PEMs, epoch) and the `_device_certs` collection (superuser only).
- Env: `TOKI_DEVICECERT=on|off`, `TOKI_DEVICECERT_LISTEN`, `TOKI_DEVICECERT_MTLS=off|optional|require`, `TOKI_DEVICECERT_LEAF_DAYS` (default 14), `TOKI_DEVICECERT_SANS`.
- **CLI:**
```
toki devicecert ca | issue | list | revoke | rotate-ca | status
```

### Tests
- **Unit:**
  - CA issue and verify with Go's `x509.Verify`.
  - Name constraints.
  - Expired leaf, revoked serial, and wrong EKU rejected.
  - The wrapped CA key does not decrypt without the hub key.
- **Integration:** an `httptest` TLS server with `VerifyClientCertIfGiven`. A cert-scoped route allows `/api/scan` but denies `/api/collections/*`.
- **e2e:** hub plus spoke in `tests/e2e/sync.sh` style. The spoke fetches its leaf, `curl --cacert` succeeds against `:8443`, and a revoked client cert is refused after a pull.

## 7. What should NOT be in the kernel/binary

| Concern | Where it belongs |
| --- | --- |
| Barrier/gate relay, GPIO, loop detectors | App or a tiny sidecar (`toki-gpio`). The binary must not own electrical I/O. |
| Parking logic (ticket format, tariff, plate matching) and receipt layout | App templates and a hub-side WASM or JS hook. Not in edge: edge has `no_wasm`. |
| ZPL/TSPL label printers, CUPS, PDF | Out of scope. Raw passthrough exists. A vendor-specific `escpos` quirk table can grow later, in `internal/escpos`. |
| Payment terminals (EDC/QRIS), fiscal devices | App and vendor SDKs. |
| Camera, ANPR/LPR, QR decoding from images | App (Flutter/WebView) or a sidecar. It posts the decoded string to `/api/scan`. |
| Browser launcher and OS kiosk lockdown | Ops docs (systemd unit, Chromium flags), not Go. |
| WASM-driven printer/scanner drivers | Not feasible. WASI has no device host API, and edge ships without the runtime. A hub-side or solo WASM module can transform data before it is enqueued. Hardware access stays native. |

## 8. Docs
- New: `docs/modules/{kiosk,printer,scanner,devicecert}.md`, plus `docs/EDGE_GATE.md`. That doc is a walk-through for a parking gate or POS: Pi setup, a udev rule for scanners and `/dev/usb/lp0`, a systemd unit running `toki serve --publicDir /opt/app`, a Chromium kiosk unit, enrollment, and a TLS trust-store install.
- Update `docs/PROFILES.md` (the modules table and four new tags), `profiles.txt` (nano line), `docs/ARCHITECTURE.md` (a Phase 3 PR line for edge modules), `docs/COMPAT.md` (all routes are additive under `/api/{print,scan,kiosk,device}`, no change to the PocketBase contract), and `docs/modules/sync.md` (the `/api/sync/devcert` endpoint).
- Per module: tests wired into `profiles_test.go` through `profiles.txt`, so every single `no_x` tag builds and vets automatically.

## 9. PR split (each at most 1 day for a Sonnet agent)

Order matters. PRs 1 to 3 have no hardware dependency and can be built and tested in CI.

| # | PR | Content | Depends on |
| --- | --- | --- | --- |
| 0 (done) | `kernel: edge provider interfaces` | `kernel/nodeident.go` (`NodeIdentity`: node id, hub id, hub pub, cert, `Sign(msg)`), `kernel/syncstatus.go` (`SyncStatus`: hub reachable, pending count, last sync), `kernel/devicecerts.go` (provider registration). Sync implements and registers them. No behavior change, unit tests only. | none |
| 1 (done) | `internal/devio` + `internal/escpos` | Linux serial and evdev readers, TCP/file dialer with the CIDR policy, ESC/POS builder, codepages, QR (native plus raster). Pure unit tests with fakes. | none |
| 2 (done) | `printer` PR1 | Collections, template DSL, `print.send` handler, status/paper-out logic, `/api/print*`, CLI, marker/stub, `no_printer`, docs, TCP-stub integration test. | 1 |
| 3 (done) | `scanner` PR1 | `_scanners`/`_scan_events`, serial and web ingestion, dedupe, `@scan` topic, `/api/scan*`, CLI, `no_scanner`, docs. | 1 |
| 4 (done) | `kiosk` PR1 | `_kiosk_devices`, pair/session/status/lock/unlock, embedded `kiosk.js` (indicator, wedge capture, lock overlay), CLI, `no_kiosk`, docs. Wires in the scanner wedge hook if PR 3 is merged. | 0, optionally 3 |
| 5 (evdev reader and `devices` done in PR 3) | `scanner` PR2 | evdev reader with `EVIOCGRAB`, `toki scan devices`, 32/64-bit struct tests, udev docs. | 3 |
| 6 (done) | `devicecert` PR1 | Hub CA with wrapped key, `_device_certs`, `/api/sync/devcert` and spoke renewal in `modules/sync`, leaf key, `:8443` listener, `toki devicecert ca|status|list`, `no_devicecert`. | 0 |
| 7 | `devicecert` PR2 | Client-cert issue/revoke, mTLS route allowlist, deny-list sync policy, `/api/device/identity` and `/api/device/attest`, rotate-ca. | 6 |
| 8 | `edge` integration | `profiles.txt` (nano tags), size measurement and `docs/PROFILES.md`, `docs/EDGE_GATE.md`, parking e2e (`tests/e2e/edge-gate.sh`: hub, spoke, TCP printer stub, pty scanner, kiosk pair, 48 h offline compressed with `TOKI_SYNC_TEST_CLOCK_OFFSET`). Feeds SYNC_DESIGN PR10. | 2 to 7 |

### Status of PR 0 and PR 1 (done)

- **PR 0.** `kernel/nodeident.go` (`NodeIdentity`, `SetNodeIdentity`, `NodeIdentityOf`), `kernel/syncstatus.go` (`SyncStatus`, `SetSyncStatusProvider`, `SyncStatusOf`; the plan's `kernel.SyncStatus(app)` is spelled `SyncStatusOf` because the struct owns the name), `kernel/devicecerts.go` (`DeviceCertProvider` with `Issue`/`Lookup`/`Revoke`, `SetDeviceCerts`, `DeviceCertsOf`). Registries are per app, nil safe, and dropped by `ReleaseEdgeProviders`. `modules/sync/providers.go` registers identity and status at `Register` (one added line in `RegisterRole`); under `no_sync` nothing registers, so `NodeIdentityOf` returns nil. Nothing implements `DeviceCertProvider` yet (PR 6).
- **PR 1.** `internal/devio`: Linux serial (termios, raw 8N1, bauds 1200 to 230400, deadlines), evdev `KeyScanner` (16 and 24 byte `input_event`, US keymap, shift and caps, Enter terminator, `EVIOCGRAB`), `LineReader`, and a `Dialer` with the CIDR `Policy`. `internal/escpos`: `Builder`, codepages CP437/CP858/WPC1252 (hand tables; `x/text/encoding/charmap` is not linked into edge), `DLE EOT` status parsing, `Render` for the template DSL with limits. Decisions: QR raster uses `github.com/skip2/go-qrcode`, which edge already links through `modules/totp`, so it costs no bytes; no encoder was written. The `@qr` directive is `@qr [size=N] [ec=L|M|Q|H] DATA`. A directive comment needs two spaces or a tab before `#`.

### Status of PR 2 (done)

`modules/printer` as specified in section 2, with these decisions:
- The paper-out re-enqueue cannot reuse the running job's `Unique` key (the unique index covers `running` rows, so `Enqueue` would return the running job itself and the polling would stop). It uses `print:<id>:w<n>` with a counter stored in `_print_jobs.waits`.
- Status is checked before the write (paper end, cover open, mechanical error give `waiting_paper` without sending anything) and once after it (paper end stop or an error stop give `waiting_paper`; a closed connection is an error and the queue retries). A printer that does not answer `DLE EOT` within `timeout_ms` prints anyway.
- `modules/sync` excludes every system collection from `_sync_policies` (`eligible()`), so the planned pull-only provisioning of `_printers` and `_print_templates` is not possible yet; `_print_jobs` is safe by that same rule and by an explicit `IsSyncReplica` guard. Admitting these two collections is a sync follow-up.
- `_print_jobs.waits` and `updated` are extra fields. `cut`/`drawer` on a printer are defaults appended to rendered jobs that lack `@cut`/`@drawer`. Templates see the paper width as `{{._cols}}`.

### Status of PR 3 (done)

`modules/scanner`: `_scanners`/`_scan_events` (raw SQL writes, no hooks, local only), serial and evdev readers over `internal/devio` with reconnect backoff and health block `scanner`, web wedge (`POST /api/scan`, `/scan/wedge.js`), dedupe LRU plus `client_seq`, `@scan` topic restricted to authenticated clients (`TOKI_SCAN_TOPIC_AUTH`), `GET /api/scan/events` and `/api/scan/scanners`, `toki scan list|listen|simulate|devices`, `no_scanner` (nano), `tests/e2e/scanner.sh`, docs in `docs/modules/scanner.md`. The evdev reader and `toki scan devices` landed here, so PR 5 only has what is left (real-device checks). Decisions: the dedupe window starts at the first read and is not extended; `terminator` is validated but all of CR/LF/CRLF end a scan in v1; a non-positive `dedupe_ms` of 0 means the default and a negative value disables it.

### Status of PR 4 (done)

`modules/kiosk` as specified in section 4, with these decisions:
- Two small seams were added so that modules do not import each other: `kernel.RevokeSession` and `kernel.RevokeUserSessions` (set by `modules/sessions`), and `apis.HealthExtra(app, name)` to read the `printer` health block for the status route.
- The session is `NewStaticAuthToken(ttl_hours)`, a non-refreshable token, so `kiosk.js` fetches a new one at 80% of the TTL. Without the `sessions` module the TTL is capped at 10 minutes (a token cannot be revoked then).
- `lock` needs a PIN (409 otherwise); the lock flag is stored in `_kiosk_devices.locked`, so a restart keeps a locked device locked. The PIN brake is in memory.
- The wedge hook is wired: `kiosk.js` loads `/scan/wedge.js` and starts it with the session token. The SDK key is `pocketbase_auth` (the SDK default); `pb_auth` is written as well.
- `GET /kiosk/pair` is a minimal page so the pairing URL works in any browser; `toki kiosk` is registered only when `TOKI_KIOSK=on` (as `print`).
- e2e: `tests/e2e/kiosk.sh` (CI job `e2e-kiosk`, which also runs `node --test modules/kiosk/kiosk.test.js`).

### Status of PR 6 (done)

`modules/devicecert` PR1 as in section 6, with these decisions:
- The wrapping key of the CA is `HKDF(Ed25519 signature of "toki_devicecert/ca/v1" by the hub key)`, not the raw hub seed: modules must not import sync, and `kernel.NodeIdentity` only exposes `Sign`. Ed25519 signatures are deterministic, so it is stable across restarts and needs the hub key. The wrapped blob is bound to the root PEM (GCM additional data).
- The hub is its own edge: it issues its own leaf locally (same files in its data dir), so a solo hub can serve the TLS listener.
- The spoke renewal is not hooked inside the sync loop (PR 8 touches `client/session.go` and `loop.go`). `modules/sync/devcert.go` runs a separate 10 s poller that asks `kernel.EdgeLeafOf(app)` (new `EdgeLeafProvider`: `LeafRequest`, `InstallLeaf`) and fetches through `Client.DevCert`, which handshakes first when needed. A new LAN address also triggers a renewal (once per distinct set).
- `kernel.DeviceCert` gained `CAPEM`. The hub filters the SANs a node asks for (`.local`, `.lan`, `.home.arpa`, `.internal`, never `.edge.toki.local`, at most 12) and always adds the node's own name.
- `TOKI_DEVICECERT_MTLS` is wired (`VerifyClientCertIfGiven` or `RequireAndVerifyClientCert` against the CA pool plus a deny list read from `_device_certs`); a verified client certificate grants no route yet (PR 7).
- `tlscheck` reads the environment (`TOKI_DEVICECERT=on` and `TOKI_DEVICECERT_LISTEN`) and the module marker to know the listener is on; the plain port stays open.

Parallelism: after PR 0 and 1, PRs 2, 3 and 6 are independent.

## 10. Risks
- **Duplicate prints** (at-least-once jobs plus non-transactional ESC/POS). Documented. The `idempotency_key` plus `Unique` key covers enqueue duplicates only.
- **Hub-synced rows must not print.** A test asserts that `_print_jobs` is never created by sync apply or pull.
- **Linux-only hardware.** darwin and windows builds compile through `*_other.go` stubs, but hardware paths return `ErrUnsupported`. CI cross-compiles all targets.
- **Layout dependence of evdev.** Only the US layout is supported in v1. Barcode scanners are usually configured as US HID.
- **Chromium and the self-signed CA.** The Pi's trust store must install the root once. Document the NSS steps for Chromium.
- **TLS plus clock.** An edge with a bad RTC will reject a leaf (`NotBefore`). The sync clock offset logic already exists. The devicecert listener should use the corrected time, or the leaf should start backdated by 1 h.

## Open questions for the product owner
1. **Print authorization.** Should every service-actor kiosk be allowed to print any template (the plan's default, `TOKI_PRINT_AUTH=auth`), or do you need per-template or per-printer ACLs (for example, only the cashier role)? The latter adds an `allow` rule field and extra tests.
2. **Peer authentication.** Is certificate-only access to `/api/scan`, `/api/print` and `/api/kiosk/status` (a route allowlist) enough for gate controllers and handhelds in v1? Or must a client certificate map to an actor with full API rights, which needs the v1.1 mapping and a stricter threat model?
3. **Offline certificate issuing.** Must an edge that has been offline for weeks be able to issue new LAN client certificates (the intermediate-CA design, v1.1)? Or is hub-side issuing before deployment acceptable, with long-lived (90 day) peer certificates?

### Critical Files for Implementation
- /Users/dodihidayatullah/tokibase/modules/sync/identity.go
- /Users/dodihidayatullah/tokibase/modules/jobs/jobs.go
- /Users/dodihidayatullah/tokibase/modules/push/push.go
- /Users/dodihidayatullah/tokibase/modules/webhooks/marker.go
- /Users/dodihidayatullah/tokibase/profiles.txt