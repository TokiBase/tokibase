# Compatibility contract

TokiBase keeps the PocketBase public contract so existing apps and SDKs keep working.

## Promised (verified in CI against the official JS and Dart SDK test suites)

- REST endpoints under `/api/*`, request/response shapes, error codes.
- Filter, sort, expand, fields syntax and the rule expression language.
- Auth flows: password, OTP, OAuth2, token refresh, impersonation.
- Realtime SSE protocol.
- `pb_data` layout: `data.db`, `auxiliary.db`, `storage/`, `backups/`.
- Collection schema JSON and migrations format.
- `pb_hooks` JavaScript (goja) hooks, kept as the `js-compat` module.

Verified by: `tests/e2e` (an unchanged `pb_data` created by upstream v0.40.4 served by the TokiBase binary, exercised with the official `pocketbase` JS SDK; CI job `e2e`).

## Pinned upstream

| | |
| --- | --- |
| PocketBase version | v0.40.4 (commit 5cec579) |
| Go | 1.27 |
| SQLite driver | modernc.org/sqlite (pure Go) |

## Deviations

Every deviation must be listed here with: what changed, why, migration path.

### Go package API: `core` types are aliases of `kernel` types (phase 0)

- What: models, fields, DB helpers, settings, migrations runner and the non-HTTP hooks/events moved to the `kernel` package. `core.Record`, `core.Collection`, `core.Field*`, `core.ModelEvent`, etc. are type aliases of the kernel types, and `core.App` embeds `kernel.App`. HTTP/REST behavior, JSON shapes, SQL and `pb_data` are unchanged.
- Why: the kernel must not import `net/http` (see `docs/decisions/0001-fork-and-kernel-split.md`).
- Migration for Go users: transaction callbacks (`RunInTransaction`) and kernel event `App` fields are `kernel.App`; use `core.AsApp(app)` to get the server hooks back. `OAuth2ProviderConfig.InitProvider()` became `core.InitOAuth2Provider(cfg)`; `filesystem.System.Serve/NewS3/NewFileFromURL` moved to `tools/filesystem/fshttp`; `mailer.SMTPClient/Sendmail` moved to `tools/mailer/clients`. Migration files (`migrations.Register(func(app core.App) error ...)`) and `pb_hooks` JS are unaffected. Full list: `docs/PHASE0_KERNEL_SPLIT.md`.

### Go package API: SQLite driver moved to `modules/store/sqlite` (phase 0)

- What: the driver import, `DefaultDBConnect`, pool tuning, pragmas, WAL/optimize/vacuum, lock retry and the concurrent/nonconcurrent builder moved out of `kernel` into `modules/store/sqlite`. `kernel.BaseAppConfig` gained `DBOpener`; `kernel.DefaultDBConnect` became `sqlite.DefaultConnect` (`core.DefaultDBConnect` still exists). `kernel.NewBaseApp` no longer has a default driver and fails on Bootstrap without `DBOpener`; `core.NewBaseApp` and `tokibase.New*` wire the SQLite opener, and a custom `DBConnect` keeps working. `tokibase.ModerncDepsCheckHookId` is unchanged. SQL, pragmas, pool sizes, retry behavior and `pb_data` are unchanged.
- Why: the kernel must not depend on the SQLite driver (`docs/PHASE0_SQLITE_STORE.md`).
- Migration for Go users: only code that used `kernel.NewBaseApp` directly needs `DBOpener: sqlite.NewOpener()`.

### Boot and collection-save warnings for public rules (phase 1, ruleguard)

- What: `modules/ruleguard` logs a warning at boot and after collection saves when an API rule is `""` (public) and not allowlisted in `pb_data/ruleguard.json`; a `strict` policy makes the app refuse to start. New `toki rule lint|allow` commands. REST endpoints, collection JSON and rule semantics are unchanged; collection saves are never blocked.
- Why: an empty rule silently means public (a real incident exposed health data).
- Migration: nothing needed (default policy `warn`). Set `{"policy":"off"}` to silence, or allowlist with `toki rule allow <collection> <kind>...`.
### New `_audit` table in auxiliary.db (phase 1, `modules/audit`)

- What: new `_audit` table in `auxiliary.db` (append-only, hash-chained) and a new `audit` CLI command. No REST/SDK/API change; `data.db` untouched. `TOKI_AUDIT=off` skips registration (the table is then never created, an existing one is left as is).
- Why: tamper-evident trail of superuser, impersonated, schema and settings changes.
- Migration path: none needed; an upstream `pb_data` gains the table on first start, and removing it (or running upstream) simply ignores it. See `docs/modules/audit.md`.

