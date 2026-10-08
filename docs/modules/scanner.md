# Module `scanner`

Barcode and QR scan ingestion for edge devices (gates, kiosks, POS): serial scanners, Linux HID scanners read through evdev, and browser keyboard-wedge capture. Package `modules/scanner`, plan in `docs/EDGE_MODULES_PLAN.md` section 3. Additive: no change to any PocketBase endpoint.

- Opt in with env `TOKI_SCANNER=on` (default off). Then the collections, the routes and the reader goroutines exist. The `toki scan` command is always present; `list`, `listen`, `devices` work without the env, `simulate` needs it.
- Build tag `no_scanner` compiles the module out (the `nano` profile does). Setting `TOKI_SCANNER=on` on such a binary is refused at boot by the stubbed-module guard.
- Non-goals: no camera decoding (the app decodes and posts to `POST /api/scan`), no gate or ticket logic, no GPIO, no wireless pairing.

## Collections

Both are system collections with `null` rules (superusers only) in `data.db`. Neither is ever synced: do not add them to `_sync_policies`. `_scanners` may be synced pull-only to provision a fleet.

`_scanners`

| Field | Notes |
| --- | --- |
| `name` | unique, `A-Z a-z 0-9 _ . -`, 1 to 64 characters |
| `kind` | `serial`, `evdev` or `web` |
| `device` | `serial`/`evdev` only: a clean path under `/dev/`, for example `/dev/serial/by-id/usb-...` or `/dev/input/by-id/...-event-kbd`. Use the stable `by-id` links |
| `baud` | serial: 1200 to 230400, default 9600 (8N1, no flow control) |
| `terminator` | `auto` (default), `cr`, `lf`, `crlf`. In v1 every one of CR, LF and CRLF ends a scan and empty lines are skipped; the value is validated and kept for forward compatibility |
| `prefix`, `suffix` | stripped when present (for example an AIM identifier `]Q1`) |
| `min_len`, `max_len` | default 1 and 512. A longer line is dropped by the reader (hard cap `max_len` + prefix + suffix + 16 bytes) |
| `charset` | optional RE2 regular expression the cleaned code must match, for example `^[A-Z0-9-]+$` (max 256 characters) |
| `dedupe_ms` | default 1500 (0 or empty = default); a negative value disables de-duplication |
| `grab` | evdev: take the device exclusively (`EVIOCGRAB`) so the digits do not also type into a focused browser or terminal |
| `enabled` | disabled scanners are not read and are refused by the API |
| `layout` | `us` (the only value) |
| `allowed_actors` | web scanners: comma list of auth collections or records (`gate_devices`, `gate_devices/abc`) that may `POST /api/scan` to this scanner; empty = `TOKI_SCAN_POST_COLLECTIONS` |

Rows are validated on save, also through the admin API. A change restarts that reader within seconds. Control characters (below 0x20) in a code reject the scan.

`_scan_events`: `scanner`, `code`, `symbology` (best effort: `ean13` for 13 digits, `qr` for a URL or more than 64 characters, an explicit hint from the caller, else `unknown`), `source` (`serial`, `evdev`, `web`, `cli`), `actor` (`collection/id` of the caller of `POST /api/scan`, empty for readers), `dup_count`, `created`, `updated`. Rows are written without record hooks (no realtime collection event, no audit entry, no sync capture). A cron job removes rows older than `TOKI_SCAN_RETENTION_HOURS` (default 168).

## De-duplication and idempotency

An in-memory LRU keyed by `scanner|code` (1024 entries). The window starts at the first scan of a code and lasts `dedupe_ms`: a repeat inside it increments `dup_count` of the existing event and emits nothing (no new row, no `@scan` message). The window is not extended by repeats, so a barcode held under the reader produces one event per window. The LRU is per process and is lost on restart.

`client_seq` makes `POST /api/scan` idempotent across retries: the same `(actor, scanner, client_seq)` within 10 minutes returns the original event with `duplicate: true` and never counts as a duplicate read.

## Readers

One goroutine per enabled `serial` or `evdev` row, started on serve. When the device disappears (USB unplug, EOF, read error) the reader reconnects with backoff from 500 ms doubling to 15 s. Status is in `GET /api/health` (superuser) under `scanner`: per scanner `state` (`connecting`, `connected`, `error`, `disabled`), `last_error`, `last_scan`, `scans`, `rejected`, `reconnects`.

- Serial: `internal/devio` termios raw mode; lines split on CR/LF/CRLF.
- evdev (Linux): reads `input_event` structs (24 bytes on 64-bit, 16 on 32-bit ARM, computed from `unsafe.Sizeof`), US keymap with shift and caps lock, the scan ends on Enter. Other platforms return "unsupported" and the reader stays in `error`.

### udev and the `input` group (evdev)

The process user needs read access to the event device. Give it the `input` group and a stable name:

```
# /etc/udev/rules.d/90-barcode.rules  (find idVendor/idProduct with `udevadm info -a -n /dev/input/eventN`)
SUBSYSTEM=="input", ATTRS{idVendor}=="05e0", ATTRS{idProduct}=="1200", MODE="0660", GROUP="input", SYMLINK+="input/barcode0"
```

```
sudo usermod -aG input toki        # then restart the service
sudo udevadm control --reload && sudo udevadm trigger
```

For a serial scanner add the user to `dialout` (Debian/Ubuntu) or `uucp`. In a systemd unit use `SupplementaryGroups=input dialout`. With `grab` on, only this process receives the keys; if the process crashes the kernel releases the grab.

