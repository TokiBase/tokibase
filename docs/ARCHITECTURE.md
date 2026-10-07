# Architecture

Design document: https://claude.ai/code/artifact/eec9a0d2-dea1-4df7-9ed3-d699224ea771

## Principle

A kernel that knows nothing about HTTP, OS processes or UI, plus modules behind one contract.

```
kernel/     schema, store interface, rule engine, record API, event bus, identity
server/     REST + SSE on top of kernel (PocketBase contract)
embed/      in-process Go API for other applications
mobile/     gomobile bindings (AAR, XCFramework)
modules/    storage drivers, auth providers, realtime, hooks, jobs, replication, sync, ops, edge
```

Dependency rules, enforced by `depguard` in CI:

1. `kernel` imports nothing outside stdlib and `dbx` (the SQLite driver lives in `modules/store/sqlite`).
2. Modules import `kernel` only; modules never import each other.
3. `server` imports `kernel` and modules.

## Phase 0 (current)

Goal: profile `solo` behaves identically to PocketBase v0.40.4.

- [x] Fork v0.40.4, module path `github.com/tokibase/tokibase`, root package `tokibase`.
- [x] Map every place SQLite leaks above the store layer (`docs/PHASE0_AUDIT.md`).
- [x] Move `core/` pieces into `kernel/`, no behavior change (`docs/PHASE0_KERNEL_SPLIT.md`). `core` stays as the server facing compatibility package (type aliases + request hooks). Store interface is still to come.
- [x] `depguard` rule for `kernel/**` in `golangci.yml` and `kernel/deps_test.go` (runs in CI with `go test`).
- [x] Move the SQLite driver, connection setup, pragmas, maintenance, lock retry and error classification to `modules/store/sqlite` behind `kernel.DBOpener`/`kernel.DBConn` (`docs/PHASE0_SQLITE_STORE.md`). Typed record CRUD on the store is still to come.
- [x] CI job that runs `golangci-lint` (kernel and core).
- [x] CI: build matrix (linux/darwin/windows x amd64/arm64), `-s -w`, size budgets.
- [x] CI: official JS SDK suite against the binary (`tests/e2e/sdk`, job `e2e`). Dart SDK suite still to come.
- [x] Serve an unchanged `pb_data` created by upstream v0.40.4 (`tests/e2e/seed.sh` + `run.sh`).

Exit gate: 100% SDK suite pass, kernel has no `net/http` import, all builds under budget.

## Phase 1 (functional scope done 2026-10-07; production proof pending)

- [x] ruleguard: explicit public API rules, `toki rule lint` (`docs/modules/ruleguard.md`).
- [x] audit: append-only hash-chained `_audit` log, `toki audit tail|verify|export` (`docs/modules/audit.md`).
- [x] backupcheck: every backup is restored to a temp dir and verified, `toki backup verify` (`docs/modules/backupcheck.md`).
- [x] walreplica: continuous WAL replication with embedded Litestream, `toki replica status|restore|snapshot|promote`, lease guard, failover drill in CI (`docs/modules/walreplica.md`). S3 backend behind `-tags replica_s3`.
- [x] adminlock: `TOKI_ADMIN_UI=on|readonly|off` (`docs/modules/adminlock.md`).
- [x] lockout: progressive per-identity lockout for failed password/OTP auth (`docs/modules/lockout.md`).
- [x] tlscheck: boot warning when serving plain HTTP without a trusted proxy header (`docs/modules/tlscheck.md`).
- [x] timelint: warn about or reject date values without a timezone at the API boundary, `toki time lint` (`docs/modules/timelint.md`).
- [x] denylog: every 401/403/429 carries a machine-readable reason, `toki deny tail` (`docs/modules/denylog.md`).
- [ ] Production proof: run on the FGR replica node with real data for 7 days, then cut over.

Exit gate: all modules on by default in profile `solo`, failover drill RTO under 30 s in CI, no COMPAT deviation on the REST contract.

## Phase 2 (in progress)

