# Phase 0: SQLite store module inventory

Goal: lift the SQLite driver and connection concerns out of `kernel/` into `modules/store/sqlite/` behind `kernel.DBOpener` / `kernel.DBConn` (`kernel/store.go`). Behavior preserving: same pragmas, pool sizes, retry intervals and SQL. `dbx` stays the query layer (filter compiler is phase 2).

## Interface (`kernel/store.go`)

- `DBOpener.Open(ctx, DBConfig) (DBConn, error)`; `DBConfig{Path, MaxOpenConns, MaxIdleConns, OptimizeOnMaintain}`.
- `DBConn`: `Concurrent()`, `Nonconcurrent()` (`*dbx.DB`), `Close`, `Maintain`, `Optimize`, `Checkpoint`, `Vacuum`, `VacuumInto`, `ErrorKind`, `LockRetry`, `ExecLockRetry`, `Route`.
- `ErrKind`: `ErrKindOther`, `ErrKindNotFound`, `ErrKindUnique`, `ErrKindLocked`, `ErrKindConstraint`.
- `BaseAppConfig.DBOpener` (new) and `BaseAppConfig.DBConnect` (legacy seam, adapted by `sqlite.NewOpenerFunc`). `core.NewBaseApp` wires the default.

## Inventory and destination

| Before | After |
| --- | --- |
| `kernel/db_connect.go` (driver import, pragmas DSN) | `modules/store/sqlite/connect.go` (`DefaultConnect`) |
| `kernel/db_connect_nodefaultdriver.go` | `modules/store/sqlite/connect_nodefaultdriver.go` |
| `kernel/db_builder.go` (`dualDBBuilder`) | `modules/store/sqlite/builder.go`, exposed as `DBConn.Route` |
| `kernel/db_retry.go` (`execLockRetry`, `baseLockRetry`, intervals, "database/table is locked" match) | `modules/store/sqlite/retry.go` + `errors.go`; kernel calls `DBConn.LockRetry/ExecLockRetry` (`BaseApp.lockRetry`, `execLockRetry` in `kernel/db.go`); `kernel.DefaultMaxLockRetries` = 12 |
| `kernel/db_retry_test.go` | `modules/store/sqlite/retry_test.go` |
| `kernel/base.go` `initDataDB/initAuxDB` (pool: data 120/15 default, aux 20/3, nonconcurrent 1/1, idle 3m) | `sqlite.Opener.Open` (`store.go`); kernel only builds `DBConfig`, dev query log funcs stay on the `*dbx.DB` |
| `kernel/base.go` cron `__pbDBOptimize__` (`wal_checkpoint(TRUNCATE)` x2, `optimize` main only) | `DBConn.Maintain`; `OptimizeOnMaintain` is true for data, false for aux (same as before). Order is now main checkpoint, main optimize, aux checkpoint |
| `kernel/base.go` `ClearBootstrap` closing handles | closes `DBConn`s first (handles are then nil) |
| `kernel/db_table.go` `VACUUM` (`Vacuum`, `AuxVacuum`) | `DBConn.Vacuum` |
| `kernel/backup_create.go` `VACUUM INTO`, `wal_checkpoint` | `DBConn.VacuumInto`, `DBConn.Checkpoint` (`createZip` now takes the `*BaseApp`) |
| `kernel/collection_record_table_sync.go` `PRAGMA optimize` | `DBConn.Optimize` |
| `kernel/validators/db.go` "unique constraint failed" match | detector hook `validators.SetUniqueErrorDetector`, registered by the sqlite module (`IsUniqueError`) |
| `modernc_versions_check.go` (root) | `modules/store/sqlite/versions_check.go` (`CheckModerncDeps`); root keeps `ModerncDepsCheckHookId` and the hook in `tokibase.go` |

## Not moved (and why)

- `kernel/db_table.go` `PRAGMA_TABLE_INFO`, `sqlite_master` queries (`TableColumns`, `TableInfo`, `TableIndexes`, `HasTable`), `collection_validate.go` and `collection_record_table_sync.go` schema SQL, `json_*` rewrites: schema/query SQL, not connection concerns. They need the typed `Tx` methods of the audit (`TableColumns`, `Indexes`, `SyncRecordSchema`), phase 2/4.
- `sql.ErrNoRows` checks (~35 places in kernel, apis, forms): stdlib sentinel and public contract; `ErrKindNotFound` classifies it but call sites stay.
- `validators.NormalizeUniqueIndexError` still parses the SQLite message layout (`table.column`) after detection; the parsing moves with the typed error work.
- `createTxApp`/`runInTransaction` still type switch on `*dbx.Tx`/`*dbx.DB` (dbx stays).
- Internal kernel tests that boot an app (`log_printer_test.go`, `system_alert_test.go`) use `kernel/testopener_test.go` because the module imports the kernel (import cycle).

## Enforcement

`kernel/deps_test.go` asserts `modernc.org/sqlite` is not in `go list -deps ./kernel`; `golangci.yml` depguard denies `modernc.org/sqlite` and `modules` for `kernel/**`.
