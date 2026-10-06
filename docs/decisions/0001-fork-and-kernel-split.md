# ADR 0001: Full fork of PocketBase with a kernel/server split

Date: 2026-10-07. Status: accepted.

## Context

PocketBase v0.40.4 has no storage interface: `core.App` exposes `dbx.Builder` and
SQLite-specific SQL is emitted from the filter compiler, schema sync, backups and
helpers (see `docs/PHASE0_AUDIT.md`). Multi-writer scaling, embedded mobile use,
and two-way offline sync cannot be added from outside.

## Decision

- Fork fully (not wrap). Keep the REST/SDK/`pb_data` contract (`docs/COMPAT.md`).
- Introduce `kernel/` with no `net/http`, `os/exec` or UI imports, and a `Store`
  interface; SQLite becomes the first store module, PostgreSQL the second (phase 4).
- Phase 0 is behavior-preserving: move code, add seams, no new features.
- The filter/rule compiler split (parser, AST, SQL emitter) is phase 2.

## Consequences

- The Go package API of `core` changes; only the HTTP contract is promised.
- Upstream security fixes are ported by hand from phase 2 onward.
- Every deviation from PocketBase behavior is recorded in `docs/COMPAT.md`.