- [x] sessions: server-side sessions with `sid` JWT claim, revoke per device/all, revoke on password/email change, optional refresh rotation, `toki sessions` (`docs/modules/sessions.md`).
- [x] jobs: durable `_jobs` queue with retry/backoff, dead-letter, cron and worker role; consumer interface `kernel.Jobs(app)` so modules never import each other, `toki jobs ...` (`docs/modules/jobs.md`).
- [x] fieldperm: per-field read/write rules in `_field_rules`, `toki fieldperm list|set|rm|lint` (`docs/modules/fieldperm.md`).
- [x] webhooks: outbound record/collection/auth events with HMAC signatures, retries, dead-letter, replay, `toki webhooks` (`docs/modules/webhooks.md`). Own delivery table for now; moves onto the kernel job queue when `modules/jobs` lands.
- [x] push: FCM HTTP v1 and APNs (JWT, HTTP/2) with device registry, topics, `/api/push/*` and delivery as `push.send` jobs, `toki push` (`docs/modules/push.md`).
- [x] crypto (PR 1): per-field encryption at rest with envelope keys, `random` and `blind-index` modes, `_crypto_keys`/`_crypto_fields`/`_crypto_index`, rotation, `toki crypto ...` (`docs/modules/crypto.md`). 
- [x] crypto (PR 2): equality filters on `blind-index` fields (`=`, `!=`, `?=`, `?!=`, relation paths, `@collection`, rules) via `kernel.BlindIndexProvider` + `search.ResolverResult.BeforeBuild`; visibility as the lookup endpoint. Sort, range/prefix search and crypto-shredding by tenant/user are later PRs.
- [x] mcp (PR 1): Model Context Protocol server over stdio, `_agents` identities, core tools, resources, prompts, `toki agent|mcp|gen` (`docs/modules/mcp.md`).
- [x] computed (PR 1): server-maintained counters and rollups (count/sum/avg/min/max/last) on existing number fields, defined in `_computed_fields`, client writes rejected, `toki computed list|add|rm|backfill|verify|drift` (`docs/modules/computed.md`). Nested rollups and multi-relation sources come later.
- [x] passkey: WebAuthn/FIDO2 passkeys on any auth collection, discoverable login returning the standard auth response, `_passkeys` + clone detection, lockout/audit sinks, `toki passkey` (`docs/modules/passkey.md`).
- [x] geo: radius/bbox queries and distance ordering on `geoPoint` via `GET /api/collections/{c}/records/near`, optional SQLite R*Tree index, `toki geo index|rebuild|drop` (`docs/modules/geo.md`).
- [x] wasm (PR 1): sandboxed hooks in `pb_hooks_wasm/` on wazero (record before/after, cron, route, job events; limits, host API `toki/1`, Go guest SDK), `toki wasm list|stats|run|validate` (`docs/modules/wasm.md`). JS `pb_hooks` stay as js-compat. `no_wasm` build tag drops it (about 2.8 MB).
- [x] wasm (PR 2): `batch.before`/`batch.after`/`batch.*` events on `kernel.OnBatchFor` (inspect sub-requests, reject the whole batch inside its transaction, fail closed, redacted), per-module `http_allow`, Go SDK `ev.Batch`. Fuel metering, auth-request and collection events stay later.
- [x] batchguard: cross-record validation of atomic `/api/batch` calls, rules in `_batch_rules` with a small safe expression language (`assert` before, `assert_post` after the sub-requests, one transaction), `kernel.OnBatch` events for WASM, `toki batch rules ...` (`docs/modules/batchguard.md`).
- [x] totp: RFC 6238 TOTP as an MFA method on the upstream `mfaId` flow, recovery codes, per-role enforcement with grace window, `toki totp` (`docs/modules/totp.md`).
- [x] nano/embed PR 1: `embed/` in-process Go API (`Start`, `Call` without TCP, `Subscribe`, `Superuser`, `Export`, one instance per data dir), `mobile/` gomobile facade, `mobile/build.sh`, `make aar|xcframework`, CI job `embed` (`docs/EMBED.md`). gomobile itself is not run in CI.
- [x] rule engine (PR 1): dialect-neutral rule AST in `kernel/rule` (`Parse` on top of fexpr) and a SQLite emitter (`kernel/rule/sql.Emit`) with byte-identical SQL/params to the legacy compiler; opt-in via `TOKI_RULE_AST=1` (default off), differential tests in `tools/search`, CI job `test-rule-ast` (`docs/RULE_ENGINE.md`). PostgreSQL emitter and in-memory evaluator are later PRs.
- [x] mcp (PR 2): streamable HTTP transport at `/api/mcp` (`TOKI_MCP=on`, bearer agent keys), `@request.auth.kind` (`guest|user|superuser|agent`) in rules with agents evaluated as `_agents` auth, sandbox mode (`--sandbox`, writes rolled back), `expires` (`docs/modules/mcp.md`). Copy-of-`pb_data` sandbox deferred.
- [x] rule engine (PR 1): dialect-neutral rule AST in `kernel/rule` (`Parse` on top of fexpr) and a SQLite emitter (`kernel/rule/sql.Emit`) with byte-identical SQL/params to the legacy compiler; opt-in via `TOKI_RULE_AST=1` (default off), differential tests in `tools/search`, CI job `test-rule-ast` (`docs/RULE_ENGINE.md`).
- [x] rule engine (PR 2): resolver/emitter split (`rule.Ref`, `rule.Dialect`, SQLite dialect byte-identical to the legacy SQL, differential corpus unchanged) and a PostgreSQL emitter/dialect (`kernel/rule/pg`, tested by golden SQL + structural parity, no PostgreSQL runtime). Unsupported on PostgreSQL: table valued joins (`:each`, multi-value relation hops) and `strftime`. In-memory evaluator is a later PR (`docs/RULE_ENGINE.md`).
- [ ] mcp (PR 2): streamable HTTP transport, `@request.auth.kind = "agent"` in rules, sandbox mode.

## Size budgets (stripped; CI enforces the numbers in `profiles.txt`: solo 46 MiB, team 46 MiB, no_ui 43 MiB, cluster (solo+replica_s3) 55 MiB, edge 28 MiB, nano 24 MiB). The full-featured `solo` build has reached the 45 MB design budget with passkey (+2.1 MB, go-webauthn/TPM/CBOR), mcp (+2 MB) and push; the `edge`/`nano` profiles exclude these through build tags (`no_mcp`, `no_passkey`, `no_push`, `no_webhooks`, `no_crypto`, ... see `docs/PROFILES.md`).

| Profile | Design budget | Measured linux/amd64 (arm64) | CI budget |
| --- | --- | --- | --- |
| nano | 14 MB per arch | 21.7 MiB (20.4) | 24 MiB |
| edge | 28 MB | 25.0 MiB (23.5) | 28 MiB |
| solo | 45 MB | 42.8 MiB (40.4) | 46 MiB |
| team | 60 MB | 42.8 MiB (= solo) | 46 MiB |
| cluster | 60 MB | 51.1 MiB (47.6) | 55 MiB |

Edge and nano exclude the JS plugin set of `./examples/base` through `no_jsvm no_ghupdate no_migratecmd` (about 7 MiB); nano also drops `no_totp no_geo`. They meet the CI budgets (edge 28 MiB, nano 24 MiB) but not the original 28/14 MB design budgets. CI budgets are measured + 2 MiB, rounded up.