## Web wedge

`GET /scan/wedge.js` (public, static, cacheable) serves a small script. It captures keyboard bursts (gap between keys at most 35 ms, at least `minLen` characters, ended by Enter, so normal typing is untouched) and posts them to `POST /api/scan` with a `client_seq` and up to 3 retries:

```html
<script src="/scan/wedge.js"></script>
<script>
  const stop = TokiScan.start({ token: () => pb.authStore.token, scanner: 'door', onScan: (r) => console.log(r.code) });
</script>
```

Options: `url` (base, default same origin), `token()`, `scanner`, `minLen` (4), `maxGap` (35), `onScan(result)`, `onError(err)`. The kiosk module bundles this hook.

## HTTP API

| Route | Auth | Notes |
| --- | --- | --- |
| `POST /api/scan` | superusers, the scanner's `allowed_actors`, else `TOKI_SCAN_POST_COLLECTIONS` (`TOKI_SCAN_POST_AUTH=auth` = every auth record); others get `403`. Rate-limit tag `scan` plus a built-in `TOKI_SCAN_RATE_PER_MIN` (default 600) per actor and per client address. The request stays in the activity log | body `{scanner?, code, client_seq?, symbology?}` (8 KiB max). `scanner` omitted = first enabled `web` scanner, else the built-in default named `web`; the built-in one exists only while no web scanner row is configured. `client_seq` is at most 128 characters. Answers `200 {id, scanner, code, symbology, source, dup_count, ts, duplicate}`. Errors: `400` with `data.code = scan_rejected` and `data.reason` (`empty`, `too_short`, `too_long`, `control_char`, `charset`), `404` unknown scanner, `409` disabled, `403` from a sync replica apply |
| `GET /api/scan/events?since=<id>&limit=<n>&scanner=<name>` | same as the topic (below) | `{items, gap}`, oldest first, `limit` 1 to 500 (default 100). Without `since` it returns the newest `limit` events. `gap: true` means `since` is not retained any more (pruned): re-read state from the items |
| `GET /api/scan/scanners` | readers (as the topic) and posting actors | status of every scanner; `device` and `last_error` only for superusers |
| `GET /scan/wedge.js` | none | the script above |

Rule: scan events are created only by the local process (readers, `POST /api/scan`, `toki scan simulate`). A request carrying a sync-replica origin is refused, so a hub row can never make an edge "scan".

## Realtime topic `@scan`

Subscribe like any realtime topic (`pb.realtime.subscribe('@scan', cb)`, or `@scan?options=...`). Unlike `@sync` the payload carries data: `{"id","scanner","code","symbology","source","actor","ts"}` (`source` = `serial`, `evdev`, `web`; `actor` = who posted a web scan, empty for readers, so a gate consumer can tell a person at the door from a forged browser scan). Delivery is only to clients allowed to read, enforced twice: the subscribe request answers `403`, and the publisher skips any client that is not allowed or whose auth record no longer exists. `TOKI_SCAN_TOPIC_AUTH=service` (default) lets superusers and `TOKI_SCAN_READ_AUTH` (collections or records) read the topic and `GET /api/scan/events`; `auth` lets every auth record read (sign-ups included); `superuser` limits both to superusers. Each client has its own ordered queue of 64 events; a client that does not read loses its oldest events and catches up with `events?since=`. After a gap in the SSE stream, call `GET /api/scan/events?since=<last id>`.

## Env

| Variable | Default | Meaning |
| --- | --- | --- |
| `TOKI_SCANNER` | off | `on` enables the module |
| `TOKI_SCAN_RETENTION_HOURS` | 168 | age after which `_scan_events` rows are removed (cron every 10 minutes) |
| `TOKI_SCAN_TOPIC_AUTH` | `service` | `service`, `auth` or `superuser` (see Realtime topic) |
| `TOKI_SCAN_READ_AUTH` | empty | collections / records that may read scans in `service` mode |
| `TOKI_SCAN_POST_AUTH` | `service` | `service`, `auth` or `superuser` for `POST /api/scan` |
| `TOKI_SCAN_POST_COLLECTIONS` | empty | collections / records that may post scans to a scanner without `allowed_actors` |
| `TOKI_SCAN_RATE_PER_MIN` | 600 | `POST /api/scan` per actor and per client address (`0` = off) |

## CLI

```
toki scan list [--json]
toki scan devices                              # /dev/input/by-id and /dev/serial/by-id
toki scan listen <scanner>                     # raw codes of a configured scanner, Ctrl-C to stop
toki scan listen --device /dev/ttyACM0 [--kind serial|evdev] [--baud 9600] [--grab]
toki scan simulate <code> [--scanner web1] [--url http://127.0.0.1:8090 --token <jwt>]
```

`listen` prints before any filter and opens the device itself: stop the server's reader of that scanner first. `simulate` without `--url` writes straight to the database (a running server keeps the row but publishes nothing, being another process); with `--url` and `--token` it calls `POST /api/scan` so `@scan` subscribers get the event.

Audit: scans are not audited; config changes follow the normal collection audit.

## Tests

Unit and integration tests in `modules/scanner` (evdev byte fixtures for the 16 and 24 byte layouts with shift and caps lock, serial splitter over a pipe, dedupe with an injected clock, filters, `POST /api/scan` dedupe, SSE `@scan` for authenticated and guest clients, reconnect after unplug). End to end: `tests/e2e/scanner.sh` uses a pty pair as a serial scanner (CI job `e2e-scanner`).
