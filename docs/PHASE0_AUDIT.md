# Phase 0 audit: SQLite leaks above the store layer

Scope: Go sources of `github.com/tokibase/tokibase` (PocketBase v0.40.4 fork), non-test files only. Method: grep over `core`, `tools`, `migrations`, `apis`, `cmd`, `plugins`, `forms`, `mails` (259 files, excluding `tools/auth`, `tools/filesystem`, `plugins/jsvm/internal` for SQL searches), plus `go list` for the import graph. Read-only; no Go code changed.

Line numbers refer to the tree at audit time and will drift.

## 0. Headline numbers

| Metric | Value |
| --- | --- |
| Non-test files in `core/` (top level) | 71 |
| Non-test files in `core/` importing `github.com/pocketbase/dbx` | 31 |
| Non-test files in `core/` importing `net/http` directly | 2 (`events.go`, `event_request_batch.go`) |
| Non-test files in `core/` importing `tools/router` (transitively `net/http`) | 3 (`event_request.go`, `events.go`, `record_model_superusers.go`) |
| `dbx.NewExp` / `dbx.HashExp` / `NewQuery(` in `core/` | 28 / 44 / 23 |
| Same, whole repo (non-test) | 47 / 57 / 31 |
| `RunInTransaction` references in `core/` | 38 |
| Files using `[[col]]` / `{{table}}` quoting (whole repo) | 25 files, ~90 lines |
| Migration files | 8 (`migrations/`) |
| Hits for `IFNULL`, `RETURNING`, `ON CONFLICT`, `INSERT OR`, `sqlite_sequence`, `GLOB`, `json_group_array`, `datetime(` | 0 in non-test code |

Key structural fact: the SQLite dependency is not hidden behind an interface. `core.App` exposes `dbx.Builder` directly (`DB()`, `ConcurrentDB()`, `NonconcurrentDB()`, `AuxDB()`, `AuxConcurrentDB()`, `AuxNonconcurrentDB()`), and `apis`, `forms`, `plugins/jsvm`, `plugins/migratecmd`, `migrations` all write queries against it. A Store interface therefore changes the public App contract, not only `core` internals.

## 1. SQLite-specific SQL

### 1.1 JSON functions

| File:line | Construct | Purpose |
| --- | --- | --- |
| `tools/dbutils/json.go:14` | `json_each(CASE WHEN iif(json_valid(c), json_type(c)='array', FALSE) THEN c ELSE json_array(c) END)` | `JSONEach(col)`: iterate multi-value fields (relation, select, file) as rows |
| `tools/dbutils/json.go:29` | `json_array_length(CASE ... json_array() ...)` | `JSONArrayLength(col)`: `:length` modifier |
| `tools/dbutils/json.go:44` | `JSON_EXTRACT(c,'$path')` with fallback `JSON_EXTRACT(json_object('pb', c), '$.pb...')` | `JSONExtract(col, path)`: json field path access, also wraps non-JSON columns |
| `core/record_field_resolver_runner.go:343,362` | `json_each({:placeholder})` | Joins over a literal JSON array bound as param (multi-value request/body fields) |
| `core/record_field_resolver_runner.go:481,485,838,840` | `dbutils.JSONExtract(...)` | JSON field subpaths, top-level primitive normalisation |
| `core/record_field_resolver_runner.go:574,620,676,718,764,769,782,800` | `dbutils.JSONEach` / `JSONArrayLength` | Back-relations (`coll_via_field`), nested relation expansion, `:length` |
| `core/collection_record_table_sync.go:231-233` | `json_valid`, `json_type == 'array'`, `json_array(col)` | Column type change single -> multi (select/relation/file): wraps scalar into JSON array |
| `core/collection_record_table_sync.go:258-259` | `json_extract(col, '$[#-1]')` | Column type change multi -> single: takes last array element (SQLite-only `#-1` path) |

### 1.2 Quoting, identifiers, DDL helpers

