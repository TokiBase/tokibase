# Capacity (measured)

The tables under "Results" and "Findings" are the first run (binary `4a848155`). The section "After the fixes" repeats the affected scenarios on the same host and data after `perf/memory-wal-logs`; findings 1, 3, 4 and 5 below are resolved there.

Measured with the harness in `tests/load` against a COPY of the real FitGymRun `pb_data`. One run, one host, one
binary: treat the numbers as an order of magnitude, not as a guarantee. Not part of CI.

## Setup

| | |
| --- | --- |
| Host | `tokibuild`: 12 vCPU, 24 GB RAM, NVMe, Ubuntu 24.04, kernel 6.8.0-142, Go 1.27.1 |
| Binary | `examples/base` (all modules, default env, `TOKI_TLS_CHECK=off`), commit `4a848155` |
| Date | 2026-10-08, about 1 h wall clock, start 07:03 UTC |
| Load generator | Go `net/http`, on the SAME host (it competes for the 12 vCPUs; server CPU in the table is in % of one core) |
| Host load | the host was otherwise idle; the 1/5/15 min load average at the start of every case is in `tests/load/results/2026-10-08-tokibuild.md` (7.6 at the start of read-list, 1-4 during most write cases, 19 at the start of the soak, all caused by the harness itself) |
| Dataset | 215 FGR collections, 56,323 records, `data.db` 55 MB, `auxiliary.db` 143 MB (4.3 M log rows after the run). Reads use the 3 largest base collections: `fgr_run_event_changes` 23,015, `fgr_workout_exercises` 9,070, `fgr_notifications` 3,463 |
| Auth | 50 users in a scratch auth collection `load_users`; `listRule` of the 3 FGR collections was set to `@request.auth.id != ''` in the copy so the rule engine is on the path. FGR `pb_hooks` are NOT loaded. SMTP, S3, backup S3/cron and rate limits are switched off in the copy |
| Scratch | `load_items` (title, n, tag with index, 200 B body) holds all writes; no FGR collection is written |
| Per case | 10 s warm-up (not measured), then 60 s |

An earlier attempt on the 2 vCPU host `wpe` was taken while other jobs kept its load average at 10-20; those numbers
were discarded and are not used here.

## Results

Failures were 0 in every case below unless stated. Latencies in ms.

### read-list (list 30 rows, `filter` + `sort=-created` + `expand`, count included)

| clients | req/s | p50 | p95 | p99 | server RSS before -> after | server CPU |
| --- | --- | --- | --- | --- | --- | --- |
| 50 | 192 | 282 | 572 | 627 | 128 -> 818 MB | 8.1 cores |
| 200 | 160 | 1205 | 2234 | 2585 | 818 -> 1496 MB | 8.7 cores |
| 500 | 151 | 2970 | 5395 | 6603 | 1496 -> 1563 MB | 8.8 cores |

Throughput is flat from 50 to 500 clients: the server is CPU bound and extra clients only queue. Per collection at 50 clients p50 is 9 ms
(`fgr_notifications`), 292 ms (`fgr_run_event_changes`), 504 ms (`fgr_workout_exercises`).

### write-create (POST to the scratch collection, user token unless marked)

| variant | 1 writer req/s (p99) | 8 writers req/s (p99) | 32 writers req/s (p99) |
| --- | --- | --- | --- |
| `TOKI_AUDIT=off` | 2007 (0.9) | 5103 (22.5) | 5213 (33.5) |
| audit on (default) | 2014 (1.0) | 5316 (19.6) | 4735 (36.4) |
| audit off, superuser token | 1946 (1.0) | 5279 (20.2) | 5505 (33.1) |
| audit on, superuser token | 1553 (5.1) | 3170 (25.6) | 3357 (40.2) |
| walreplica `file://` (1 s sync) | 1580 (1.1) | 2477 (14.8) | 2484 (89.3) |
| sync JS hook, 50 ms busy loop | 19.9 (50.9) | 158 (64.1) | 325 (238.3) |
| sync JS hook, 50 ms blocking `$http.send` | 19.6 (51.5) | 155 (64.8) | 614 (74.0) |

- Audit only records superuser/impersonated writes, so a user-token workload shows no overhead; for superuser writes it costs 18-39 % of throughput.
- walreplica to a local path costs 21 % at 1 writer and about 50 % at 8-32 writers.
- A synchronous hook caps a writer at `1 / hook time` (about 20/s at 50 ms); total throughput is `writers / 0.05 s` until CPU (busy loop) or the JS VM pool (`--hooksPool`, default 15, visible in the 32-writer I/O case) runs out.

