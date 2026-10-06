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

Status: **phase 0** (fork, rename, kernel/server split, compatibility CI). Not ready for use.
See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and [docs/COMPAT.md](docs/COMPAT.md).

## Build

```sh
go build ./examples/base   # requires Go 1.27 (GOTOOLCHAIN=auto downloads it)
```

## License

MIT. TokiBase contains code from PocketBase, Copyright (c) 2022-present Gani Georgiev,
see [LICENSE.md](LICENSE.md) and [NOTICE.md](NOTICE.md).