| File:line | Construct | Purpose |
| --- | --- | --- |
| whole repo (25 files) | `[[col]]`, `{{table}}` | dbx quoting; dbx translates per driver. Safe if dbx driver layer is replaced, but the compiler hard-codes the tokens in strings (top files: `migrations/1640988000_init.go` 19, `core/record_field_resolver_runner.go` 17, `core/collection_record_table_sync.go` 11, `migrations/1640988000_aux_init.go` 8) |
| `core/field.go:230` | reserved name `_rowid_` | Field-name blacklist |
| `tools/search/sort.go:34` | `[[_rowid_]] ASC/DESC` | `@rowid` sort token |
| `tools/search/provider.go:265` | prefix `_rowid_` with first FROM table | Count/sort over implicit rowid |
| `apis/record_crud.go:84` | `CountCol("_rowid_")` | List total uses implicit rowid (breaks for views without rowid and for non-SQLite stores) |
| `core/field_text.go:167` | `TEXT PRIMARY KEY DEFAULT ('r'\|\|lower(hex(randomblob(7)))) NOT NULL` | Default id generator as DB default expression |
| `core/field_text.go:209` | `id = {:id} COLLATE NOCASE` | Custom-id uniqueness check, case-insensitive |
| `core/record_query.go:565` | `email = {:email} COLLATE NOCASE` | Auth email lookup (relies on unique index with NOCASE) |
| `apis/record_auth_with_password.go:151` | `field = {:identity} COLLATE NOCASE` | Password login identity lookup |
| `apis/record_auth_with_oauth2.go:243` | `username = {:username} COLLATE NOCASE` | OAuth2 username collision check |
| `migrations/1717233556_v0.23_migrate.go:498` | `CREATE UNIQUE INDEX ... (username COLLATE NOCASE)` | Auth collection username index |
| `tools/dbutils/index.go:100` | `" COLLATE "` | Index builder/parser preserves per-column collation |
| `core/collection_query.go:280` | `CAST([[id]] as TEXT)` | View query normalisation (id forced to TEXT) |
| `core/collection_validate.go:566` | `FROM sqlite_master WHERE type='index' ... LOWER(tbl_name)` | Index-name uniqueness across collections |

### 1.3 Pattern matching and expressions in the filter compiler

| File:line | Construct | Purpose |
| --- | --- | --- |
| `tools/search/filter.go:196,198,203,205` | `LIKE ... ESCAPE '\'`, `NOT LIKE` | `~` / `!~` operators; `wrapLikeParams` escapes `%`, `_`, `\` |
| `tools/search/filter.go:334-415` | `COALESCE(a,'') op COALESCE(b,'')` | Null-safe `=`/`!=` (reasoned around SQLite seek vs scan) |
| `core/record_field_resolver_runner.go:826` | `col = TRUE` | Bool literal (SQLite 3.23+) in resolver output |
| `tools/search/token_functions.go:84-175` | `strftime(fmt, time, mods...)` | Filter function `strftime()` is a direct passthrough of SQLite `strftime` (max 10 args, modifiers allowed) |
| `core/log_query.go:42` | `strftime('%Y-%m-%d %H:00:00', created)` | Logs stats grouped by hour |
| `migrations/1640988000_aux_init.go:21` | Index on `strftime('%Y-%m-%d %H:00:00', created)` | Expression index for the stats query |
| `migrations/1640988000_*.go` | `DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ'))` | created/updated defaults; dates are TEXT in a fixed format and compared lexicographically |

### 1.4 PRAGMA, VACUUM, WAL, meta tables

