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
### `_batch_rules` system collection and batch rejection code (phase 2, `modules/batchguard`)

- What: new system collection `_batch_rules` (superusers only) holds rules that validate a whole `/api/batch` call (`assert` before, `assert_post` after the sub-requests, inside the batch transaction). A rejected batch answers `400` with `{"message":"Batch rejected.","data":{"batch":{"code":"validation_batch_rule","message":"<rule message>","rule":"<name>"}}}` and nothing is committed. When a rule with `assert_post` or a `kernel.OnBatchFor(app)` handler applies, the batch response is sent after the commit instead of while the transaction is open (same body and status). New CLI `toki batch`. Endpoints, request and success response shapes are unchanged; with no applicable rule nothing changes.
- Why: record rules cannot express invariants across the records of one batch.
- Migration: nothing needed; the collection is created at boot. Clients should handle `data.batch.code = validation_batch_rule`. See `docs/modules/batchguard.md`.

### `embed` and `mobile` packages, `Config.SkipFlagParse` (phase 2)

- What: additive Go API. Package `embed` (`embed.Start`, `Instance.Call/CallContext/Subscribe/SubscribeAs/Superuser/Export/Stop`) runs the server inside another process; `mobile` is its gomobile facade. `tokibase.Config.SkipFlagParse` skips the eager `os.Args` flag parsing, so `--dev`, `--hooksDir`, `--encryptionEnv` and the other global flags are never read: pass them through `Config` (for settings encryption set `Config.DefaultEncryptionEnv`, or `embed.Options.EncryptionEnv`). `embed` sets `ServeEvent.InstallerFunc = nil` (no installer link). The embedded loopback listener sends no CORS grant by default and answers only loopback Host headers; `mobile.Start` has no TCP listener unless an address is given. `kernel.OnBatch` (process global) became `kernel.OnBatchFor(app)`. No HTTP endpoint or response shape changes.
- Why: run nano on mobile and desktop without a CLI.
- Migration: nothing for servers. Go callers of the unreleased `kernel.OnBatch` bind on `kernel.OnBatchFor(app)`. See `docs/EMBED.md`.

### New `_webhooks` collection and `_webhook_deliveries` table, outbound HTTP (phase 2, `modules/webhooks`)

- What: new superuser-only collection `_webhooks` in `data.db` (appears in `GET /api/collections` for superusers, field `secret` is hidden) and new `_webhook_deliveries` table in `auxiliary.db`; the process sends signed HTTP POSTs to configured URLs after record, collection and `auth.login` events; new `webhooks` CLI command. Existing endpoints, status codes and response shapes are unchanged; with no webhook configured nothing is sent.
- Why: integrations without polling or custom `pb_hooks`.
- Migration path: nothing needed. `TOKI_WEBHOOKS=off` skips registration (the collection and table are then not created; existing ones stay). Upstream PocketBase ignores the auxiliary table and treats `_webhooks` as a normal base collection; delete it to return to a pristine schema. See `docs/modules/webhooks.md`.
### New `/api/push/*` endpoints and `_push_*` collections, outbound HTTPS (phase 2, `modules/push`)

- What: new additive endpoints `POST /api/push/devices`, `DELETE /api/push/devices/{token}`, `POST /api/push/subscribe`, `POST /api/push/unsubscribe`, `GET /api/push/topics` (any auth record, own devices only) and `POST /api/push/send` (superuser); new superuser-only system collections `_push_devices`, `_push_topics`, `_push_subscriptions` in `data.db` (appear in `GET /api/collections` for superusers); deleting an auth record also deletes its devices; the process calls FCM and APNs when a send job runs; new `push` CLI command. Existing endpoints, status codes and response shapes are unchanged; with no provider configured nothing is sent.
- Why: mobile push without custom `pb_hooks` and provider glue.
- Migration path: nothing needed. `TOKI_PUSH=off` skips registration (collections stay if they exist). Upstream PocketBase treats the `_push_*` collections as normal base collections and ignores the endpoints; delete them to return to a pristine schema. See `docs/modules/push.md`.
### `_agents` system collection for MCP agent identities (phase 2, `modules/mcp`)

- What: a new system collection `_agents` (main `data.db`: `name`, `key_hash` (hidden), `role`, `collections`, `rate_per_min`, `enabled`, `created`, `updated`; all API rules null, superusers only) is created at bootstrap when missing. It shows up in `GET /api/collections` for superusers like any system collection. No endpoint, status code or body shape changes. Agents write after a role and allowlist check (`docs/modules/mcp.md`).
- Why: agent identity and revocation must live in the same database as the data, and rules of a later phase will reference it.
- Migration: nothing needed for REST clients and SDKs. Built with `-tags no_mcp` the collection is not created. Code that enumerates all collections (for example schema exporters) should skip `_agents` like the other system collections.

### `@request.auth.kind` rule field and `/api/mcp` (phase 2, `modules/mcp` PR 2, `kernel`)