### realtime (SSE, subscribed to the scratch collection with a `listRule`; 100 records burst by 4 writers, then 100 at 20/s)

| connections | burst fan-out p50 / p99 | paced p50 / p99 | dropped events | RSS idle -> loaded | server CPU for the case |
| --- | --- | --- | --- | --- | --- |
| 200 | 4.0 / 10.5 | 2.7 / 12.4 | 0 of 40,000 | 172 -> 263 MB | 4.5 s |
| 500 | 7.2 / 17.0 | 4.2 / 10.0 | 0 of 100,000 | 257 -> 277 MB | 9.7 s |
| 1000 | 14.3 / 121.8 | 4.8 / 11.7 | 0 of 200,000 | 253 -> 308 MB | 23.0 s |

No dead connections, no duplicates, no missing events. Latency is measured from just before the POST until the client read the event, on the same host.

### batch (`/api/batch` creates, 4 concurrent clients)

| sub-requests | batchguard rules | batches/s | records/s | p50 | p99 |
| --- | --- | --- | --- | --- | --- |
| 50 | none | 112.6 | 5630 | 28.7 | 114.6 |
| 50 | trivial `assert true` | 109.0 | 5452 | 30.1 | 111.4 |
| 50 | trivial `assert_post true` | 91.9 | 4595 | 34.9 | 143.8 |
| 500 | none | 13.6 | 6800 | 222 | 934 |
| 500 | trivial `assert true` | 13.8 | 6925 | 221 | 860 |
| 500 | trivial `assert_post true` | 12.2 | 6083 | 246 | 1048 |

A probe batch committed exactly N rows in every case, and a rule with `assert false` answered 400 `validation_batch_rule` with 0 rows committed. `assert` costs nothing measurable; `assert_post` (re-reads the written records) costs 10-17 %.

### backup-under-load (read-list at 200 clients; 30 s baseline)

| phase | length | req/s | p50 | p99 |
| --- | --- | --- | --- | --- |
| baseline | 30 s | 167 | 1124 | 2540 |
| `toki backup create` (364 MB zip) | 46.4 s | 157 | 1270 | 2656 |
| `toki backup verify latest` (integrity ok) | 38.0 s | 141 | 1385 | 3268 |
| `POST /api/backups` | did not return within the 120 s client timeout | 136 | 1443 | 3325 |
| after | 20 s | 155 | 1285 | 2662 |

CLI backup and verify slow reads by 6-16 % and p99 by up to 1.3x; no read failed. Server RSS peaked at 2.4 GB in this case.

### mixed-soak (20 min, 32 workers with about 50 ms think time, 70 % list / 25 % create / 5 % SSE session)

253 req/s average (min 224, max 299), 0 failures, p50 12 ms, per-minute p99 between 253 and 430 ms; p99 of minutes 1-3 was 367 ms and of minutes 18-20 was 266 ms (no drift). Create p99 177 ms; SSE connect p99 193 ms; event fan-out p99 179 ms (14,473 sessions, all got an event).
Server RSS: 310 MB (minute 1), 384 MB (minute 20). `data.db-wal` grew linearly from 61 MB to 1.03 GB (about 50 MB/min), see below. Per-minute table in the results file.

## After the fixes (branch `perf/memory-wal-logs`)

Same host (`tokibuild`), same FGR copy, same harness (`tests/load/run.sh`, port 8098), 2026-10-08 08:36-09:15 UTC. The host was not idle: other jobs kept the load average at 11-19 at the start of every case, so single numbers move by +-10 % (the same pool size measured twice gave 186-213 req/s at 200 clients). Raw reports: `tests/load/results/2026-10-08-tokibuild-after-*.md`, `...-bk0.md`.

### Summary

| anomaly | before | after |
| --- | --- | --- |
| RSS, read-list 50 clients | 128 -> 818 MB | 82 -> 369 MB |
| RSS, read-list 200 clients | 818 -> 1496 MB (peak 1567) | 369 -> 392 MB (peak 411) |
| read-list 200 clients, req/s / p99 | 160 / 2585 ms | 199 / 2267 ms |
| RSS, read-list 500 clients | 1496 -> 1563 MB | 392 -> 438 MB (peak 453) |
| RSS peak during backup-under-load | 2414 MB | 607-636 MB (backup + 200 clients) |
| `data.db-wal`, 20 min mixed soak | 61 MB -> 1032 MB, linear | sawtooth, maximum 257 MB, truncated every 5-6 min |
| mixed soak throughput / p99 | 253 req/s / 295 ms (min 1) | 284 req/s / 224 ms (min 1: 429) |
| `POST /api/backups`, 200 clients, `auxiliary.db` 2.4 GB | no answer in 120 s (76 s when waiting longer, see below) | 106 s while the log cap was still pruning that 2.4 GB; 9.5 s once `auxiliary.db` is at the cap |
| `toki backup create`, same | 46.4 s | 10.3 s at the cap |
| `auxiliary.db` live size | 143 MB -> 2.5 GB in one hour | held at 410-512 MB (`TOKI_LOGS_MAX_MB`) |