| File:line | Construct | Purpose |
| --- | --- | --- |
| `core/db_connect.go:14` | `busy_timeout(10000)`, `journal_mode(WAL)`, `journal_size_limit(200000000)`, `synchronous(NORMAL)`, `foreign_keys(ON)`, `temp_store(MEMORY)`, `cache_size(-32000)`, `_defensive=1` | Connection string pragmas (modernc DSN syntax) |
| `core/db_table.go:14,37` | `PRAGMA_TABLE_INFO({:tableName})` | `TableColumns`, `TableInfo`; `TableInfoRow` mirrors SQLite pragma columns |
| `core/db_table.go:63` | `sqlite_master` (`type='index'`, `tbl_name`, `sql`) | `TableIndexes`: stored `CREATE INDEX` text |
| `core/db_table.go:110-120` | `sqlite_master` / table lookup | `HasTable` (table or view, case-insensitive) |
| `core/db_table.go:134` | `VACUUM` | `Vacuum()` / `AuxVacuum()` |
| `core/base.go:1452-1464` | `PRAGMA wal_checkpoint(TRUNCATE)` (data, aux), `PRAGMA optimize` | Periodic maintenance (cron) |
| `core/base.go:1642`, `apis/logs.go:90` | `AuxVacuum()` after log cleanup | Reclaim aux space |
| `core/collection_record_table_sync.go:147` | `PRAGMA optimize` | After schema sync |
| `core/collection_record_table_sync.go:191` | `sqlite_master` | Detect/handle index creation during column rename (comment: "alternative to writable_schema PRAGMA") |
| `core/backup_create.go:210,256` | `VACUUM INTO {:path}` | Consistent live copy for backups |
| `core/backup_create.go:231-232,274-275,281-282` | `-wal`, `-shm` exclusions; `PRAGMA wal_checkpoint(TRUNCATE)` | File-level WAL assumptions |
| `migrations/1778828400_normalize_indexes.go:32-37,75,107` | `sqlite_master`, `name NOT LIKE 'sqlite_autoindex_%'` | Index normalisation migration |

### 1.5 Error-string coupling

| File:line | Dependency |
| --- | --- |
| `core/db_retry.go:52-53` | Retry only if error text contains `database is locked` / `table is locked` (string match; no typed error) |
| `core/validators/db.go:42-60` | Matches `unique constraint failed` in driver message to produce validation errors |
| `apis/sql.go:84` | Empty-query special case referencing go-sqlite3 behavior |
| `apis/sql.go:76-100` | SQL console classifies write queries by prefix (`INSERT`, `CREATE`, `DROP`, `DETACH`, `ALTER`, `REPLACE`); raw SQL executed against the data DB |
| `core/db_table.go:44` | Comment: driver does not error on missing table (handled by empty result) |
| `database/sql.ErrNoRows` | Used as the public "not found" signal in about 35 places across `core`, `apis`, `forms`, `plugins`, `tools/router/error.go:139` |

## 2. Filter / rule compiler

Pipeline: `tools/search` parses the filter string with `ganigeorgiev/fexpr`, turns tokens into `dbx.Expression`, using a `FieldResolver` supplied by `core`.

| Component | File | Dialect coupling |
| --- | --- | --- |
| Expression builder | `tools/search/filter.go` | Emits `LIKE ... ESCAPE '\'`, `COALESCE`, `= TRUE`; builds `dbx.NewExp` strings with `[[ ]]` |
| Token functions | `tools/search/token_functions.go` | `strftime(...)` is raw SQLite; `geoDistance` (SQL math expression) |
| Macros | `tools/search/identifier_macros.go` | `@now`, `@todayStart`... computed in Go as strings (portable), compared as TEXT dates |
| Multi-match (`?=` operators) | `tools/search/multi_match_subquery.go` | Builds `EXISTS`/subquery over joined alias with `[[ ]]` |
| Pagination, sort, count | `tools/search/provider.go`, `sort.go` | `_rowid_` special-casing, `COUNT(col)` over `CountCol`, `LIMIT/OFFSET` |
| Resolver (Record rules) | `core/record_field_resolver.go`, `record_field_resolver_runner.go` (~840 lines), `record_field_resolver_replace_expr.go` | Heaviest leak. Registers JOINs (back-relations, nested relations), `json_each` joins, `JSONExtract` for json fields and request body, `:length`, `:each`, `:lower`, `:isset` modifiers. Calls `tools/dbutils` JSON helpers |
| Dialect helpers | `tools/dbutils/json.go` (`JSONEach`, `JSONArrayLength`, `JSONExtract`), `tools/dbutils/index.go` (`ParseIndex`, `Build`, `FindSingleColumnUniqueIndex`), `tools/dbutils/select.go` | `json.go` is the only file that is purely SQLite. `index.go` parses/builds SQLite `CREATE INDEX` text (stored verbatim in `Collection.Indexes`) |
| Field-level SQL | `core/field_*.go` `ColumnType(app)`, `DriverValue`, `PrepareValue`, `Intercept*` | `ColumnType` returns SQLite type strings (see section 4). Date fields are stored TEXT `2006-01-02 15:04:05.000Z` |
| Rule evaluation | `core/record_query.go` (`CanAccessRecord`, `FindRecordsByFilter`, `RecordQuery`) | Rules are executed as SQL (`SELECT 1 ... WHERE id=? AND (rule)`), not in-memory |

