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

1. `kernel` imports nothing outside stdlib and the SQLite driver.
2. Modules import `kernel` only; modules never import each other.
3. `server` imports `kernel` and modules.

## Phase 0 (current)

Goal: profile `solo` behaves identically to PocketBase v0.40.4.

- [x] Fork v0.40.4, module path `github.com/tokibase/tokibase`, root package `tokibase`.
- [ ] Map every place SQLite leaks above the store layer (`docs/PHASE0_AUDIT.md`).
- [x] Move `core/` pieces into `kernel/`, no behavior change (`docs/PHASE0_KERNEL_SPLIT.md`). `core` stays as the server facing compatibility package (type aliases + request hooks). Store interface is still to come.
- [x] `depguard` rule for `kernel/**` in `golangci.yml` and `kernel/deps_test.go` (runs in CI with `go test`).
- [ ] CI job that runs `golangci-lint`.
- [ ] CI: build matrix (linux/darwin/windows x amd64/arm64), `-s -w`, size budgets.
- [ ] CI: official JS SDK and Dart SDK test suites against the binary.
- [ ] `toki import pb_data/` smoke test with a real PocketBase data directory.

Exit gate: 100% SDK suite pass, kernel has no `net/http` import, all builds under budget.

## Size budgets (phase 0 baseline, stripped)

| Profile | Budget |
| --- | --- |
| nano | 14 MB per arch |
| edge | 28 MB |
| solo | 45 MB |
| team / cluster | 60 MB |