- What: (1) collection rules and filters gain a virtual field `@request.auth.kind` with the value `guest` (no auth), `user` (a record of any auth collection except `_superusers`), `superuser` or `agent` (an MCP agent). A real field named `kind` on the auth collection takes precedence, so existing rules on such collections keep their meaning. Generated SQL for all existing expressions is byte-identical (`@request.auth.kind` is a bound parameter like other static request values). (2) MCP agents (not operators) are evaluated with their `_agents` record as the request auth, namespaced: `@request.auth.id`, `.collectionId`, `.collectionName` and `.kind` resolve; the agent attributes are `@request.auth.agent.role`, `.name`, ...; every other `@request.auth.<field>` (`role`, `name`, `email`, ...) is empty for an agent, so rules written for user fields never match an agent. **Behaviour change for MCP only:** a rule such as `@request.auth.id != ""` now admits agents (also in fieldperm read/write rules and record request hooks run for agent writes); in PR 1 agents were guests. Use `@request.auth.kind = "user"` for user-only rules. REST requests are unaffected (agents can not authenticate over REST). (3) `_agents` gains the fields `sandbox` (bool) and `expires` (date), added to existing instances at bootstrap. (4) With `TOKI_MCP=on` the route `/api/mcp` (any method) exists (behind a reverse proxy: set the Application URL setting or `TOKI_MCP_ALLOWED_HOSTS`, see `docs/modules/mcp.md`); default off, absent in `no_mcp` builds. No existing endpoint, status code or body shape changes.
- Why: per-collection rules for agents instead of guest evaluation, and remote agents over HTTP.
- Migration: nothing for REST clients and SDKs. Review rules that only test `@request.auth.id != ""` before creating reader/writer agents with access to those collections, and add `@request.auth.kind = "user"` where needed. Upstream PocketBase has no `kind` field; rules using it do not port back unless the auth collection has such a field.

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

### WASM batch events and `http_allow` (phase 2, `modules/wasm`)

- What: a WASM module may list `batch.before`, `batch.after` or `batch.*` in its sidecar `events`; it is then called inside the `/api/batch` transaction and can reject the whole batch with a status and message of its choosing (default 400), or fails closed with `500 {"message":"Hook failed."}` (`413` when the sub-requests exceed 4 MiB; `500` when the time budget is exceeded). New sidecar key `http_allow` narrows the outbound HTTP allowlist per module.
- Visible change: only for deployments that add such a module; the rejection body is the standard `{"status","message","data"}` error. Without a module declaring a batch event nothing is bound and `/api/batch` is unchanged. With any batch hook bound (even `batch.before` only) every batch that reaches the guard is buffered and committed before the response is sent, and `,id` is appended to `fields=` of sub-request URLs so the host can find the written records; the added `id` is removed from the response again, so clients get exactly the fields they asked for. A guest may answer with any status 400-599 (including 401/403/429). `batch.after` receives public data (hidden fields and credential fields such as `tokenKey` are never delivered, like record events). The total guest time of one batch is capped by `TOKI_WASM_BATCH_BUDGET` (default 10s, over all modules and both phases; exceeding fails the batch closed with 500) because the batch transaction holds the write lock. A hook bound during a request (hot reload) makes that request fail closed with `503` instead of skipping `batch.after`. Upstream PocketBase has no equivalent and ignores `pb_hooks_wasm/`.
- Migration: nothing needed.

### New `/api/collections/{collection}/totp/*` and `auth-with-totp` endpoints, `_totp` collection, `totp_required` error (phase 2, `modules/totp`)

- What: additive endpoints `POST .../totp/setup`, `POST .../totp/confirm`, `DELETE .../totp`, `POST .../totp/recovery/regenerate` (auth record, fresh auth) and `POST .../auth-with-totp` (`{mfaId, code}`, completes an MFA challenge with the standard auth response, `AuthMethod` `totp`). New system collection `_totp` (superusers only; created when a key is configured). New error: a login can answer `403` with `data.totp.code = "totp_required"` when `TOKI_TOTP_REQUIRED_ROLES` / `TOKI_TOTP_REQUIRE_SUPERUSERS` apply and the user has no TOTP and no grace window. New `totp` CLI command. Without enforcement env/params no existing endpoint, status code or body shape changes.
- Why: authenticator-app second factor and recovery codes without a custom backend.
- Migration: nothing needed for REST clients and SDKs. Upstream PocketBase ignores the endpoints and treats `_totp` as a normal base collection; delete it to return to a pristine schema. See `docs/modules/totp.md`.

### Lean build profiles `edge` and `nano` (phase 2, `profiles.txt`)