Observation: rule enforcement is SQL-only. A non-SQL store (or a Go-side evaluator for edge/embed profiles) needs a second implementation of the same filter grammar, so the compiler should be split into (a) parser to AST, (b) SQL emitter (dialect), (c) optional in-memory evaluator. Today (a) and (b) are fused in `tools/search/filter.go`.

## 3. Driver and connection setup

| Item | Location | Detail |
| --- | --- | --- |
| Driver import | `core/db_connect.go` (`!no_default_driver`) | `_ "modernc.org/sqlite"`, `dbx.Open("sqlite", path+pragmas)` |
| Driver override | `core/base.go:60,225` | `BaseAppConfig.DBConnect DBConnectFunc func(dbPath string) (*dbx.DB, error)`; default `DefaultDBConnect`. `no_default_driver` build tag stub panics (`core/db_connect_nodefaultdriver.go`). This is the only existing seam |
| Version guard | `modernc_versions_check.go` (root pkg) | Warns if `modernc.org/sqlite` != v1.57.0 or `libc` != v1.74.4; reads `debug.ReadBuildInfo` |
| Two DB files | `core/base.go:49-50,1259,1321` | `data.db` and `auxiliary.db` under `DataDir()`; aux holds `_logs` and runtime-only tables (migration `1640988000_aux_init.go`) |
| Four handles per DB | `core/base.go:87-90,1259-1340` | `concurrentDB` (pool `DataMaxOpenConns`=120, idle 15; aux 20/3) and `nonconcurrentDB` (1 conn) per file, wrapped by `dualDBBuilder` (`core/db_builder.go`, 53 dbx refs) that routes SELECT to concurrent and writes to nonconcurrent to avoid `SQLITE_BUSY` |
| Pool tuning | `core/base.go:1265-1337` | `SetMaxOpenConns`, `SetMaxIdleConns`, `SetConnMaxIdleTime(3m)` |
| Query logging | `core/base.go:1278-1285` | `QueryLogFunc` / `ExecLogFunc` on `*dbx.DB` |
| Lock retry | `core/db_retry.go`, `core/db.go:84`, `core/record_query.go:48` | `WithExecHook(execLockRetry(QueryTimeout, 12))`, backoff 50..1000 ms |
| Transactions | `core/db_tx.go`, `core/db.go` | `RunInTransaction` / `AuxRunInTransaction`; in a tx, concurrent and nonconcurrent builders both point at the same `*dbx.Tx`. Comments (`record_model.go:1494`, `collection_model.go:700`) rely on SQLite single-writer semantics (select outside tx to avoid SQLITE_LOCKED) |
| Periodic maintenance | `core/base.go:1452-1464` | WAL checkpoint + optimize on cron |
| Type assertions | `core/base.go:1594` | `e.App.AuxNonconcurrentDB().(*dbx.Tx)` couples to dbx concrete type |

Leak: the connection layer assumes a local file path (`dbPath`), single-writer with in-process routing, WAL, and a second database used as a cheap logs sink.

## 4. Migrations and schema DDL

