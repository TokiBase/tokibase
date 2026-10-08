# Capacity (measured)

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

## Findings and claims check

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

## Re-running

```sh
# on a Linux host with Go, sqlite3 and a pb_data to copy (the source is only read with `sqlite3 .backup`)
tests/load/run.sh --src /path/to/pb_data --work /home/me/tokibase-load --tag myhost
# one scenario, shorter:
tests/load/run.sh --src ... --work ... --scenarios read-list --tag try -- -duration 10s -warmup 3s -read-conc 10,200
tests/load/run.sh ... --quick     # smoke run of all scenarios, a few minutes
```

`run.sh` copies the data, builds a stripped `toki`, and runs `go run ./tests/load`, which starts and restarts the server on `127.0.0.1:8097` for each variant (PID file `<work>/server.pid`), creates a superuser, 50 users and the scratch collection in the copy, and writes `tests/load/results/<date>-<tag>.md` and `.json` (commit the markdown only). Flags: `-scenarios` (read-list, write-create, realtime, batch, backup-under-load, mixed-soak), `-duration`, `-warmup`, `-soak`, `-read-conc`, `-write-conc`, `-rt-conns`, `-batch-sizes`. Allow 5 GB of disk for the replica and log growth, and run it on a host with nothing else running.
