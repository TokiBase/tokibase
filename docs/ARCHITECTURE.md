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

## Size budgets (stripped; CI enforces solo 36 MiB, no_ui 33 MiB, solo+replica_s3 45 MiB)

| Profile | Budget |
| --- | --- |
| nano | 14 MB per arch |
| edge | 28 MB |
| solo | 45 MB |
| team / cluster | 60 MB |
