# Module `audit`

Append-only, hash-chained log of privileged and schema-changing actions. Package `modules/audit`. No REST contract change.

- Enabled by default: `tokibase.go` calls `audit.Register(app)` and adds the `audit` command.
- Disable with env `TOKI_AUDIT=off` (also `false`, `0`, `disabled`). Then no hooks, no table creation, no CLI command.

## Storage

Table `_audit` in `auxiliary.db` (same DB as `_logs`, via `app.AuxDB()`).

The table is created with `CREATE TABLE IF NOT EXISTS` at bootstrap (or immediately when `Register` runs on an already bootstrapped app), not via the kernel migrations list. Reason: `SystemMigrations` entries always run for every build including `TOKI_AUDIT=off`, are recorded in `_migrations`, and a `go:embed`-free module would need a side-effect import in `migrations/`. Create-if-not-exists keeps the module removable and leaves no migration trace. The `_audit` table is not part of the upstream schema, so upstream simply ignores it.

| Column | Notes |
| --- | --- |
| `id` | 15-char text pk |
| `created` | UTC `2006-01-02 15:04:05.000Z` |
| `seq` | integer, unique, gapless, starts at 1 |
| `actor_kind` | `superuser` \| `user` \| `agent` \| `system` (`agent` is emitted by `modules/mcp` for AI agent tool calls: `agent.<tool>` actions, see `docs/modules/mcp.md`) |
| `actor_id`, `actor_collection` | auth record id and collection name; empty for `system` |
| `impersonated_by` | `NULL`, or `unknown` for impersonated sessions (the static token does not carry the issuer) |
| `action` | `record.create/update/delete`, `collection.create/update/delete`, `settings.update`, `auth.impersonate`, `backup.create`, `backup.restore` |
| `collection`, `record` | target; for backups `record` is the backup name; collections import is `collection.update` with `record = "import"` |
| `before`, `after` | JSON, nullable, secrets stripped, 64 KB cap |
| `diff` | JSON `{field: {old, new}}`, updates only (record, collection, settings) |
| `request` | JSON `{method, path, ip, user_agent}` |
| `prev_hash`, `hash` | the chain |

## What is captured (phase 1)

- Record create/update/delete via the REST API when the caller is a superuser, or the session is impersonated (non-refreshable static token). Regular user writes are NOT logged (volume).
- Collection create/update/delete and import, settings update.
- Impersonation (`POST /api/collections/{c}/impersonate/{id}`, `OnRecordAuthRequest` with empty `AuthMethod`); the issued token is never stored.
- Backup create (after success) and restore (BEFORE the restore, because a successful restore restarts the process and swaps the data dir, `auxiliary.db` included; the restored DB brings its own older chain).

Not captured: writes made through Go code / `pb_hooks` / the CLI (`superuser` command) that do not go through the REST request hooks, batch requests (`/api/batch`), realtime-driven writes, and backup actor identity (kernel backup hooks have no request, so the actor is `system`).

Handlers run outermost (priority `-1<<20`): `before` is snapshotted ahead of other handlers and `after` taken after a successful `e.Next()`. A failed request writes nothing.

Redaction: password-type fields, `tokenKey`, and any key named `token`, `privateKey`, `apiKey`, `otp` or containing `password` / `secret` (recursively, also in settings and collection JSON such as SMTP password, S3 secret, OAuth2 `clientSecret`). A changed secret still shows in `diff` as `"[redacted]"`. File fields keep file names only, never contents. Each JSON column above 64 KB becomes `{"__audit_truncated__":true,"original_bytes":N,"preview":"..."}`.

## Writes and failures

Synchronous, in the same request after success. Any audit write error is logged (`audit: failed to write entry`) and never fails the request. The chain head (last `seq` + hash) is cached in memory under a mutex and re-read from the DB at bootstrap (and once on an insert conflict).

## Hash chain

`hash = sha256_hex(canonical JSON {seq, created, actor_kind, actor_id, actor_collection, impersonated_by, action, collection, record, before, after, request, prev_hash})`. Canonical = fixed key order, JSON columns re-encoded with sorted keys, no HTML escaping, numbers verbatim. The first row has `prev_hash = ""`. `diff` is derived from before/after and not part of the hash.

`verify` detects: modified rows, deleted or inserted rows in the middle (seq gap / prev_hash mismatch), first row missing (seq must start at 1). It cannot detect deletion of the newest rows (truncation of the tail) or a full rewrite of the chain by someone who recomputes all hashes: anchor the head hash externally (for example copy the last `hash` from `toki audit tail --limit 1 --json` somewhere else) if that matters.

## CLI

```
toki audit tail   [--since 1h] [--limit 50] [--json]
toki audit verify                      # exit code 1 and "BROKEN at seq N" on the first broken row
toki audit export --since <duration|date> [--out file]   # JSONL, oldest first, --out must not exist
```

`--since` accepts `90m`, `1h`, `7d`, `2026-10-01` or RFC3339.

## Retention

Deliberately absent in phase 1: there is no prune/delete command and no setting. The log is append-only; use `export` to archive. A future prune must be chain-aware (checkpoint row) and is out of scope.

## Limits

Phase 1 targets low-volume privileged traffic: one aux-DB insert per captured request, serialized by a mutex (a single writer, no cross-process coordination: do not run two writing processes on one `pb_data`).

## Phase 2 plan

- Optional logging of regular user writes (per-collection opt-in, async batch writer).
- Field-level permission/visibility audit (who read or changed protected fields), ties into `ruleguard`.
- Actor for backups and impersonation issuer (`impersonated_by` real id) by threading request context.
- Chain checkpoints + chain-aware prune, external head anchoring.
- Capture of Go/JS-hook writes and batch requests.