### New `_jobs` table in auxiliary.db (phase 2, `modules/jobs`)

- What: new `_jobs` table in `auxiliary.db` (durable job queue), in-process workers started at `OnServe` (`TOKI_JOBS_WORKERS`, default 4), a new `jobs` CLI command and `serve --role worker` / `TOKI_ROLE=worker` (only `/api/health` is served). No REST/SDK change in the default role; `data.db` untouched. `TOKI_JOBS=off` skips registration.
- Why: shared foundation for push, webhooks and data jobs.
- Migration: none needed; an upstream `pb_data` gains the table on first start, and removing it (or running upstream) simply ignores it. See `docs/modules/jobs.md`.

### Boot warning for plain HTTP without trusted proxy (phase 1, tlscheck)

- What: `modules/tlscheck` logs a warning (and prints it to stderr) at `OnServe` when the server listens on plain HTTP on a non-loopback address and `Settings.TrustedProxy.Headers` is empty; `TOKI_TLS_CHECK=strict` makes `serve` refuse to start in that case. REST, settings and `pb_data` are unchanged.
- Why: such a server is probably reached by clients without encryption.
- Migration: nothing needed (default `warn`). Set the trusted proxy headers behind nginx/Caddy/Cloudflare, or `TOKI_TLS_CHECK=off`.

### Time zone check for submitted date values, new validation error code (phase 1, timelint)

- What: `modules/timelint` inspects date fields in record create/update requests. Default `TOKI_TIMELINT=warn` only logs (once per collection+field per hour) and changes nothing. With `TOKI_TIMELINT=strict` a date-time string that carries no zone designator (for example `2026-10-07 10:00:00`) is rejected with 400 and a field error with the new code `validation_invalid_timezone`, in the same shape as upstream field errors (`data.<field>.code/message`). Date-only values (`YYYY-MM-DD`) and values with `Z` or `+hh:mm` are accepted. New CLI `toki time lint`.
- Why: zoneless values are silently read as UTC, a common cause of "+N hours per sync" bugs.
- Migration: nothing needed (default `warn`). Before enabling `strict`, make clients send ISO 8601 with an offset or `Z`; stock PocketBase SDKs that send `Date.toISOString()` already comply.

### Structured denial logs (phase 1, denylog)

- What: `modules/denylog` writes one extra Warn log entry (message `denylog: request denied`, attribute `toki.deny=true`) for every 401, 403 and 429 response, with `status`, `method`, `path`, `ip`, `auth_kind`, `auth_id`, `collection`, `reason`, `rule_kind`, `rate_limited`. Log attributes and the `_logs` table content only: responses, status codes, headers and schemas are unchanged. New CLI `toki deny tail`.
- Why: denials were only visible as generic request logs without a machine-readable reason.
- Migration: nothing needed. `TOKI_DENYLOG=off` stops the extra entries (they count toward the logs retention like any other log).

### Backup verification after create, `toki backup` CLI (phase 1, backupcheck)

- What: `modules/backupcheck` verifies every created backup asynchronously (restore to a temp dir, `PRAGMA integrity_check`, counts, sampled files) and logs the result. New `toki backup create|list|verify|verify-all` commands (no `backup` CLI existed). Nothing changes in REST endpoints, backup file format or settings.
- Why: an untested backup is not a backup.
- Migration: nothing needed. `TOKI_BACKUP_VERIFY=off` disables the hook.

### Health response gains `data.replica` when WAL replication is active (phase 1, walreplica)

