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

None yet. Every deviation must be listed here with: what changed, why, migration path.

## Not promised

- Go package API (`core`, `apis`, ...) may change between TokiBase minor versions.
- Admin UI internals.
