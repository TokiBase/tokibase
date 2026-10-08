# Module `printer`

Durable, retried printing of receipts, tickets and QR codes on ESC/POS printers (TCP 9100, serial, USB `usblp` files). Package `modules/printer`, built on `internal/escpos` (byte builder, template DSL, status parsing) and `internal/devio` (dialer with CIDR policy, serial). Part of the edge modules plan (`docs/EDGE_MODULES_PLAN.md` section 2).

- Opt-in: `TOKI_PRINTER=on` (also `true`, `1`, `yes`). Off or unset: no collections, no routes, no handler, no `print` command.
- Compile out with `-tags no_printer` (the `nano` profile does). The boot guard refuses to start a `no_printer` binary when `TOKI_PRINTER` is set.
- Needs the job queue (`TOKI_JOBS` is on by default). Transmission runs inside `toki serve`; `toki print send` only queues.
- Non-goals: CUPS, PDF, ZPL/TSPL (use raw bytes), fiscal printers, Windows spooler, a Bluetooth stack (bind `/dev/rfcomm0` yourself), receipt layouts (those are app owned templates).

## Collections

System collections in `data.db`, all rules `null` (superuser only). Authenticated service actors use the `/api/print*` routes, never the collection API.

### `_printers`

| Field | Notes |
| --- | --- |
| `name` | unique |
| `transport` | `tcp` (`10.0.0.50:9100`), `serial` (`/dev/ttyUSB0`) or `file` (`/dev/usb/lp0`) |
| `address` | see transport; validated on save against `TOKI_PRINT_ALLOW_CIDRS` (IP literals) and the device path prefixes |
| `baud` | serial only, default 9600 (1200 to 230400) |
| `cols` | paper width in characters (typical 32, 42, 48; default 42). Templates see it as `{{._cols}}` |
| `codepage` | `cp437` (default), `cp858`, `wpc1252` |
| `cut` | append feed + cut to a rendered job that has no `@cut` |
| `drawer` | append the cash drawer kick to a rendered job that has no `@drawer` |
| `qr_native` | true = `GS ( k` QR; false = raster QR image (printers without native QR) |
| `enabled` | disabled printers refuse new jobs and queued ones wait (retry with backoff) |
| `default` | used when neither the request nor the template names a printer; with exactly one enabled printer it is implicit |
| `timeout_ms` | dial, read and write timeout, default 5000, clamped to 200..30000. A `file` transport write that does not return in this time (plus 1 ms per 4 bytes) is abandoned and the file is closed |
| `status_timeout_ms` | wait for the `DLE EOT` answer, default 1000, clamped to 100..10000 (separate from the write timeout) |

### `_print_templates`

`name` (unique), `version` (starts at 1, bumped on every save), `body` (template DSL below, at most 16 KiB), `printer` (optional default printer).

### `_print_jobs`

The business record. It is separate from the kernel `_jobs` rows because those are purged after 7 days and the app needs the status.

| Field | Notes |
| --- | --- |
| `printer`, `template`, `template_version` | what was printed. The template is rendered when the job is enqueued, so a later template edit never changes a reprint |
| `payload` | the rendered ESC/POS bytes, base64, at most `TOKI_PRINT_MAX_BYTES` (default 64 KiB, hard cap 1 MiB) |
| `state` | `queued`, `printing`, `waiting_paper`, `done`, `done_unconfirmed` (everything was written, then the printer reported a fault; never resent automatically, `retry` reprints it), `failed` (an attempt failed, the queue retries), `dead` (attempts exhausted, or the paper wait ran out) |
| `attempts` | real transmissions tried |
| `waits` | paper-out polls so far |
| `last_error` | last failure or the reason for `waiting_paper` |
| `job_id` | the kernel job that carries it |
| `idempotency_key` | unique when set |
| `actor` | `<collection>/<id>` of the caller, `cli` for the CLI |
| `copies` | 1 to 10 |
| `printed_at`, `created`, `updated` | |

A daily cron (03:37, only in the serving process) deletes `done`, `done_unconfirmed`, `failed` and `dead` rows not updated for `TOKI_PRINT_RETENTION_DAYS` (default 14). `queued`, `printing` and `waiting_paper` rows are kept.