### 1. Memory

Measured with `LOAD_SERVER_ENV` (read-list, 30 s per case, RSS after the case; req/s is +-10 % noise). "unindexed" is the FGR schema as is.

| max conns | `cache_size` | `temp_store` | RSS 50 / 200 clients (MB) | req/s 50 / 200 |
| --- | --- | --- | --- | --- |
| 120 (before) | 32000 KB | memory | 859 / 1498 | 223 / 199 |
| 120 | 8192 KB | memory | 803 / 1467 | 214 / 197 |
| 48 | 32000 KB | memory | 705 / 701 | 229 / 209 |
| 48 | 8192 KB | file | 660 / 670 | 228 / 200 |
| 48 | 2048 KB | file | 507 / 527 | 211 / 169 |
| 48 | 8192 KB | file, soft heap limit 64 MB | 662 / 668 | 192 / 176 |
| 24 | 8192 KB | memory | 373 / 393 | 187 / 204 |
| 24 | 8192 KB | file | 371 / 396 | 247 / 214 |
| 24 | 4096 KB | file | 347 / 367 | 177 / 192 |
| 16 | 8192 KB | file | 277 / 301 | 215 / 186 |
| 12 | 8192 KB | file | 232 / 250 | 192 / 198 |

- The suspected `cache_size` is not the main cause: 32 -> 8 MB changes RSS by 4-5 %. `temp_store` and the SQLite soft heap limit did not change RSS either.
- The pool size is the lever: RSS is about 80 MB + 12 MB per busy connection, because the unindexed `sort=-created` list builds a 7-12 MB sort in every running query. The queries are CPU bound (12 cores), so 12 connections give the same throughput as 120; fewer connections only shorten the queue inside SQLite.
- Defaults now: pool `2 x CPUs` between 16 and 120 (24 here), `cache_size` 8 MB, `temp_store` memory. `TOKI_DB_MAX_CONNS`, `TOKI_DB_CACHE_KB`, `TOKI_DB_TEMP_STORE`, `TOKI_DB_HEAP_MB`, `TOKI_DB_MMAP_MB` override them ([PROFILES.md](PROFILES.md), "Sizing for a 1 GB VPS"). Result with the defaults: **392 MB at 200 clients** (target 512 MB), 199 req/s (before 160, same noise).
- Not done: a shared cache budget across connections (SQLite has no shared page cache budget for private connections; the product `max conns x cache_size` is the bound), and the RSS stays after the load stops (the allocator keeps the pages: 392 MB after, 369 MB before the next case).
- With the `created` indexes of finding 2 (below) the same 200 clients drive 7-8x the throughput, and RSS rises to 553 MB because the Go heap now serves 1100-1700 responses/s. `GOMEMLIMIT=350MiB` kept it at 514 MB at 200 clients with 1696 req/s. Set `GOMEMLIMIT` on small hosts.

### 2. WAL

`modules/store/sqlite/walmaint.go`: `kernel` cron `__tokiWALMaintain__` runs every minute on `data.db` and `auxiliary.db` a `PRAGMA wal_checkpoint(PASSIVE)` on a pooled read connection and, when the WAL file is larger than `TOKI_WAL_MAX_MB` (default 256), a `wal_checkpoint(TRUNCATE)` on the single write connection (writers are held back, readers on old snapshots finish within the 15 s attempt timeout while new readers already read the database file; up to 3 attempts with backoff). The cause is as suspected: the automatic checkpoint is PASSIVE, it can copy frames but never reset the log while a reader is always active. Counters (sizes, runs, escalations, failures, last error) and the pool state are in `GET /api/health` for superusers (`data.db.data.wal`, `data.db.auxiliary.wal`, `...pool`). Skipped while walreplica is active.

