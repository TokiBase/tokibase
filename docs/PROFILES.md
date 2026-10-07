# Build profiles

A profile is a set of Go build tags. Every optional module can be compiled out with `-tags no_<module>`: its real files carry `//go:build !no_<module>` and a `stub.go` (`//go:build no_<module>`) keeps the exported surface used by `tokibase.go`, `tokibase_mcp.go`, `apis` and `cmd` (Register and sinks are no-ops, `Enabled()` is false, `NewCommand` returns a hidden command that fails with "not available in this build").

The tag sets, and the size budgets CI enforces, live in [`profiles.txt`](../profiles.txt) (single source for the Makefile, CI and `profiles_test.go`).

```sh
make solo          # out/toki-solo     everything (default)
make team          # = solo
make cluster       # solo + replica_s3
make edge          # out/toki-edge
make nano          # out/toki-nano
make edge GOOS=linux GOARCH=arm64   # cross-compile
```

| Profile | Tags | Size linux/amd64 | linux/arm64 | Budget |
| --- | --- | --- | --- | --- |
| solo | none | 39.8 MiB | 37.6 MiB | 46 MiB |
| team | none (= solo) | 39.8 MiB | 37.6 MiB | 46 MiB |
| cluster | `replica_s3` | 48.0 MiB | 44.8 MiB | 56 MiB |
| edge | `no_mcp no_passkey no_push no_webhooks no_ui no_adminlock` | 32.2 MiB | 30.3 MiB | 34 MiB |
| nano | edge + `no_replica no_backupcheck no_audit` | 29.1 MiB | 27.4 MiB | 31 MiB |

Sizes: stripped (`-s -w`, `-trimpath`, `CGO_ENABLED=0`) `./examples/base`.

## Modules per profile

| Module | tag | solo/team | cluster | edge | nano |
| --- | --- | --- | --- | --- | --- |
| ruleguard | `no_ruleguard` | yes | yes | yes | yes |
| sessions | `no_sessions` | yes | yes | yes | yes |
| fieldperm | `no_fieldperm` | yes | yes | yes | yes |
| crypto | `no_crypto` | yes | yes | yes | yes |
| computed | `no_computed` | yes | yes | yes | yes |
| timelint | `no_timelint` | yes | yes | yes | yes |
| lockout | `no_lockout` | yes | yes | yes | yes |
| denylog | `no_denylog` | yes | yes | yes | yes |
| tlscheck | `no_tlscheck` | yes | yes | yes | yes |
| jobs | none (see below) | yes | yes | yes | yes |
| audit | `no_audit` | yes | yes | yes | no |
| backupcheck | `no_backupcheck` | yes | yes | yes | no |
| walreplica | `no_replica` (s3 backend: `replica_s3`) | file:// | file:// + s3:// | file:// | no |
| webhooks | `no_webhooks` | yes | yes | no | no |
| push | `no_push` | yes | yes | no | no |
| passkey | `no_passkey` | yes | yes | no | no |
| mcp | `no_mcp` | yes | yes | no | no |
| adminlock | `no_adminlock` | yes | yes | no | no |
| Admin UI | `no_ui` | yes | yes | no | no |

`jobs` has no tag: push, webhooks and computed enqueue work through it, it is small, and `TOKI_JOBS=off` disables it at runtime. Under `no_adminlock` the Admin UI mode switch (`TOKI_ADMIN_UI`) does not exist; edge and nano also drop the UI, so there is nothing to lock.

Any other combination works: tags are independent (CI and `profiles_test.go` build every single tag and all tags together). Removing a module also removes its CLI commands (`toki passkey`, `toki audit`, ...); `toki backup` keeps `create` but loses `list`/`verify`/`verify-all` under `no_backupcheck`.

## What a stub changes at runtime

- Audit sinks of other modules are not wired when `no_audit` is set (no audit log is created).
- `no_lockout`: passkey failure/lock sinks become no-ops.
- The MCP provider wiring (`tokibase_mcp.go`) is only built without `no_mcp`.
- `/api/health` omits replica fields under `no_replica`.

## Size and the JS plugin

`./examples/base` links `plugins/jsvm`, `ghupdate` and `migratecmd`. Measured with a bare `tokibase.New().Start()` main, the same tag sets give edge 24.8 MiB and nano 21.6 MiB (linux/amd64): about 7.4 MiB of the profile sizes above is the plugin set, not tokibase modules. A no-plugin example binary is the way to reach the original 28 MiB (edge) and 14 MiB (nano) design goals.

## Checking

- `go test -run 'TestProfile|TestEachStub' .` builds and vets every profile and every single `no_*` tag (skipped with `-short`).
- CI job `profiles` builds edge and nano for linux/amd64 and linux/arm64, enforces the budgets from `profiles.txt` and vets with the profile tags.