| Area | Location | SQLite-specific behavior |
| --- | --- | --- |
| Table sync engine | `core/collection_record_table_sync.go` | `CreateTable` (dbx), `RenameTable`, `AddColumn`, `RenameColumn`, `DropColumn` (needs SQLite >= 3.35), temp-column dance for type changes with `json_*` CASE rewrites (lines 207-282), `DROP INDEX IF EXISTS`, index re-creation from stored text, `PRAGMA optimize` |
| Column types | `core/field_*.go` `ColumnType` (14 implementations: bool, number, text, email, url, editor, date, autodate, select, file, relation, json, password, geo_point) | Verified samples: `TEXT DEFAULT '' NOT NULL`, `NUMERIC DEFAULT 0 NOT NULL`, `BOOLEAN DEFAULT FALSE NOT NULL`, `JSON DEFAULT NULL`, `JSON DEFAULT '[]' NOT NULL` (select); text id uses `randomblob` default (field_text.go:167). Selects/relations/files are JSON arrays in TEXT/JSON column when multi-value |
| Auto indexes | `core/collection_model.go:1028,1054` | `CREATE UNIQUE INDEX` for `id`/email/username/token key; partial index `WHERE col != ''` |
| Index text | `tools/dbutils/index.go` | Parser/builder for SQLite index grammar; `Collection.Indexes []string` persists SQL text |
| Validation | `core/collection_validate.go:548,566` | "Invalid CREATE INDEX expression", `sqlite_master` name lookup |
| Views | `core/view.go`, `core/collection_query.go:270-290` | `CREATE VIEW {{n}} AS SELECT * FROM (<user SQL>)`; user-authored SQL is the view definition; `CreateViewFields` infers fields via `TableInfo` and `sqlite` type affinity |
| Table utils | `core/db_table.go` | `TableColumns`, `TableInfo`, `TableIndexes`, `HasTable`, `DeleteTable` (`DROP TABLE IF EXISTS`), `Vacuum` |
| Migration runner | `core/migrations_runner.go:252`, `core/migrations_list.go` | `CREATE TABLE IF NOT EXISTS _migrations (file VARCHAR(255) PRIMARY KEY NOT NULL, applied INTEGER NOT NULL)` |
| System tables | `migrations/1640988000_init.go` (_params, _collections, _mfas, _otps, _externalAuths, _authOrigins, _superusers...), `migrations/1640988000_aux_init.go` (_logs) | Raw `CREATE TABLE` with `[[ ]]`, `randomblob`, `strftime` defaults |
| Legacy upgrade | `migrations/1717233556..59_v0.23_*.go` | Rewrites v0.22 schema; includes `COLLATE NOCASE` username index |
| Index normaliser | `migrations/1778828400_normalize_indexes.go` | Reads `sqlite_master`, rewrites stored index SQL |
| Migration codegen | `plugins/migratecmd/*` | Emits Go/JS migrations from collection snapshots (dialect-neutral, depends on `core.App` DB methods) |

## 5. Backups

| Assumption | Location | Detail |
| --- | --- | --- |
| DB is two files in DataDir | `core/backup_create.go:205-282` | `VACUUM INTO` temp file in `pb_data/.localtemp`, zip it as `data.db` / `auxiliary.db`, then zip the rest of the dir excluding `data.db`, `-wal`, `-shm`, same for aux |
| Final WAL checkpoint | `core/backup_create.go:281-282` | `PRAGMA wal_checkpoint(TRUNCATE)` after VACUUM INTO (errors ignored "some drivers may not support") |
| Whole `pb_data` is the unit | `core/backup_create.go:286` | `copyDirToZip(os.DirFS(DataDir))` includes `storage/`, hooks, etc. |
| Restore = swap directory | `core/backup_restore.go:141-190` | Requires `data.db` in archive; moves whole `pb_data` to temp, moves extracted content in, then `app.Restart()` (process re-exec; `osutils.MoveDirContent`). Wrapped in `RunInTransaction`+`AuxRunInTransaction` as a lock |
| Archive | `tools/archive/create.go`, `extract.go` | Plain zip of a directory tree; no DB awareness |
| Cron | `core/base.go` + `apis/backup*.go` | Backups stored in `pb_data/backups` via `NewBackupsFilesystem` (local or S3) |

A non-file store needs: `Backup(ctx, w io.Writer)` / `Restore(ctx, r io.Reader)` on the store, with files (storage dir) handled separately by the kernel. Restore currently depends on process restart, which is a server concern.

## 6. `net/http` and request-context coupling in `core/`