Soak (20 min, same mix as before): `data.db-wal` per minute 37, 63, 112, 159, 211, **8**, 67, 117, 117, 134, 197, 257, **9**, 70, 129, 189, 251, **7**, 63, 112 MB. Maximum 257 MB (threshold 256 plus one minute of writes, about 50 MB/min) against 1032 MB before; no write failed, p99 did not drift (x0.94 between minutes 1-3 and 18-20). A unit test (`TestMaintainWALWithOverlappingReaders`) reproduces the starvation with three overlapping readers.

### 3. API backup under load

Before, measured again on the code of this branch with the log cap switched off (`TOKI_LOGS_MAX_MB=0`, `auxiliary.db` 2.4 GB) and a client timeout of 15 min: CLI backup 53.4 s, verify 30.8 s, `POST /api/backups` **76.5 s** (204). So it did finish, it was 1.4x the CLI, and the original 120 s client timeout was crossed because of the 2.3 GB `auxiliary.db` (`VACUUM INTO` of it plus zipping 2.3 GB of mostly request logs) on a host whose cores were all busy with the 200 clients. It was not the checkpoint (the final `TRUNCATE` is bounded by the 10 s busy timeout and `VACUUM INTO` takes no checkpoint) and not the backup lock. The fix is the size of `auxiliary.db` plus an opt-in non-blocking call:

- With `auxiliary.db` at the cap (live 480 MB): CLI create 10.3 s (was 46.4), verify 5.5 s (was 38.0), **API 9.5 s** (was > 120 s), reads during the API backup 159 req/s against 164 baseline, p99 x1.1. Zip 90 MB (was 364 MB).
- Starting from a 2.4 GB `auxiliary.db` with the cap on, the first minutes prune about 3 million rows in 10,000 row statements (it competes with the log writer), and the API backup took 106 s in that window; this is a one-time convergence after upgrading. The file itself keeps its size (free pages are reused); the backup copy (`VACUUM INTO`) is compact. To shrink the file run the logs vacuum once (settings, logs, "Vacuum" or `AuxVacuum()`).
- `POST /api/backups?async=true` (or header `Prefer: respond-async`) returns `202` with `{state, name, startedAt}` immediately and `GET /api/backups/status` (superuser) reports `running`, `done` or `failed`. Without the opt-in the call is unchanged (204 when done). The job is a goroutine of the server, not a `kernel.Jobs` job: the durable queue is optional (`TOKI_JOBS=off`, nano) and re-running a half-finished multi-minute backup after a crash is not wanted. See `docs/COMPAT.md`.

### 4. Logs and indexes

- `TOKI_LOGS_MAX_MB` (default 512, `0` = off): the cron `__tokiLogsSizeCap__` (every minute) deletes the oldest `_logs` rows by insertion order while the live size of `auxiliary.db` (pages minus free list) is above the cap, down to 80 % of it. Measured: 2.4 GB inflated + 700 MB soak start -> live size 424 MB, 689k rows, file 859 MB. It is a size cap on the whole `auxiliary.db` (other auxiliary tables are small), on top of `logs.maxDays`; with `logs.maxDays = 0` nothing is logged and nothing pruned.
- `TOKI_LOGS_SAMPLE_OK=N` (default 1 = all) keeps 1 of N successful GET request logs; errors, writes and non-GET requests are always logged.
- `toki db advise [--json] [--min-records N] [--no-logs]` lists the unindexed `sort`/`filter` fields from the schema (autodate `created`/`updated`, single relation fields) and from the slow `GET .../records` request logs, with the `CREATE INDEX` statement. On the FGR copy it reports 26 findings; with `--min-records 5000`: `created` and `updated` of `fgr_run_event_changes` and `fgr_workout_exercises`, `fgr_workout_exercises.exercise` (filter), and `fgr_workout_exercises.order` seen in slow requests. `fgr_notifications.created` (3463 records) is below that threshold.
- The index is the biggest single lever for reads. With an index on `created` in the three load collections (`CREATE INDEX ... (created)`, nothing else changed), read-list gave **1611 req/s at 50 clients (p50 26 ms, was 205 req/s, 237 ms)** and 1143-1696 req/s at 200 clients (p50 109-153 ms, was 199 req/s, 917 ms). Add the indexes that `db advise` suggests to the FGR schema.

## Findings and claims check (original run, before the fixes)

