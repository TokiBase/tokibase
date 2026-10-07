# TokiBase

One binary, from phone to cluster. Online or offline. PocketBase-compatible.

TokiBase is a full fork of [PocketBase](https://github.com/pocketbase/pocketbase) (v0.40.4)
rebuilt around a small HTTP-free kernel plus removable modules, shipped as five profiles:

| Profile | Shape | For |
| --- | --- | --- |
| `nano` | Go library, Android AAR, iOS XCFramework | Embedded in apps, fully offline, two-way sync when online |
| `edge` | Single binary on Pi / mini PC | Parking gates, kiosks, signage, POS |
| `solo` | Single binary on a 1 GB VPS | Drop-in PocketBase replacement with real HA |
| `team` | Primary + read nodes + workers | Production teams |
| `cluster` | N stateless nodes + PostgreSQL + NATS | Multi-tenant SaaS |

Status: **phase 1** (phase 0 done: kernel/server split, sqlite store module, compatibility e2e). Not ready for production use.
See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and [docs/COMPAT.md](docs/COMPAT.md).

## Modules

- `modules/ruleguard`: public (`""`) API rules must be allowlisted in `pb_data/ruleguard.json`, otherwise they are warned about at boot and in `toki rule lint` ([docs/modules/ruleguard.md](docs/modules/ruleguard.md)).
- `modules/audit`: append-only, hash-chained audit log of privileged and schema-changing actions, `toki audit tail|verify|export` ([docs/modules/audit.md](docs/modules/audit.md)). Disable with `TOKI_AUDIT=off`.
- `modules/adminlock`: `TOKI_ADMIN_UI=on|readonly|off` serves the Admin UI read-only (schema, settings and superuser changes from the UI get 403) or not at all ([docs/modules/adminlock.md](docs/modules/adminlock.md)).
- `modules/lockout`: progressive per-identity lockout of failed password/OTP authentication, independent of client IP, `toki lockout list|unlock|clear` ([docs/modules/lockout.md](docs/modules/lockout.md)). Disable with `TOKI_LOCKOUT=off`.
- `modules/sessions`: server-side sessions (`sid` JWT claim), revoke per device or all, revoke on password/email change, optional refresh rotation, `toki sessions list|revoke|revoke-all|purge` ([docs/modules/sessions.md](docs/modules/sessions.md)). Disable with `TOKI_SESSIONS=off`.
- `modules/jobs`: durable job queue on `auxiliary.db` (retry with backoff, dead-letter, cron, `--role worker`), consumer interface `kernel.Jobs(app)`, `toki jobs list|retry|purge|stats` ([docs/modules/jobs.md](docs/modules/jobs.md)). Disable with `TOKI_JOBS=off`.
- `modules/tlscheck`: warns at boot (`TOKI_TLS_CHECK=warn|strict|off`) when the server listens on plain HTTP on a non-loopback address with no trusted proxy header configured ([docs/modules/tlscheck.md](docs/modules/tlscheck.md)).
- `modules/timelint`: date values submitted without a time zone are warned about (`TOKI_TIMELINT=warn`, default) or rejected with `validation_invalid_timezone` (`strict`); `toki time lint` scans stored values ([docs/modules/timelint.md](docs/modules/timelint.md)).
- `modules/denylog`: every 401/403/429 response gets a structured Warn log (`toki.deny=true`: status, path, ip, auth_kind, auth_id, collection, reason, rule_kind, rate_limited), sampled per minute; `toki deny tail` ([docs/modules/denylog.md](docs/modules/denylog.md)). Disable with `TOKI_DENYLOG=off`.
- `modules/backupcheck`: every created backup is restored to a temp dir and verified (`PRAGMA integrity_check`, counts, sampled files); `toki backup verify latest` ([docs/modules/backupcheck.md](docs/modules/backupcheck.md)).
- `modules/walreplica` (s3 backend needs `-tags replica_s3`): set `TOKI_REPLICA_URL` (`file://` or `s3://`) to continuously replicate `data.db` and `auxiliary.db` (Litestream embedded, RPO of seconds) and restore with `toki replica restore` or fail over with `toki replica promote` (one replicator per URL is guarded by a lease; drill: `tests/e2e/failover.sh`) ([docs/modules/walreplica.md](docs/modules/walreplica.md)).

## Build

```sh
go build ./examples/base   # requires Go 1.27 (GOTOOLCHAIN=auto downloads it)
```

## License

MIT. TokiBase contains code from PocketBase, Copyright (c) 2022-present Gani Georgiev,
see [LICENSE.md](LICENSE.md) and [NOTICE.md](NOTICE.md).
