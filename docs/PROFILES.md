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
| solo | none | 42.8 MiB | 40.4 MiB | 46 MiB |
| team | none (= solo) | 42.8 MiB | 40.4 MiB | 46 MiB |
| cluster | `replica_s3` | 51.1 MiB | 47.6 MiB | 55 MiB |
| edge | `no_payments no_mcp no_passkey no_push no_webhooks no_ui no_adminlock no_wasm no_jsvm no_ghupdate no_migratecmd no_roles` | 25.0 MiB | 23.5 MiB | 28 MiB |
| nano | edge + `no_replica no_backupcheck no_audit no_totp no_geo` (edge already has `no_roles`) | 21.7 MiB | 20.4 MiB | 24 MiB |

Sizes: stripped (`-s -w`, `-trimpath`, `CGO_ENABLED=0`) `./examples/base`.  darwin/arm64 solo measures 41.6 MiB.

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
| adminlock | `no_adminlock no_wasm` | yes | yes | no | no |
| Admin UI | `no_ui` | yes | yes | no | no |
| totp | `no_totp` | yes | yes | yes | no |
| nativeauth | `no_nativeauth` | yes | yes | yes | yes |
| geo | `no_geo` | yes | yes | yes | no |
| roles | `no_roles` | yes | yes | no | no |
| payments | `no_payments` (runtime: `TOKI_PAYMENTS=off`) | yes | yes | no | no |
| sync | `no_sync` (runtime: `TOKI_SYNC_ROLE=off`, the default) | yes | yes | yes | yes |
| jsvm plugin (pb_hooks, JS migrations) | `no_jsvm` | yes | yes | no | no |
| migrate command | `no_migratecmd` | yes | yes | no | no |
| ghupdate (`update` command) | `no_ghupdate` | yes | yes | no | no |

`jobs` has no tag: push, webhooks and computed enqueue work through it, it is small, and `TOKI_JOBS=off` disables it at runtime. Under `no_adminlock` the Admin UI mode switch (`TOKI_ADMIN_UI`) does not exist; edge and nano also drop the UI, so there is nothing to lock.

**Tags are for fresh data dirs.** Tags are independent at build time (CI and `profiles_test.go` build every single tag and all tags together), but a binary built with `no_<module>` is NOT safe on a `pb_data` that was used by a build containing that module: the guards of the module (field encryption, field permissions, computed-field write protection, session revocation, lockout, ...) silently disappear while their data stays. Removing a module also removes its CLI commands (`toki passkey`, `toki audit`, ...); `toki backup` keeps `create` but loses `list`/`verify`/`verify-all` under `no_backupcheck`.

## Stubbed module boot guard

Every module registers a marker at init with `kernel.RegisterModuleMarker(name, collections, envs, stubbed)` (some also list data-dir files, for example `ruleguard.json`). The real implementation registers `stubbed=false`, its `no_<module>` stub registers the same names with `stubbed=true`. After a successful bootstrap, for every stubbed marker the binary checks:

- whether one of the owned system collections/tables exists in the database (for example `_crypto_fields`, `_crypto_keys`, `_field_rules`, `_computed_fields`, `_sessions`, `_lockout`, `_passkeys`, `_webhooks`, `_wasm_kv`, `_changes`),
- whether one of the owned env vars is set (for example `TOKI_CRYPTO_MASTER_KEY`, `TOKI_LOCKOUT`, `TOKI_REPLICA_URL`, `TOKI_SYNC_ROLE`, `TOKI_ADMIN_UI`; the values `off`, `0`, `false`, `no` count as unset, except for `TOKI_ADMIN_UI` where `off` is a restrictive mode).

If anything is found the process refuses to start with an error listing the module and each collection or env var. Fix it by rebuilding without the tag, or by removing the data and env. `TOKI_ALLOW_STUBBED_MODULES=1` lets the process start anyway; it then logs one ERROR line per finding at every boot and the guards of those modules are OFF. Edge and nano are meant for new data dirs; on an existing database they start only with that override.

## What a stub changes at runtime

- Audit sinks of other modules are not wired when `no_audit` is set (no audit log is created).
- `no_lockout`: passkey failure/lock sinks become no-ops.
- The MCP provider wiring (`tokibase_mcp.go`) is only built without `no_mcp`.
- `/api/health` omits replica fields under `no_replica`.

## Plugins (`examples/base`)

`./examples/base` links three optional plugins, each behind its own tag through small tagged files (`plugins_<name>.go` with `//go:build !no_<name>`, `plugins_<name>_stub.go` with the inverse), so `main.go` stays the same in every build:

| Tag | Plugin | Under the tag |
| --- | --- | --- |
| `no_jsvm` | `plugins/jsvm` | no `pb_hooks`/JS migrations; the `--hooks*` flags are still accepted and ignored |
| `no_migratecmd` | `plugins/migratecmd` | no `migrate` command, no automigrate; the `--migrationsDir`/`--automigrate` flags are still accepted and ignored |
| `no_ghupdate` | `plugins/ghupdate` | no `update` command |

Without any of these tags (solo, team, cluster) the binary, its flags and its behavior are unchanged. The three plugins weigh about 7 MiB together; with them removed, edge reaches 25.0 MiB and nano 21.7 MiB (linux/amd64), under the CI budgets of this phase (edge 28 MiB, nano 24 MiB, measured + 2 MiB). nano also drops `totp` and `geo`. The original 28 MiB (edge) and 14 MiB (nano) design goals of the architecture doc are not met by `./examples/base`.

Like the module tags, a binary without `no_migratecmd`/`no_jsvm` removed features: databases migrated by JS migrations (`pb_migrations/*.js`) are not migrated by a build with `no_jsvm`.

## Checking

- `go test -run 'TestProfile|TestEachStub' .` builds and vets every profile and every single `no_*` tag, and `TestExamplePluginTags` builds and vets `./examples/base` with each plugin tag (all skipped with `-short`).
- CI job `profiles` builds edge and nano for linux/amd64 and linux/arm64, enforces the budgets from `profiles.txt` and vets with the profile tags.