1. **README says `solo` is "a single binary on a 1 GB VPS".** Contradicted for this dataset under concurrent reads: RSS 818 MB at 50 clients, 1.5 GB at 200, 2.4 GB during backup-under-load (idle: 128 MB). Cause: `cache_size(-32000)` (32 MB) per SQLite connection with a pool of up to 120 connections (`kernel/base.go` `DefaultDataMaxOpenConns`) plus `temp_store(MEMORY)` for the sorts. A 1 GB host needs a smaller `DataMaxOpenConns`; not verified here.
2. **Read throughput is bounded by unindexed sorts in the FGR schema, not by the kernel.** A single `fgr_workout_exercises` list takes 4.3 ms without sort and 15.6 ms with `sort=-created` (full scan plus temp b-tree: `EXPLAIN QUERY PLAN` shows `SCAN` and `USE TEMP B-TREE FOR ORDER BY`, no index on `created`). With the sort, about 27 ms of CPU per request. Not compared with upstream PocketBase (no PocketBase binary was run).
3. **`data.db-wal` is not bounded under sustained load.** It reached 1.03 GB in 20 min (the `journal_size_limit` is 200 MB) with walreplica off. The only `wal_checkpoint(TRUNCATE)` is the nightly maintenance and backups (`modules/store/sqlite/store.go`); the automatic checkpoint is presumably starved by continuously overlapping readers (not verified). Disk and restart-time risk for a busy instance.
4. **`POST /api/backups` did not finish in 120 s under 200 read clients**, while the CLI backup took 46 s. The backup path runs a TRUNCATE checkpoint that waits for all readers (documented in `walreplica.md`); likely the same starvation as finding 3 (not verified).
5. **Request logging dominates `auxiliary.db`.** It grew from 143 MB to 2.5 GB (4.3 M `_logs` rows, about 0.55 KB per request) in one hour of load; it is also what walreplica ships (replica dir 5.4 GB), which is part of the 50 % replication write overhead. Logs are kept for the configured `logs.maxDays`.
6. walreplica.md says writers "are not blocked": true (no failures, p99 89 ms at 32 writers), but throughput halves. No claim in the docs gives a number to compare with.
7. `docs/modules/audit.md` says audit targets low-volume privileged traffic: consistent; superuser writes lose up to 39 % throughput with audit on.
8. batchguard.md: "with no rules it adds no work": consistent (within noise of a trivial `assert`).
9. Not measured because the harness does not cover them: sync "1-3 ms per change" (SYNC_DESIGN), replica RPO/RTO (covered by `tests/e2e/failover.sh`), jobs throughput. The docs contain no capacity claim for SSE connections, writes/s or reads/s.
10. No dropped realtime events, no errors, no p99 above 2 s at 50 clients or fewer, and no unbounded RSS growth (soak: 310 to 384 MB).

## Replica size (walreplica, 2026-10-08)

The 7-day soak showed the file replica growing to 1.7 GB in 5 h with flat databases. Cause: hourly snapshots of a growing `auxiliary.db` (logs) kept for 24 h, plus all compaction levels since the oldest snapshot; nothing is pruned before the first snapshot is 24 h old. Defaults are now snapshot 6 h, retention 24 h, `TOKI_REPLICA_MAX_MB` 4096 (warn and prune expired restore points). Measured in a 17 min stress run with time scaled 1 h = 30 s: 5.7 GB with the old ratio (1 h : 24 h) against 2.6-2.9 GB with the new one (6 h : 24 h); with a 700 MB limit the guard pruned to 1.5-1.9 GB. Plan for replica size = (retention / snapshot interval + 1) x compressed databases + changes in the window, and keep `TOKI_LOGS_MAX_MB` set. Details and the restore-after-prune check: `docs/modules/walreplica.md` (Sizing).

## Re-running

```sh
# on a Linux host with Go, sqlite3 and a pb_data to copy (the source is only read with `sqlite3 .backup`)
tests/load/run.sh --src /path/to/pb_data --work /home/me/tokibase-load --tag myhost
# one scenario, shorter:
tests/load/run.sh --src ... --work ... --scenarios read-list --tag try -- -duration 10s -warmup 3s -read-conc 10,200
tests/load/run.sh ... --quick     # smoke run of all scenarios, a few minutes
```

`run.sh` copies the data, builds a stripped `toki`, and runs `go run ./tests/load`, which starts and restarts the server on `127.0.0.1:8097` for each variant (PID file `<work>/server.pid`), creates a superuser, 50 users and the scratch collection in the copy, and writes `tests/load/results/<date>-<tag>.md` and `.json` (commit the markdown only). Flags: `-scenarios` (read-list, write-create, realtime, batch, backup-under-load, mixed-soak), `-duration`, `-warmup`, `-soak`, `-read-conc`, `-write-conc`, `-rt-conns`, `-batch-sizes`. Allow 5 GB of disk for the replica and log growth, and run it on a host with nothing else running.
