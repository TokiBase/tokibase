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

| Profile | Tags | linux/amd64 | linux/arm64 | Budget | Headroom (amd64) |
| --- | --- | --- | --- | --- | --- |
| solo | none | 45.70 MiB | 43.00 MiB | 48 MiB | 5.0% |
| team | none (= solo) | 45.70 MiB | 43.00 MiB | 48 MiB | 5.0% |
| cluster | `replica_s3` | 53.91 MiB | 50.19 MiB | 57 MiB | 5.7% |
| edge | `no_payments no_mcp no_passkey no_push no_webhooks no_ui no_adminlock no_wasm no_jsvm no_ghupdate no_migratecmd no_roles` | 27.44 MiB | 25.63 MiB | 30 MiB | 9.3% |
| nano | edge + `no_replica no_backupcheck no_audit no_totp no_geo no_thumbs no_oauth2 no_s3fs no_printer no_scanner no_kiosk no_devicecert` (edge already has `no_roles`) | 22.29 MiB | 20.94 MiB | 24 MiB | 7.7% |

This is the one authoritative table. The tags and budgets live in [`profiles.txt`](../profiles.txt); `ci.yml` (jobs `build`, `profiles`) reads the budgets from it, `docs/ARCHITECTURE.md` and the README only link here.

**How it was measured** (edge PR 8, 2026-10-09, after rebase onto main with #86/#87; `origin/main` after sync PR9 and devicecert QC (#86, #87), Go from `go.mod`, build VM `tokibuild`): `make <profile> [GOOS=linux GOARCH=<arch>]`, i.e. `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" ./examples/base`; exact bytes amd64: edge 28,770,464, nano 23,376,032, solo/team 47,915,168, cluster 56,524,960 (arm64: 26,869,920, 21,954,720, 45,088,928, 52,625,568). `no_ui` (an extra CI check, not a profile) is 44,859,552 B (42.78 MiB) with a 45 MiB budget. The older numbers in this document (edge 25.5, nano 21.1, solo 42.8) were taken before sync PR1-PR8 and the four edge modules landed.

**Budget rule:** measured linux/amd64 + about 5%, rounded up to a whole MiB (the CI runners can differ from the VM by a few hundred KiB; cluster needed a raise once for that reason). Edge gets 30 MiB instead of the 29 MiB the rule gives: sync PR9 (crypto key export) and PR10 (embed/mobile facade, client conditions) are still to land and each costs 100-250 KiB, and the edge modules plan (`docs/EDGE_MODULES_PLAN.md` section 5) already accepted that edge does not meet the original 28 MB goal; 30 MiB keeps about 2.6 MiB for those and for one or two review fixes. If a later PR needs more than that, raise the budget in the PR and add its row to the size log below. nano gets 24 (rule: 23.4 rounded up), solo/team 48 (47.9), cluster 57 (56.5).

**Why the server profiles keep the hardware modules.** `solo`, `team` and `cluster` keep printer, scanner, kiosk and devicecert (no `no_printer`, `no_scanner`, `no_kiosk`, `no_devicecert`), as the plan (section 1) decided. All four are opt-in at runtime (`TOKI_PRINTER`, `TOKI_SCANNER`, `TOKI_KIOSK`, `TOKI_DEVICECERT`, default off: no collections, no routes, no goroutines), together they cost about 0.7 MiB, and a server host has real uses for them: the hub runs `toki devicecert issue|revoke|rotate-ca` (the CA lives on the hub), a solo host can have a USB printer or a scanner attached, and `/api/print` and `/api/scan` are plain HTTP APIs that a back-office server can serve for a nearby gate. Only `nano` (embedded in apps, no hardware, no TLS listener) drops them.

### Edge modules size log

Each edge module PR recorded its measured cost here (linux/amd64, stripped, `make edge`; the budget was 28 MiB then and is 30 MiB since PR 8).

| PR | What | Edge before | Edge after | Delta |
| --- | --- | --- | --- | --- |
| 0 | kernel providers (`NodeIdentity`, `SyncStatus`, `DeviceCerts`) | 27,623,584 B (26.34 MiB, includes PR 0) | same | small interfaces, not measurable |
| 1 | `internal/devio` + `internal/escpos` (all entry points forced reachable with a temporary probe, nothing links them yet) | 27,623,584 B | 27,742,368 B (26.46 MiB) | +118,784 B (116 KiB); linux/arm64 with probe 25,886,880 B (24.69 MiB) |
| 2 | `modules/printer` (collections, `print.send`, `/api/print*`, `toki print`; compiled in, runtime opt-in) | 27,660,448 B (26.38 MiB, origin/main) | 27,914,400 B (26.62 MiB) | +253,952 B (248 KiB); budget 28 MiB leaves 1.38 MiB |
| 3 | `modules/scanner` (serial, evdev, web wedge, `@scan`, `/api/scan*`, `toki scan`; `wedge.js` embedded) | 27,914,400 B (origin/main with printer) | 28,061,856 B (26.76 MiB) | +147,456 B (144 KiB) |
| 4 | `modules/kiosk` (pairing, device sessions, `/api/kiosk*`, `kiosk.js` embedded, `toki kiosk`; plus the `sessions` revoke seam and `apis.HealthExtra`) | 28,131,488 B (26.83 MiB, origin/main 4db9ab73) | 28,225,696 B (26.92 MiB) | +94,208 B (92 KiB); budget 28 MiB leaves 1.08 MiB |
| 6 | `modules/devicecert` (hub CA, `_device_certs`, `/api/sync/devcert`, leaf renewal, TLS listener, `toki devicecert`; stdlib crypto only) | 28,225,696 B (origin/main with kiosk) | 28,360,864 B (27.05 MiB) | +135,168 B (132 KiB); budget 28 MiB leaves 0.95 MiB |
| 7 | `modules/devicecert` PR2 (client certs, mTLS route scope, deny list, `/api/device/*`, `rotate-ca`, EKU split, bundle file; stdlib only, `.p12` through the `openssl` binary) | 28,360,864 B (PR 6) | 28,487,840 B (27.17 MiB) | +126,976 B (124 KiB); budget 28 MiB leaves 0.83 MiB |
| 8 | integration (no code in the binary; re-measured after sync PR1-PR8 and the other merges) | 28,487,840 B (PR 7) | 28,770,464 B (27.44 MiB) | +225,280 B from the other merged work; budget 30 MiB leaves 2.56 MiB |

`go-qrcode` (raster QR fallback) is already linked into edge through `modules/totp`, so it adds nothing; a hand written encoder was not needed. `x/text/encoding/charmap` is not linked in edge, so the codepages are hand tables (about 2 KiB). The measured baseline at PR 1 was 26.34 MiB; the current number is in the table at the top.
### Sync PR10 size log (nano and edge, linux/amd64, stripped, `make nano` / `make edge`)

| What | Before (origin/main 5d04b774) | After | Delta | Budget |
| --- | --- | --- | --- | --- |
| nano `examples/base` | 23,330,976 B (22.25 MiB) | 23,363,744 B (22.28 MiB) | +32,768 B (32 KiB) | 23 MiB (24,117,248 B), 0.72 MiB left |
| edge `examples/base` | 28,713,120 B (27.38 MiB) | 28,749,984 B (27.42 MiB) | +36,864 B (36 KiB) | 28 MiB (29,360,128 B), 0.58 MiB left |

PR10 adds the `embed` sync facade, the client conditions and the `Scheduler` seam; no new dependency. The `mobile` AAR was not rebuilt on the VM (no Android SDK there, see [EMBED.md](EMBED.md#android-aar-on-linux)); the wrappers add only methods of `Handle` and import nothing new, so the Android `libgojni.so` grows by about the nano delta.

`go-qrcode` (raster QR fallback) is already linked into edge through `modules/totp`, so it adds nothing; a hand written encoder was not needed. `x/text/encoding/charmap` is not linked in edge, so the codepages are hand tables (about 2 KiB). The 25.5 MiB in the table above predates later PRs; the measured baseline at PR 1 is 26.34 MiB.

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
| scanner (opt in: `TOKI_SCANNER=on`) | `no_scanner` | yes | yes | yes | no |
| kiosk (opt in: `TOKI_KIOSK=on`) | `no_kiosk` | yes | yes | yes | no |
| devicecert (opt in: `TOKI_DEVICECERT=on`) | `no_devicecert` | yes | yes | yes | no |
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
| printer | `no_printer` (runtime: opt-in `TOKI_PRINTER=on`) | yes | yes | yes | no |
| jsvm plugin (pb_hooks, JS migrations) | `no_jsvm` | yes | yes | no | no |
| migrate command | `no_migratecmd` | yes | yes | no | no |
| ghupdate (`update` command) | `no_ghupdate` | yes | yes | no | no |

`jobs` has no tag: push, webhooks and computed enqueue work through it, it is small, and `TOKI_JOBS=off` disables it at runtime. Under `no_adminlock` the Admin UI mode switch (`TOKI_ADMIN_UI`) does not exist; edge and nano also drop the UI, so there is nothing to lock.

**Tags are for fresh data dirs.** Tags are independent at build time (CI and `profiles_test.go` build every single tag and all tags together), but a binary built with `no_<module>` is NOT safe on a `pb_data` that was used by a build containing that module: the guards of the module (field encryption, field permissions, computed-field write protection, session revocation, lockout, ...) silently disappear while their data stays. Removing a module also removes its CLI commands (`toki passkey`, `toki audit`, ...); `toki backup` keeps `create` but loses `list`/`verify`/`verify-all` under `no_backupcheck`.

## Sizing for a 1 GB VPS

`solo` fits a 1 GB host only with a bounded SQLite pool. Measured on the FGR dataset (215 collections, 56k records, `data.db` 55 MB) under 200 concurrent list requests with `sort`, `filter` and `expand`: each pooled read connection adds about 12 MB of RSS (page cache plus the sort buffers of its running query), the idle process is about 80-130 MB. The queries are CPU bound, so a larger pool only queues work inside SQLite. Details and numbers: [CAPACITY.md](CAPACITY.md).

| Env | Default | What it does | 1 GB VPS (1-2 vCPU) |
| --- | --- | --- | --- |
| `TOKI_DB_MAX_CONNS` | `2 x CPUs`, between 16 and 120 | size of the `data.db` read pool | `8` (about 100 MB at full load, extrapolated from 12 MB per connection, not measured on a 1 GB host) |
| `TOKI_DB_CACHE_KB` | `8192` | `cache_size` of every connection, in KiB (was 32000) | `4096` |
| `TOKI_DB_TEMP_STORE` | `memory` | `file` keeps big sorts out of RAM; needs a writable `SQLITE_TMPDIR`/`TMPDIR` or `/tmp` | `memory` |
| `TOKI_DB_HEAP_MB` | `0` (off) | SQLite soft heap limit for the process | leave off (did not lower RSS in the read test) |
| `TOKI_DB_MMAP_MB` | `0` (off) | `mmap_size` of every connection | leave off |
| `TOKI_WAL_MAX_MB` | `256` | WAL size above which the minutely maintenance runs a truncating checkpoint (`0` = never) | `64` |
| `TOKI_LOGS_MAX_MB` | `512` | cap of the live size of `auxiliary.db`; the oldest request logs are pruned above it (`0` = off) | `128` |
| `TOKI_LOGS_SAMPLE_OK` | `1` | keep 1 of N successful `GET` request logs (errors and writes are always logged) | `10` |

Also set `GOMEMLIMIT` (for example `GOMEMLIMIT=400MiB`) so the Go heap collects earlier. It does not count the memory the SQLite engine allocates (about 12 MB per busy connection), which is what `TOKI_DB_MAX_CONNS` bounds. Example systemd drop-in:

```ini
[Service]
Environment=TOKI_DB_MAX_CONNS=8 TOKI_DB_CACHE_KB=4096 TOKI_WAL_MAX_MB=64 TOKI_LOGS_MAX_MB=128 TOKI_LOGS_SAMPLE_OK=10 GOMEMLIMIT=400MiB
MemoryMax=900M
```

Every pooled query waits for a free connection, so a pool smaller than the number of long reads you run in parallel (exports, big lists) adds queueing; watch `data.db.data.pool.waitCount` in `GET /api/health` (superuser) and raise `TOKI_DB_MAX_CONNS` when it grows steadily.

## Library tags (not modules)

Three more tags compile out library code in `tools/*`. They are not modules: no marker, no system collection, no boot guard, so data written with them stays valid (only the feature is missing at run time). nano uses all three; solo, team, cluster and edge use none.

| Tag | Removes | Behaviour under the tag |
| --- | --- | --- |
| `no_thumbs` | `disintegration/imaging`, `x/image` (thumbnail generation) | `?thumb=` requests fail the thumb step and the original file is served; no image resizing |
| `no_oauth2` | the 32 OAuth2/OIDC provider implementations in `tools/auth` (`Providers` stays empty) | no OAuth2 login or provider config (settings validation rejects any provider name); `nativeauth` (Google/Apple id_token) is not affected |
| `no_s3fs` | the S3 file system driver (`fshttp.NewS3`) | S3 storage and S3 backups return an error; local file system only |

`TestLibraryTags` builds and vets the tree with each of them.

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

Without any of these tags (solo, team, cluster) the binary, its flags and its behavior are unchanged. The three plugins weigh about 7 MiB together; with them removed, edge and nano are the sizes in the table at the top. nano also drops `totp`, `geo` and the three library tags above (-1.0 MiB together). The original 28 MiB (edge) and 14 MiB (nano) design goals of the architecture doc are not met by `./examples/base`; see [NANO_SIZE.md](NANO_SIZE.md) for where the bytes are and what it would take.

Like the module tags, a binary without `no_migratecmd`/`no_jsvm` removed features: databases migrated by JS migrations (`pb_migrations/*.js`) are not migrated by a build with `no_jsvm`.

## Checking

- `go test -run 'TestProfile|TestEachStub' .` builds and vets every profile and every single `no_*` tag, and `TestExamplePluginTags` builds and vets `./examples/base` with each plugin tag (all skipped with `-short`).
- CI job `profiles` builds edge and nano for linux/amd64 and linux/arm64, enforces the budgets from `profiles.txt` and vets with the profile tags. CI job `e2e-edge-gate` runs `tests/e2e/edge-gate.sh` on a binary built from the edge tags ([EDGE_GATE.md](EDGE_GATE.md)).