- What: for superuser callers only, and only while `TOKI_REPLICA_URL` replication is running, `GET /api/health` adds `data.replica` (`healthy`, optional `reason`, `lease` (`held`, `nodeId`, `hostname`, `heartbeatAt`, ...; file:// replicas), per database `localTxid`, `replicaTxid`, `lastSync`, `lagSeconds`, `lastError`). Responses for guests and regular users, and for servers without replication, are byte for byte unchanged. Built-in nightly and pre-backup `wal_checkpoint(TRUNCATE)` are skipped while replication is active (Litestream owns checkpoints), and backups exclude `.data.db-litestream` / `.auxiliary.db-litestream`.
- Why: replica lag is the signal operators need; an extra key in the superuser-only `data` map does not break the documented fields.
- Migration: nothing needed; clients ignoring unknown `data` keys are unaffected.

### Admin UI read-only or disabled by env (phase 1, adminlock)

- What: `TOKI_ADMIN_UI=readonly` makes collection create/update/delete/import, settings update and `_superusers` record create/update/delete return 403 (JSON error in the usual shape) when the request is a superuser request carrying a `Referer`/`Origin` that points at `/_/` on the same host. `TOKI_ADMIN_UI=off` clears `ui.DistDirFS` so `/_/` is 404, like a `no_ui` build (installer and OAuth2 redirect fall back as in `no_ui`). Default `on` is byte for byte upstream.
- Why: production schema changes should come from migration files in git, not from clicks in the UI.
- Migration: nothing needed; SDK, CLI and migration calls (no UI referer) are never blocked. See `docs/modules/adminlock.md` for the detection rule and its limits.
### `Retry-After` header and per-identity lockout on auth endpoints (phase 1, `modules/lockout`)

- What: after repeated failed `auth-with-password` / `auth-with-otp` for one identity (default 5 failures in 15 minutes), further attempts for that identity, even with the correct credentials, get the same 400 body as a wrong password (`Failed to authenticate.` / `Invalid or expired OTP`) plus a `Retry-After: <seconds>` header. The header is sent only while the identity is locked. New `_lockout` table in `auxiliary.db`, new `lockout` CLI command. No endpoint, status code or body shape changes.
- Why: upstream rate limits are IP/path based and do not stop credential stuffing spread over many IPs.
- Migration: nothing needed. `TOKI_LOCKOUT=off` disables it; `toki lockout unlock|clear` recovers locked accounts. See `docs/modules/lockout.md`.

### `sid` claim in auth JWTs, revoked tokens answer 401 before expiry, new `_sessions` table (phase 2, `modules/sessions`)

- What: every auth token (login, refresh, impersonation) gains a `sid` claim and a row in the new `_sessions` table in `data.db`. A token whose session was revoked (CLI, password or email change, optional rotation) is treated as unauthenticated: authed endpoints return upstream's 401 shape before the token's `exp`. Tokens issued without the module (no `sid`) are unaffected. New `sessions` CLI command, optional request header `X-Toki-Device`. Rotation (`TOKI_SESSIONS_ROTATE=on`, default off) makes `auth-refresh` revoke the previous token, which can log out parallel tabs sharing a token. Endpoints, bodies and token signing are unchanged; the kernel gained the `kernel.OnAuthTokenIssue` seam (nil = upstream behavior).
- Why: stolen or lost-device tokens could not be invalidated before expiry without changing the user's `tokenKey`.
- Migration: nothing needed; an upstream `pb_data` gains the table on first start and upstream ignores it (upstream does not know `sid`, so after going back to upstream revoked tokens work again until expiry). `TOKI_SESSIONS=off` disables everything. See `docs/modules/sessions.md`.
### `_field_rules` system collection and per-field rules (phase 2, `modules/fieldperm`)

- What: new system collection `_field_rules` (superusers only) stores per-field `read_rule` / `write_rule`. A field whose read rule fails is simply absent from the JSON of list/view/create/update responses, realtime events and expanded records (same as a hidden field); a write rule that fails answers `400` with `data.<field>.code = validation_field_not_allowed`, in the upstream field error shape. New CLI `toki fieldperm`. Collection JSON, endpoints and SDK contracts are unchanged; with no rows nothing changes.
- Why: guarding a single field (for example a clan `leader`) needed hand written hooks.
- Migration: nothing needed; the collection is created at boot. Clients must not assume every schema field is present in a response. See `docs/modules/fieldperm.md`.
### `_computed_fields` system collection and server-maintained number fields (phase 2, `modules/computed`)

- What: new system collection `_computed_fields` (superusers only) defines counters and rollups on EXISTING number fields of a parent collection. The server rewrites such a field after every committed write of the child collection. A create or update request that changes a computed field answers `400` with `data.<field>.code = validation_computed_field` (superusers too, unless `TOKI_COMPUTED_ALLOW_MANUAL=1`); re-sending the stored value is accepted. Parent saves made by the module fire the normal record hooks, realtime events and webhooks. New CLI `toki computed`. Collection JSON, endpoints and SDK contracts are unchanged; with no rows nothing changes.
- Why: totals such as `likes_count` were written by client code or hand written hooks and drifted.
- Migration: nothing needed; the collection is created at boot. Clients that wrote these fields must stop (the write is rejected); run `toki computed backfill <collection>` after adding a definition. See `docs/modules/computed.md`.
### New `_webhooks` collection and `_webhook_deliveries` table, outbound HTTP (phase 2, `modules/webhooks`)

- What: new superuser-only collection `_webhooks` in `data.db` (appears in `GET /api/collections` for superusers, field `secret` is hidden) and new `_webhook_deliveries` table in `auxiliary.db`; the process sends signed HTTP POSTs to configured URLs after record, collection and `auth.login` events; new `webhooks` CLI command. Existing endpoints, status codes and response shapes are unchanged; with no webhook configured nothing is sent.
- Why: integrations without polling or custom `pb_hooks`.
- Migration path: nothing needed. `TOKI_WEBHOOKS=off` skips registration (the collection and table are then not created; existing ones stay). Upstream PocketBase ignores the auxiliary table and treats `_webhooks` as a normal base collection; delete it to return to a pristine schema. See `docs/modules/webhooks.md`.
### New `/api/push/*` endpoints and `_push_*` collections, outbound HTTPS (phase 2, `modules/push`)

- What: new additive endpoints `POST /api/push/devices`, `DELETE /api/push/devices/{token}`, `POST /api/push/subscribe`, `POST /api/push/unsubscribe`, `GET /api/push/topics` (any auth record, own devices only) and `POST /api/push/send` (superuser); new superuser-only system collections `_push_devices`, `_push_topics`, `_push_subscriptions` in `data.db` (appear in `GET /api/collections` for superusers); deleting an auth record also deletes its devices; the process calls FCM and APNs when a send job runs; new `push` CLI command. Existing endpoints, status codes and response shapes are unchanged; with no provider configured nothing is sent.
- Why: mobile push without custom `pb_hooks` and provider glue.
- Migration path: nothing needed. `TOKI_PUSH=off` skips registration (collections stay if they exist). Upstream PocketBase treats the `_push_*` collections as normal base collections and ignores the endpoints; delete them to return to a pristine schema. See `docs/modules/push.md`.
### `_agents` system collection for MCP agent identities (phase 2, `modules/mcp`)

- What: a new system collection `_agents` (main `data.db`: `name`, `key_hash` (hidden), `role`, `collections`, `rate_per_min`, `enabled`, `created`, `updated`; all API rules null, superusers only) is created at bootstrap when missing. It shows up in `GET /api/collections` for superusers like any system collection. No endpoint, status code or body shape changes. PR 1 agents evaluate collection rules as guest and write after a role and allowlist check (`docs/modules/mcp.md`, "Limitations of PR 1"); rules gain no new syntax yet.
- Why: agent identity and revocation must live in the same database as the data, and rules of a later phase will reference it.
- Migration: nothing needed for REST clients and SDKs. Built with `-tags no_mcp` the collection is not created. Code that enumerates all collections (for example schema exporters) should skip `_agents` like the other system collections.

### New `/api/collections/{collection}/passkeys/*` endpoints and `_passkeys` collection (phase 2, `modules/passkey`)

- What: when `TOKI_PASSKEY_RP_ID` is set, six additive endpoints appear under `/api/collections/{collection}/passkeys/` (`register/options`, `register/verify`, `GET` list, `DELETE {id}`, `login/options`, `login/verify`). `login/verify` returns the standard auth response (`token`, `record`, `meta`) with `AuthMethod` `passkey`, so authRule, MFA, sessions and `OnRecordAuthRequest` apply. New system collection `_passkeys` (superusers only, `public_key` hidden) in `data.db`, new `_passkey_challenges` table in `auxiliary.db`, new `passkey` CLI command. Without the env nothing is registered: the endpoints answer 404 and no collection is created. No existing endpoint, status code or body shape changes.
- Why: passwordless login with passkeys (Face ID / fingerprint / security key) without a custom backend.
- Migration: nothing needed for REST clients and SDKs. Code that enumerates all collections can see `_passkeys` (like other `_` system collections). See `docs/modules/passkey.md`.

### New `GET /api/collections/{collection}/records/near` endpoint and `distance_km` item key (phase 2, `modules/geo`)

- What: an additive endpoint for radius (`near=<field>:<lat>,<lon>,<km>`) and bounding-box (`bbox=...`) queries on `geoPoint` fields. It returns the standard list envelope; with `near` each item carries one extra top-level key `distance_km` and results are ordered by distance. The collection `listRule` applies as on the list endpoint. `toki geo index` can create an SQLite R*Tree virtual table `_geo_<collection>_<field>` in `data.db`. No existing endpoint, status code, body shape or the filter language changes. The path `records/near` shadows a record whose id is literally `near` (ids are 15 characters by default).
- Why: nearby search without custom hooks or client-side filtering that breaks pagination.
- Migration: nothing needed for REST clients and SDKs. Drop the `_geo_*` tables (`toki geo drop`) before returning to upstream if you want a pristine schema; upstream ignores them. See `docs/modules/geo.md`.
### New hooks directory `pb_hooks_wasm/`, `_wasm_kv` and `_wasm_stats` tables (phase 2, `modules/wasm`)

- What: new flags `--wasmHooksDir` (default `<dataDir>/../pb_hooks_wasm`) and `--wasmHooksWatch`; `*.wasm` modules found there run as sandboxed record, cron, route and job hooks (a record before-hook can change fields or reject with a 4xx/5xx the guest chooses; a failing guest answers `500 {"message":"Hook failed."}`); custom routes only appear when a module declares them; new `wasm` CLI command; two small tables `_wasm_kv` and `_wasm_stats` in `auxiliary.db` (not collections, not visible in the REST API). No existing endpoint, status code or response shape changes, and with an empty or missing directory nothing runs. JS `pb_hooks` are untouched and run after WASM handlers of the same event.
- Why: business logic in any language with CPU/memory/time limits, without goja.
- Migration: nothing needed. `TOKI_WASM=off` or `-tags no_wasm` removes the feature; upstream PocketBase ignores the directory and the auxiliary tables.
### New `/api/collections/{collection}/totp/*` and `auth-with-totp` endpoints, `_totp` collection, `totp_required` error (phase 2, `modules/totp`)

- What: additive endpoints `POST .../totp/setup`, `POST .../totp/confirm`, `DELETE .../totp`, `POST .../totp/recovery/regenerate` (auth record, fresh auth) and `POST .../auth-with-totp` (`{mfaId, code}`, completes an MFA challenge with the standard auth response, `AuthMethod` `totp`). New system collection `_totp` (superusers only; created when a key is configured). New error: a login can answer `403` with `data.totp.code = "totp_required"` when `TOKI_TOTP_REQUIRED_ROLES` / `TOKI_TOTP_REQUIRE_SUPERUSERS` apply and the user has no TOTP and no grace window. New `totp` CLI command. Without enforcement env/params no existing endpoint, status code or body shape changes.
- Why: authenticator-app second factor and recovery codes without a custom backend.
- Migration: nothing needed for REST clients and SDKs. Upstream PocketBase ignores the endpoints and treats `_totp` as a normal base collection; delete it to return to a pristine schema. See `docs/modules/totp.md`.

## Not promised

- Go package API (`core`, `apis`, ...) may change between TokiBase minor versions.
- Admin UI internals.
- The jsvm `types.d.ts` now also exposes a `kernel` namespace; `core.*` names remain as aliases.

### `_crypto_*` collections, `_crypto_index` table, filter rejection on encrypted fields (phase 2, `modules/crypto`)

- What: system collections `_crypto_fields` and `_crypto_keys` (rules `null`) and the plain table `_crypto_index` in `data.db`, created at bootstrap; new `crypto` CLI command and `GET /api/crypto/lookup/{collection}/{field}`. Only when an operator enables encryption for a field (`toki crypto enable`): the column then holds `tkc1:<ver>:...` ciphertext, record API responses are decrypted by `OnRecordEnrich`, and `filter=`/`sort=` on `GET /api/collections/{c}/records` that touch an encrypted field answer `400` with `data.<filter|sort>.code = "validation_encrypted_field"`. Writes to a collection with encrypted fields are refused when no master key is configured. Go/JS code that loads records with `app.Find*` sees ciphertext until `crypto.Decrypt(app, record)`. Collection JSON schema, other endpoints and rules are unchanged; with no encrypted fields nothing changes.
- Why: protect data in dumps, backups and replicas.
- Migration: none needed; an upstream `pb_data` gains the empty collections/table on first start. Before moving back to upstream run `toki crypto disable <collection> <field> --i-understand` for every field, otherwise upstream serves ciphertext. See `docs/modules/crypto.md`.
