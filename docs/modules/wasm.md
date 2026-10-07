# Module `wasm`

Sandboxed hook runtime: business logic written in any language that compiles to WASI (Go, Rust, C, Zig, AssemblyScript, TinyGo...) runs inside [wazero](https://wazero.io) (pure Go, no cgo) with wall-clock and memory limits and an explicit host API. JS `pb_hooks` (jsvm) are unchanged and keep working; WASM hooks are an additional, stricter option. Package `modules/wasm`, Go guest SDK in `modules/wasm/sdk/go`.

- Always on: `tokibase.go` calls `wasm.Register`, which adds the flags and loads `pb_hooks_wasm/`. With an empty or missing directory nothing happens. `TOKI_WASM=off` skips registration; building with `-tags no_wasm` replaces the module by a stub (no wazero in the binary, about 2.8 MB smaller).
- PR 1 scope. Not included: fuel-based CPU metering (see Limits), events for auth requests (`OnRecordAuthRequest`), collection events, a per-module egress policy beyond the global allowlist.

## Loading

```
pb_hooks_wasm/
  validate.wasm        # the module
  validate.toml        # optional sidecar (same base name)
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--wasmHooksDir` | `<dataDir>/../pb_hooks_wasm` | directory scanned for `*.wasm` |
| `--wasmHooksWatch` | off | reload modules when a `.wasm`/`.toml` in the directory changes (debounced 250 ms, in process, no restart) |

| Env | Meaning |
| --- | --- |
| `TOKI_WASM` | `off` disables the module |
| `TOKI_WASM_HTTP_ALLOW` | comma separated host patterns `http_fetch` may reach (`api.example.com`, `*.example.com`); empty = no outbound HTTP at all |
| `TOKI_WASM_ALLOW_PRIVATE` | `1` lets `http_fetch` reach loopback/private addresses (off by default, SSRF guard) |
| `TOKI_WASM_CACHE_DIR` | persist wazero's compilation cache across restarts (otherwise it lives in memory) |

The module name is the file name without `.wasm`. Modules are compiled once (wazero compilation cache, keyed by content) at start and on reload, and instantiated per call: every call gets a fresh linear memory and a fresh WASI environment, so guests cannot share state (use `kv_*` or records). Concurrent instances per module are bounded by a pool of `clamp(2*GOMAXPROCS, 4, 16)`. A module that fails to compile, imports unknown functions or has a bad sidecar is skipped with an error in the log (and in `toki wasm list`); the others keep running. After a reload in-flight calls finish on the old instance.

Routes are registered when the server starts: adding a new `route:` event needs a restart (a warning is logged); changing the code behind an existing route, or any record/cron/job event, is picked up live.

## Sidecar `<name>.toml`

All keys are optional. A module without `events` is loaded but never called (still usable with `toki wasm run`).

```toml
events = ["record.create.posts", "record.update.*", "record.after.create.posts",
          "cron:*/5 * * * *", "route:POST /api/hello", "job:welcome"]
timeout_ms   = 2000      # default 2000, max 120000
memory_pages = 256       # 64 KiB each; default 256 (16 MiB), max 16384 (64 suits small Rust/TinyGo guests)
needs = ["http", "records", "mail", "kv", "jobs"]   # host capabilities, default none
env = { REGION = "eu" }  # WASI environment (also accepted as an [env] table)
```

Only strings, integers, arrays, inline tables and a `[env]` table are supported (a small built-in parser, no dependency).

| Event | Fires |
| --- | --- |
| `record.<create\|update\|delete\|*>.<collection\|*>` | before the write (inside `OnRecordCreate/Update/Delete`, before validation): the guest may change fields or reject |
| `record.after.<action>.<collection>` | after the write succeeded, read-only, failures only logged |
| `cron:<5-field expr>` | on schedule, as a durable job when `kernel.Jobs(app)` has a queue (deduplicated per minute slot across processes, 3 attempts), otherwise inline from `app.Cron()` |
| `route:<METHOD> <path>` | custom route, registered on `OnServe` (`{name}` path params allowed) |
| `job:<name>` | job enqueued by the module itself via `jobs_enqueue` |

A wildcard collection (`*`) never matches system collections whose name starts with `_`; name them explicitly if you need them. Record hooks fire for writes through the REST API and for writes made by Go/JS code (`app.Save`), because they sit on the model hooks. Writes made by a guest through `records_save`/`records_delete` do not re-trigger WASM hooks (loop guard); other hooks still fire.

## ABI `toki/1`

A guest is a WASI preview1 **command** (exports `_start`, `memory`). Each call:

1. The host instantiates the module, writes the event JSON to **stdin**.
2. The guest reads stdin, does its work and writes one JSON **result** to stdout, then exits (return from `main` or `proc_exit(0)`).
3. stderr is captured (64 KiB) and logged when the call fails. stdout is capped at **1 MiB**.

A non-zero exit, a trap, an out-of-memory, a timeout, empty or invalid stdout all count as a failed call.

### Event (stdin)

```json
{
  "abi": "toki/1", "module": "validate", "event": "record.create.posts",
  "kind": "record", "phase": "before", "action": "create", "collection": "posts",
  "record":   {"id": "...", "title": "hello"},
  "original": {"...": "previous values, update/delete only"},
  "actor": {"kind": "auth", "id": "RECORD_ID", "collection": "users"},
  "request_info": {"method": "POST", "context": "default", "query": {}, "headers": {}, "body": {}},
  "time": "2026-10-07T12:00:00Z"
}
```

- `kind`: `record | cron | route | job`. Record events carry `record` (public export: hidden fields are omitted), `original` (except create) and `request_info` (only when the write came from an HTTP request).
- `actor.kind`: `superuser`, `auth` (with `id` and `collection`), `guest`, or `system` (cron, jobs, writes from code).
- Route events carry `route: {method, path, path_params, query, headers (lowercased), body (raw string, max 4 MiB)}`.
- Cron events carry `cron: {expr}`; job events `job: {name, payload}`.

### Result (stdout)

```json
{"ok": true, "record": {"title": "HELLO"}}
{"ok": false, "status": 400, "message": "title is required",
 "data": {"title": {"code": "validation_required", "message": "Cannot be blank."}}}
```

| Field | Used by | Meaning |
| --- | --- | --- |
| `ok` | all | `false` rejects a before-hook (default status 400) or answers a route with an error |
| `record` | before create/update | fields to change (merged with `Set`). `id`, `collectionId`, `collectionName`, `expand`, `tokenKey`, `passwordHash` are ignored; the normal validation still runs on the result |
| `status`, `message`, `data` | rejections | HTTP status 400-599, message, field errors. `data` values may be `{"code","message"}` objects or plain strings |
| `status`, `headers`, `body` | routes | response (status default 200). `body` is a JSON string = written raw, any other JSON value = encoded as JSON (`Content-Type: application/json` unless you set one) |

A guest failure never leaks internals: before-hooks and routes answer `500 {"message":"Hook failed."}` (the write is aborted), details go to the log with the module and stderr. After-hook, cron and job failures are logged (jobs are retried by the queue).

### Host functions (import module `toki`)

Every function except `log` has the signature `(req_ptr: i32, req_len: i32) -> i64`. The request is a JSON document in guest memory. The host runs it, asks the guest for a buffer by calling the exported `toki_alloc`, writes the JSON response there and returns `ptr << 32 | len` (`0` = the host could not deliver, e.g. the guest has no allocator). Responses always contain `"ok": true|false`; on failure `"error"` holds the reason.

Guests must export:

- `toki_alloc(size: i32) -> i32`: return a pointer to `size` writable bytes that stay valid until the guest frees them.
- `toki_free(ptr: i32, size: i32)`: release a buffer. The host never calls it in `toki/1` (instances are single use), but guests should export it so allocators pair up and future versions can rely on it. `toki wasm validate` warns when it is missing.

| Function | Capability | Request | Response |
| --- | --- | --- | --- |
| `log(level: i32, ptr: i32, len: i32)` | none | level 0 debug, 1 info, 2 warn, 3 error; UTF-8 text (max 64 KiB) | none |
| `records_find` | `records` | `{collection, id}` or `{collection, filter, params, sort, limit (max 500), offset}`; `filter` uses `{:name}` placeholders | `{records: [...]}` (public export) |
| `records_save` | `records` | `{collection, id?, data}`: empty `id` creates | `{record}` |
| `records_delete` | `records` | `{collection, id}` | `{}` |
| `http_fetch` | `http` | `{method, url, headers, body, timeout_ms (max 10000)}` | `{status, headers, body}` (body max 1 MiB) |
| `mail_send` | `mail` | `{to: [..max 50], subject, html, text}` from the configured sender | `{}` |
| `kv_get` | `kv` | `{key}` (max 256 bytes) | `{found, value}` |
| `kv_set` | `kv` | `{key, value (max 64 KiB), ttl_s}` | `{}` |
| `jobs_enqueue` | `jobs` | `{name, payload, delay_s, unique}`; `name` must be declared as `job:<name>` | `{id}` |

Notes:

- `records_*` run with application privileges (no API rules), like Go hooks. Grant `records` only to code you trust with the data.
- `http_fetch` needs the `http` capability **and** the host in `TOKI_WASM_HTTP_ALLOW`. Connections are checked after DNS resolution and redirects are not followed, so private, loopback, link-local, CGNAT and NAT64 ranges are blocked (same list as webhooks) unless `TOKI_WASM_ALLOW_PRIVATE=1`.
- `kv_*` is a per-module namespace in `auxiliary.db` (`_wasm_kv`); expired keys are purged every 15 s.
- A call without the capability gets `{"ok":false,"error":"capability \"http\" not granted: add it to needs in <module>.toml"}`.
- Calls nested through the host are limited to a depth of 3.

Imports other than `wasi_snapshot_preview1` and `toki.*` make the module fail to load.

### Writing a guest in another language

Implement: (1) read all of stdin, parse JSON; (2) write the result JSON to stdout; (3) export `toki_alloc`/`toki_free` and `memory`; (4) import host calls with the module name `toki` and the table above. Examples: Rust `wasm32-wasip1` with `#[no_mangle] extern "C" fn toki_alloc`, TinyGo with `//go:wasmexport`, C with `__attribute__((export_name("toki_alloc")))`. Check with `toki wasm validate`.

## Go guest SDK

Package `github.com/tokibase/tokibase/modules/wasm/sdk/go` (name `toki`, standard library only). Build with the standard toolchain, no TinyGo needed:

```go
package main

import (
	"strings"

	toki "github.com/tokibase/tokibase/modules/wasm/sdk/go"
)

func main() {
	toki.Run(func(ev *toki.Event) (*toki.Result, error) {
		title, _ := ev.Record["title"].(string)
		if strings.TrimSpace(title) == "" {
			return toki.Reject(400, "title is required", map[string]any{"title": "Cannot be blank."}), nil
		}
		return toki.Ok().Set("title", strings.ToUpper(title)), nil
	})
}
```

```
GOOS=wasip1 GOARCH=wasm go build -o pb_hooks_wasm/validate.wasm .
echo 'events = ["record.create.posts"]' > pb_hooks_wasm/validate.toml
```

The SDK exports `toki_alloc`/`toki_free` itself and wraps the host calls: `toki.RecordsFind/RecordsSave/RecordsDelete`, `toki.HTTPFetch`, `toki.MailSend`, `toki.KVGet/KVSet`, `toki.JobsEnqueue`, `toki.Log/Logf`, and for routes `toki.JSON(status, body)`. On non-WASI targets the package compiles with stubs so the pure logic can be unit tested natively. A Go guest costs roughly 4.5 MB of `.wasm` and about 5 ms per call on a laptop (runtime start dominates; TinyGo or Rust guests start in well under 1 ms).

## Limits and failure handling

| Limit | Mechanism | Result |
| --- | --- | --- |
| Wall clock (`timeout_ms`, default 2 s) | `context.WithTimeout` + wazero `WithCloseOnContextDone`: the instance is closed even inside a tight loop | failed call (`timeout`) |
| Memory (`memory_pages`) | wazero `WithMemoryLimitPages`: `memory.grow` beyond the limit fails, the guest runtime aborts | failed call (`exit`/`memory`) |
| stdout | 1 MiB | failed call (`output`) |
| Host request | 4 MiB | host call error |
| Concurrency | per-module instance pool | callers wait, bounded by the timeout |

**There is no fuel/instruction-count limit**: wazero has none. CPU use is bounded only by `timeout_ms` (wall clock), so a guest can burn one core for up to that long per call and a flood of requests can occupy all pool slots. Keep timeouts small for request-path hooks, and rate limit routes at the proxy. A module whose declared minimum memory exceeds `memory_pages` is not loaded (`compile ...: section memory: min 48 pages over limit of N pages` in the log and in `toki wasm list`). A standard-Go guest declares 48 pages and, with `encoding/json` (the SDK uses it), runs out of memory at 64 pages during start; use at least 100 pages for Go guests, keep the default 256 unless you know your toolchain.

Panics and traps become hook errors: `500 Hook failed.` for routes and before-hooks (the write does not happen), a log line otherwise. The server process is never affected. The host also recovers from panics in its own code.

## Metrics and logs

Per module: calls, errors, total time, last error, last call. Counters are kept in memory and flushed every 15 s and on shutdown into `_wasm_stats` (`auxiliary.db`), so `toki wasm stats` (a separate process) sees them. Guest `log` output and failures are written through the app logger with `wasm_module` and `event` attributes.

## CLI

```
toki wasm list [--json]       modules, events, needs, limits, load errors
toki wasm stats               calls, errors, average ms, last call, last error
toki wasm run <module> --event <name> [--payload file.json] [--commit]
toki wasm validate <file.wasm>
```

`run` invokes the module offline with the given event document (the file may hold any `EventIn` field: `record`, `actor`, `request_info`, `route`...). Without `--commit` the host calls `records_save`, `records_delete`, `kv_set`, `mail_send`, `jobs_enqueue` and non-GET `http_fetch` are suppressed and reported under `suppressed_effects`; reads and GET fetches still happen. `validate` compiles the file and checks `_start`, memory, allocator exports and imports.

## Coexistence with JS hooks

- WASM and JS hooks bind to the same kernel hooks. WASM handlers are bound first (at module registration), so for the same event a WASM before-hook runs before `pb_hooks/*.pb.js` handlers; JS sees the record as the guest left it. After-hooks run in the same order.
- A rejection from either side aborts the write.
- Both can be used at once; hot reload is independent (`--hooksWatch` restarts the process, `--wasmHooksWatch` reloads in place).
- JS `routerAdd` routes and WASM `route:` events share the router: a WASM route that collides with an existing route is skipped with a warning.