| File | Coupling |
| --- | --- |
| `core/events.go` | Imports `net/http`, `tools/router`, `tools/subscriptions`. Defines `ServeEvent` with `Router *router.Router[*RequestEvent]`, `Server *http.Server`, `CertManager *autocert.Manager`, `Listener net.Listener`, plus all `*RequestEvent`-based hook events (record/collection/auth/realtime/file/settings/batch request events) |
| `core/event_request.go` | Embeds `router.Event` (which wraps `http.ResponseWriter` + `*http.Request`); `RealIP()`, `RequestInfo()`, `HasSuperuserAuth()`; builds `RequestInfo` (headers, query, body) used by the rule resolver |
| `core/event_request_batch.go` | Imports `net/http` for `http.MethodGet...` in `BatchRequestsForm` validation |
| `core/record_model_superusers.go` | Imports `tools/router` for `router.NewBadRequestError` |
| `core/app.go`, `core/base.go` | Import `tools/subscriptions` (SSE `Broker`), expose `SubscriptionsBroker()`; define `OnServe`, `OnBatchRequest`, ... hooks parameterised on `*RequestEvent` |
| `core/record_field_resolver.go` | Takes `*RequestInfo` (method, headers, query, body, auth, context) for `@request.*` rules |
| `core/record_query.go` | `CanAccessRecord(record, requestInfo, rule)` signature carries `*RequestInfo` |
| `core/field_file.go` | Uses `tools/filesystem` (`*filesystem.File`), which is also imported by `tools/router`; no direct `net/http` import |
| `go list -deps ./core` | `net/http` appears as a transitive dependency (via `tools/router`) |

Exit-gate gap ("kernel has no `net/http` import"): 4 files must change (`events.go`, `event_request.go`, `event_request_batch.go`, `record_model_superusers.go`), plus the `Router`/`Server` members of `ServeEvent`. `RequestInfo` itself is plain data and can stay in the kernel; `RequestEvent`/`ServeEvent`/`BatchRequestEvent` should move to `server/`.

## 7. Package dependency graph

Direct importers (non-test and test, `go list`):

| Package | Imported by |
| --- | --- |
| `core` | root `tokibase`, `apis`, `cmd`, `forms`, `mails`, `migrations`, `plugins/ghupdate`, `plugins/jsvm`, `plugins/jsvm/internal/types`, `plugins/migratecmd`, `tests`, `examples/base` (indirect) |
| `apis` | `cmd`, `plugins/jsvm`, `tests`, `examples/base` |
| `tools/router` | `apis`, `core`, `plugins/jsvm` |
| `tools/hook` | root `tokibase`, `apis`, `core` (14 files), `examples/base`, `plugins/jsvm`, `tests`, `tools/filesystem`, `tools/mailer`, `tools/router` |
| `tools/search` | `apis`, `core`, `tools/picker` |
| `tools/dbutils` | `apis`, `core`, `migrations`, `tools/search` |
| `tools/subscriptions` | `apis`, `core`, `plugins/jsvm` |
| `tools/archive` | `core`, `plugins/ghupdate` |
| `tools/filesystem` | `apis`, `core`, `core/validators`, `forms`, `plugins/jsvm`, `tools/router` |

`core` direct tokibase imports (20): `core/validators`, `tools/{archive,auth,cron,dbutils,filesystem,hook,inflector,list,logger,mailer,osutils,router,routine,search,security,store,subscriptions,tokenizer,types}`.

`tools/router` imports: `tools/filesystem`, `tools/hook`, `tools/inflector`, `tools/picker`, `tools/store`, `net/http`. `tools/picker` imports `tools/search`; so `router -> picker -> search` makes the HTTP layer depend on the filter compiler and, via `tools/search`, on `dbx`.

`dbx` importers (non-test files): `core` 31, `apis` 7, `plugins` 3, `migrations` 3, `tools` 5, `forms` 1.

Layering problems against the target rules in `docs/ARCHITECTURE.md`:

