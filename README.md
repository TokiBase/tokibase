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
<<<<<<< HEAD
- `modules/adminlock`: `TOKI_ADMIN_UI=on|readonly|off` serves the Admin UI read-only (schema, settings and superuser changes from the UI get 403) or not at all ([docs/modules/adminlock.md](docs/modules/adminlock.md)).
=======
- `modules/lockout`: progressive per-identity lockout of failed password/OTP authentication, independent of client IP, `toki lockout list|unlock|clear` ([docs/modules/lockout.md](docs/modules/lockout.md)). Disable with `TOKI_LOCKOUT=off`.
>>>>>>> 11e88c00 (Add lockout module: progressive per-identity auth lockout)
- `modules/backupcheck`: every created backup is restored to a temp dir and verified (`PRAGMA integrity_check`, counts, sampled files); `toki backup verify latest` ([docs/modules/backupcheck.md](docs/modules/backupcheck.md)).
- `modules/walreplica` (s3 backend needs `-tags replica_s3`): set `TOKI_REPLICA_URL` (`file://` or `s3://`) to continuously replicate `data.db` and `auxiliary.db` (Litestream embedded, RPO of seconds) and restore with `toki replica restore` or fail over with `toki replica promote` (one replicator per URL is guarded by a lease; drill: `tests/e2e/failover.sh`) ([docs/modules/walreplica.md](docs/modules/walreplica.md)).

## Build

```sh
go build ./examples/base   # requires Go 1.27 (GOTOOLCHAIN=auto downloads it)
```

## License

MIT. TokiBase contains code from PocketBase, Copyright (c) 2022-present Gani Georgiev,
see [LICENSE.md](LICENSE.md) and [NOTICE.md](NOTICE.md).
