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

## Not promised

- Go package API (`core`, `apis`, ...) may change between TokiBase minor versions.
- Admin UI internals.