1. `core` -> `tools/router` (and thus `net/http`).
2. `core` -> `tools/subscriptions`, `tools/mailer`, `tools/auth`, `tools/cron`, `tools/filesystem` (S3/HTTP clients). Kernel must import "nothing outside stdlib and the SQLite driver": these must become module interfaces or move to `server`/`modules`.
3. `tools/search` and `tools/dbutils` import `dbx` and encode SQLite syntax; `tools/router` pulls `tools/picker` -> `tools/search`.
4. `forms`, `mails`, `plugins/jsvm`, `plugins/migratecmd` consume `core.App` including raw DB builders.
5. Root package `tokibase` wires `core` + `apis`-adjacent behavior (`modernc_versions_check.go`, `tokibase.go`).

## 8. Proposed Store interface boundary

Goal: `kernel` never builds SQL. The SQL emitter and the SQLite driver live behind `Store`. Types `Collection`, `Record`, `Field` stay in the kernel; the store persists them and executes queries over a dialect-neutral query AST.

```go
// Lifecycle
type Store interface {
    Open(ctx context.Context) error
    Close() error
    Tx(ctx context.Context, opts TxOptions, fn func(tx Tx) error) error // nested-safe
    Tx0() Tx                                                             // autocommit handle
    Backup(ctx context.Context, w io.Writer) error
    Restore(ctx context.Context, r io.Reader) error
    Maintain(ctx context.Context) error // checkpoint + optimize + vacuum equivalent
    Capabilities() Caps                 // JSONPath, partial indexes, expression indexes, rowid, views, raw SQL
}

type Tx interface {
    // Schema
    SaveCollection(c *Collection) error
    DeleteCollection(c *Collection) error                   // drops table/view and indexes
    SyncRecordSchema(old, new *Collection) error            // create/rename/add/drop/retype columns
    ListCollections() ([]*Collection, error)
    TableExists(name string) bool
    TableColumns(name string) ([]ColumnInfo, error)
    Indexes(table string) (map[string]IndexDef, error)
    IndexNameTaken(name, exceptTable string) (string, bool, error)
    SaveView(name string, q ViewQuery) error
    DeleteView(name string) error
    ViewFields(q ViewQuery) (FieldsList, error)

    // Records (typed CRUD, no SQL strings)
    InsertRecord(c *Collection, r *Record) error
    UpdateRecord(c *Collection, r *Record) error
    DeleteRecord(c *Collection, id string) error
    GetRecord(c *Collection, id string) (*Record, error)               // ErrNotFound
    GetRecordBy(c *Collection, field string, v any, caseInsensitive bool) (*Record, error)
    FindRecords(c *Collection, q Query) (*Page[Record], error)
    CountRecords(c *Collection, q Query) (int64, error)
    ExistsRecord(c *Collection, q Query) (bool, error)                  // rule check: CanAccessRecord
    ExistsID(c *Collection, id string, ci bool) (bool, error)

    // System/aux records
    Params() ParamStore                                                  // _params
    Logs() LogStore                                                      // aux: Write(batch), List(q), Stats(range, bucket), DeleteBefore(t, minLevel)
    Aux() AuxStore                                                       // MFAs, OTPs, ExternalAuths, AuthOrigins CRUD

    // Escape hatch (admin SQL console, jsvm $app.db())
    RawQuery(ctx context.Context, sql string, args map[string]any, maxRows int) (*RawResult, error)
}

// Query AST emitted by the filter parser; the store compiles it.
type Query struct {
    Filter Expr          // parsed filter/rule AST, includes request-context bindings
    Sort   []SortSpec    // field, dir, special Rowid
    Page, PerPage int; SkipTotal bool
    Expand []string
}
type Expr interface{ exprNode() } // Cmp{Op, L, R, Any bool}, And, Or, Not, Func{Name,Args}, Path{Segments, Modifier}, Lit

type ErrKind int // NotFound, Unique, Locked, Constraint, Syntax
func Classify(err error) ErrKind // replaces string matching in db_retry.go and validators/db.go
```

Supporting kernel changes that the interface implies:

- Replace `database/sql.ErrNoRows` with `kernel.ErrNotFound` (about 35 call sites).
- Replace `Collection.Indexes []string` (SQL text) with a structured `IndexDef{Name, Unique, Columns[{Name, Collate, Desc}], Where}`; keep text round-trip for SQLite.
- Replace `Field.ColumnType(app)` with `Field.Kind()` and let the store map kinds to column types.
- Move `execLockRetry`, `dualDBBuilder`, pool tuning, pragmas, WAL checkpoints into the SQLite store module.
- Replace `Settings`/logs direct `dbx` queries with `ParamStore`/`LogStore`.
- Keep `RequestInfo` (plain data) in the kernel; move `RequestEvent`, `ServeEvent`, `BatchRequestEvent` to `server`.