- What: only for binaries built with the lean tag sets (`solo`, `team` and `cluster` are unchanged). `no_jsvm` removes the JS engine: `pb_hooks/` and JS `pb_migrations/*.js` are not loaded, so hooks (including authorization hooks) do not run and JS migrations are not applied; the `--hooksDir/--hooksWatch/--hooksPool/--migrationsDir` flags still parse. The binary refuses to start when `--hooksDir`/`--migrationsDir` is set or when a `pb_hooks`/`pb_migrations` directory exists next to the data dir, unless `TOKI_ALLOW_STUBBED_MODULES=1` (then it logs at ERROR). `no_migratecmd`: `toki migrate` exits with an explicit "compiled out" error and automigrate is off. `no_ghupdate`: `toki update` exits with a "compiled out" error. `no_totp`/`no_geo` drop the TOTP and geo endpoints (stubbed module guard applies to `_totp` and its env vars; run `toki geo` rebuild on a geo-enabled build before using `records/near` on data written by a `no_geo` build).
- Why: smaller binaries for edge and embedded targets.
- Migration: use the `solo` tag set (no tags) to keep full upstream behavior. See `docs/PROFILES.md`.

### Opt-in rule compiler path `TOKI_RULE_AST=1` (phase 2, `kernel/rule`)

- What: experimental switch, read once at process start (default off). With `1` filters and rules are parsed into an AST and emitted by `tools/search.EmitAST`; the SQL and parameters are byte-identical to the default compiler (differential tests). Both paths now also reject filters longer than 65536 bytes or nested deeper than 64 groups with a `400`-class filter error (upstream has no such limits).
- Why: groundwork for other SQL dialects and an in-memory rule evaluator.
- Migration: nothing needed; unset the variable to use the default path. See `docs/RULE_ENGINE.md`.
- Rule engine PR 2 (`rule.Dialect`, `kernel/rule/pg`) changes nothing user-visible: the SQLite SQL, parameters and error texts are byte-identical (same differential corpus). Compat restore (QC round 5): PR 2 had changed the JSON path segment handling (a segment like `1é` gave `$.a[1]` instead of PocketBase's `$.a.1`); the PocketBase order (index check on the raw segment, then sanitizing) is back and pinned by `TestRecordFieldResolverJSONPathSegmentOrder` / `TestSegmentFromRawUpstreamOrder`.

## Not promised

- Go package API (`core`, `apis`, ...) may change between TokiBase minor versions.
- Admin UI internals.
- The jsvm `types.d.ts` now also exposes a `kernel` namespace; `core.*` names remain as aliases.

### `_crypto_*` collections, `_crypto_index` table, filter rejection on encrypted fields (phase 2, `modules/crypto`)

- What: system collections `_crypto_fields` and `_crypto_keys` (rules `null`) and the plain table `_crypto_index` in `data.db`, created at bootstrap; new `crypto` CLI command and `GET /api/crypto/lookup/{collection}/{field}`. Only when an operator enables encryption for a field (`toki crypto enable`): the column then holds `tkc1:<ver>:...` ciphertext, record API responses are decrypted by `OnRecordEnrich`, and `filter=`/`sort=` on `GET /api/collections/{c}/records` that touch an encrypted field answer `400` with `data.<filter|sort>.code = "validation_encrypted_field"`, except equality (`=`, `!=`, `?=`, `?!=` against a non-empty string) on `blind-index` fields, which is rewritten to an index lookup (crypto PR 2; also in collection rules). `tools/search.ResolverResult` gains the optional `BeforeBuild` hook and `kernel` the `BlindIndexProvider` seam; SQL of expressions that do not touch a blind-index field is unchanged. Writes to a collection with encrypted fields are refused when no master key is configured. Go/JS code that loads records with `app.Find*` sees ciphertext until `crypto.Decrypt(app, record)`. Collection JSON schema, other endpoints and rules are unchanged; with no encrypted fields nothing changes.
- Why: protect data in dumps, backups and replicas.
- Migration: none needed; an upstream `pb_data` gains the empty collections/table on first start. Before moving back to upstream run `toki crypto disable <collection> <field> --i-understand` for every field, otherwise upstream serves ciphertext. See `docs/modules/crypto.md`.

### `_roles` / `_memberships` system collections and the rule functions `@role()` / `@member()` (phase 2, `modules/roles`)

- What: two new system collections (superusers only, `null` rules): `_roles` (`name` unique, `description`) and `_memberships` (`user_collection`, `user`, `role` relation to `_roles` with cascade delete, `scope`, `scope_collection`, `expires`). Collection rules and filters gain the functions `@role("name") = true`, `@role("name", scope) = true` and `@member(scope) = true` (SQLite only; the names start with `@`, which no upstream function does, so no existing expression changes meaning and the generated SQL of every expression that does not use them is byte-identical). Deleting an auth record or a scope record deletes its memberships. New CLI `toki roles`.
- Why: roles and per-team/tenant grants without a `role` field on every auth collection or custom hooks.
- Migration: nothing needed; the collections are created at boot and are empty. A rule that uses `@role()` does not validate on a build with `no_roles` (and on upstream PocketBase); remove such rules before moving back. See `docs/modules/roles.md`.
