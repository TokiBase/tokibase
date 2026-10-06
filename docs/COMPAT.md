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

### Backup verification after create, `toki backup` CLI (phase 1, backupcheck)

- What: `modules/backupcheck` verifies every created backup asynchronously (restore to a temp dir, `PRAGMA integrity_check`, counts, sampled files) and logs the result. New `toki backup create|list|verify|verify-all` commands (no `backup` CLI existed). Nothing changes in REST endpoints, backup file format or settings.
- Why: an untested backup is not a backup.
- Migration: nothing needed. `TOKI_BACKUP_VERIFY=off` disables the hook.

## Not promised

- Go package API (`core`, `apis`, ...) may change between TokiBase minor versions.
- Admin UI internals.
- The jsvm `types.d.ts` now also exposes a `kernel` namespace; `core.*` names remain as aliases.