## Template DSL

Go `text/template` with the functions `pad`, `padl`, `money`, `date`, `upper`. No `call`, no `template`/`define`, no I/O. Lines starting with `@` are directives:

```
@center
@size 2 2
PARKIR SIMPANG
@size 1 1
@left
Plat: {{.plate}}
Masuk: {{date .in "02 Jan 15:04"}}
@qr {{.ticket_id}}
@barcode code128 {{.ticket_id}}
@feed 3
@cut
@drawer
```

The directive list, the `@qr [size=N] [ec=L|M|Q|H] DATA` form, comments (two spaces or a tab before `#`) and the limits are those of `internal/escpos` (`Render`). `date` formats in the time zone of the box. Data limits (5000 nodes, depth 8, 4096 bytes per string) and the output limit apply; a violation answers 400 and nothing is stored. Data is never a directive: in every string of the request `data`, control characters (newlines included) become spaces and a leading `@` is printed literally, so only the template source can start a directive or a cut/drawer. A missing key prints nothing (not `<no value>`). The error of a failed render is generic for the caller and the detail goes to the server log.

## HTTP API

All routes are tagged `print` for the rate limiter (`Settings > Rate limits`, label `print`). Superusers and excluded IPs skip rate limits as everywhere. In addition `POST /api/print` and `/retry` have a built-in throttle of `TOKI_PRINT_RATE_PER_MIN` requests per minute for each actor and for each client address (default 60), and a regular actor may have at most `TOKI_PRINT_MAX_QUEUED_PER_ACTOR` unfinished jobs (`queued`, `printing`, `waiting_paper`, `failed`; default 20), else `429`.

| Route | Auth | Notes |
| --- | --- | --- |
| `POST /api/print` | superusers and the actors of `TOKI_PRINT_ALLOW_COLLECTIONS` (default). `TOKI_PRINT_AUTH=auth` lets every auth record print, `superuser` limits it to superusers. Others get `403` | body `{printer?, template, data?, copies?, idempotency_key?}` or, superusers only, `{printer?, raw_b64, copies?}`. Answers `200 {id, state}`; a repeated `idempotency_key` of the same actor answers the first job with `"duplicate": true`; keys are scoped per actor (another actor with the same key gets its own job) |
| `GET /api/print/{id}` | same | job status without the payload. Regular users see only their own jobs (others answer 404) |
| `POST /api/print/{id}/retry` | same | queues a `failed`, `dead` or `done_unconfirmed` job again (409 for any other state; audit action `print.retry`) |
| `GET /api/print/printers` | same | `name, transport, cols, default, enabled, status, queue_depth`. `address` and `baud` only for superusers; disabled printers only for superusers. `status.detail` and a job's `last_error` hold the full text (addresses, device paths) only for superusers; other callers get a fixed text (`paper out`, `cover open`, ... or a generic message) and the full error stays in the server log |

`status` is the last status this process read from the printer (`unknown`, `ok`, `paper`, `offline`).

## Transmission

The kernel job `print.send` carries only `{"id": "<print job id>"}` (`MaxAttempts` 20). The handler:

1. skips a job that is already `done` or `dead` (the queue delivers at least once);
2. takes the per-printer lock (in process), so one printer prints one job at a time with any number of workers. The wait for the lock ends when the job context ends or after 30 s; the job then fails with a retryable error and its worker is free again;
3. opens the transport under the CIDR policy (`file`/`serial`: allowed device path prefixes only);
4. TCP and serial: sends `DLE EOT 1..4` and reads the 4 status bytes within `status_timeout_ms`;
   - paper end: nothing is sent, the state becomes `waiting_paper` and a fresh `print.send` job is queued with a 10 s delay (poll until paper is loaded). This does not burn an attempt and does not follow the retry backoff. After 360 polls (1 hour) the job is `dead` and the audit action `print.stuck` is written;
   - cover open, mechanical, cutter or unrecoverable error, offline: nothing is sent, the job is `failed` and retried by the queue backoff (5 s doubling, 1 h cap), `dead` after 20 attempts;
   - no answer or an unparsable answer: the printer is assumed not to support real-time status (many cheap models), printing continues and the printer is not asked again for 10 minutes (it is probed again after that);