### Effort estimate

| Area | Primary files | Estimate | Reason |
| --- | --- | --- | --- |
| Driver/connection/pool/pragma/retry | `core/db_connect*.go`, `db_builder.go`, `db_retry.go`, `db_tx.go`, `base.go` conns | Medium | Mostly lift-and-move into a `sqlite` module; the `App` interface exposes `dbx.Builder` publicly (breaking change for `apis`, `forms`, `jsvm`, `migrations`) |
| Raw `dbx.NewExp/HashExp/NewQuery` in non-compiler `core` code | `*_query.go`, `record_model*.go`, `collection_*.go`, `settings_*` | Medium | About 100 call sites; mechanical but each needs a typed Store method; `otp/mfa/external_auth/auth_origin` are simple CRUD |
| JSON helpers | `tools/dbutils/json.go`, resolver runner lines above, table sync CASE rewrites | Small | Three helpers plus two rewrite statements; move behind the dialect emitter |
| Filter/rule compiler | `tools/search/*`, `core/record_field_resolver*.go` | Large | Parser/AST/emitter are fused; the resolver registers JOINs and aliases directly with `dbx`; needs a clean AST and a SQLite emitter with parity tests against the existing suite. Optional in-memory evaluator is separate (Large) |
| Schema DDL and table sync | `collection_record_table_sync.go`, `field_*.go ColumnType`, `view.go`, `db_table.go`, `collection_validate.go` | Large | Retyping columns, views from user SQL, index text parsing, and `sqlite_master` introspection all need structured equivalents |
| System migrations | `migrations/*.go` | Small | Can stay SQLite-only under the `sqlite` module as `Store.Init()`; legacy v0.23 migrations remain as is |
| Backups | `backup_create.go`, `backup_restore.go`, `tools/archive` | Medium | `VACUUM INTO` and WAL exclusions move to store; restore swaps whole dir and restarts process, which must become a server concern with store-level `Restore` |
| `net/http` removal from `core` | `events.go`, `event_request*.go`, `record_model_superusers.go`, `app.go`/`base.go` hook signatures | Medium | 4 files plus every hook signature using `*RequestEvent`; hooks are public API and parity with PocketBase hook names must be kept via `server` re-exports |
| Dependency cleanup (`subscriptions`, `mailer`, `auth`, `cron`, `filesystem`) | `core/app.go`, `base.go` | Medium | Convert to module interfaces; `router -> picker -> search` edge must be cut |
| Error classification | `db_retry.go`, `validators/db.go`, `ErrNoRows` sites | Small | Typed errors from the driver (`modernc` exposes codes) |

Suggested order: (1) move `net/http` types out of `core` (Medium, unblocks the exit gate and is independent of SQL work); (2) introduce `kernel.ErrNotFound`/`Classify` (Small); (3) route all non-compiler SQL through `Store` methods while keeping SQLite implementation in-tree (Medium); (4) split the filter parser from the SQL emitter (Large); (5) structured schema/index types and DDL behind the store (Large); (6) move driver, backups, maintenance into the `sqlite` module (Medium).

## 9. Open questions and unverified items

- `go list` output was used for the import graph; `go vet`/build was not run.
- `core/field_*.go` `ColumnType` return strings were not individually transcribed; the examples in section 4 are representative, not exhaustive.
- Test files (`*_test.go`) were excluded; many contain SQLite-specific assertions that will need dialect-neutral rewrites.
- `plugins/jsvm` exposes `$app.db()`/`$dbx` bindings, so JS hooks can emit arbitrary SQL; a Store boundary needs a compatibility decision (keep `RawQuery`, or drop for non-SQLite profiles).
- `apis/sql.go` (admin SQL console) only makes sense for SQL stores; gate by `Capabilities().RawSQL`.