5. writes the payload `copies` times, then queries the status once more. A paper end stop, error stop, cover open, mechanical, cutter or unrecoverable error reported after the write sets `done_unconfirmed` (audit `print.unconfirmed`, log line): the data is in the printer buffer, which prints the rest when the fault is fixed, so the job is never resent. A connection that breaks while reading this status gives the same state. `file` transport has no status channel, and its writes are bounded by `timeout_ms`;
6. on success `state=done`, `printed_at` set. On an error the state is `failed` and the queue retries; after 20 failed attempts the state is `dead` and the audit action `print.dead` is written.

Idempotency and duplicates: ESC/POS is not transactional. A crash or a dropped connection in the middle of a write can print a receipt twice on retry. A fault reported after a completed write does not resend (`done_unconfirmed`; use `retry` to reprint on purpose). The `idempotency_key` (per actor) and the queue `Unique` key (a hash of actor and key) stop double enqueue, not double paper. The paper-out re-enqueue uses the key `print:<id>:w<n>` (a counter): the running job still owns `print:<id>`, so the "same Unique key" of the plan would only return the running job.

## Environment

| Env | Default | Meaning |
| --- | --- | --- |
| `TOKI_PRINTER` | off | `on` enables the module |
| `TOKI_PRINT_AUTH` | `service` | `service`: superusers and `TOKI_PRINT_ALLOW_COLLECTIONS` only; `auth`: every auth record, public sign-ups included (explicit opt-in, it can open the cash drawer); `superuser`: superusers only |
| `TOKI_PRINT_ALLOW_COLLECTIONS` | empty | comma list of auth collections (`gate_devices`) or single records (`gate_devices/abc`) that may print in `service` mode |
| `TOKI_PRINT_MAX_QUEUED_PER_ACTOR` | 20 | unfinished jobs per regular actor (`0` = unlimited) |
| `TOKI_PRINT_RATE_PER_MIN` | 60 | `POST /api/print` and `/retry` per actor and per client address (`0` = off) |
| `TOKI_PRINT_ALLOW_CIDRS` | `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8` | networks a TCP printer may resolve to. Link-local (including 169.254.169.254), unspecified and multicast are always denied. Device files must start with `/dev/usb/`, `/dev/lp`, `/dev/ttyUSB`, `/dev/ttyACM`, `/dev/ttyS` or `/dev/ttyAMA` |
| `TOKI_PRINT_RETENTION_DAYS` | 14 | age of `done` rows that the daily prune deletes |
| `TOKI_PRINT_MAX_BYTES` | 65536 | largest rendered or raw payload (cap 1 MiB) |

The printer address comes only from the superuser-owned `_printers` row, never from request input.

## CLI

```
toki print printers [--json]
toki print test <printer> [--qr]       # direct, bypasses the queue: self-test page + status check
toki print send <printer> <template> --data '{...}' [--copies N] [--key K]
toki print jobs [--state ...] [--limit N] [--json]
toki print retry <id>
toki print status <printer>            # DLE EOT status, decoded
```

`test` and `status` open the printer from the CLI process, so run them on the box that is wired to it.

## Audit and health

- Audit actions `print.job` (when a job is queued: printer, template, actor, copies, bytes) and `print.dead`.
- `GET /api/health` (superusers) gets a `printer` block: per printer the transport, enabled flag and last known status, and the job counts per state.

## Sync

- `_print_jobs` is never synced: system collections (names starting with `_`) are not eligible for sync capture, and the module refuses to create a job from a pull, snapshot or bundle apply (`kernel.IsSyncReplica`) both in `Enqueue` and in a model create hook (`ErrSyncReplica`). A hub row applied on a spoke can therefore never print.
- `_printers` and `_print_templates` are meant to be provisioned from the hub (pull-only), but `modules/sync` currently excludes every system collection from `_sync_policies`, so a policy row for them is rejected or ignored. Until a later sync PR admits them, provision them with the same migration or `toki` script on every box. The policy to use then is `direction=pull` for both collections and no policy at all for `_print_jobs`.

## Size

See the edge size log in `docs/PROFILES.md`.
