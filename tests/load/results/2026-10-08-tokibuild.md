# TokiBase load results (tokibuild, 2026-10-08)

Host: auxiliary.db=`143.0M` data.db=`55.3M` date_utc=`2026-10-08T07:03:10Z` kernel=`6.8.0-142-generic` load_generator=`same host, Go net/http` loadavg_start=`7.57 5.26 2.12` mem=`24607164 kB` toki=`toki version (untracked) 4a848155` vcpu=`12` 

Dataset: 215 FGR collections, 56323 records. Largest base collections used for reads:

- `fgr_run_event_changes`: 23015 records, sort by created=true, expand=`event`, listRule set to auth-required=true
- `fgr_workout_exercises`: 9070 records, sort by created=true, expand=`training_day`, listRule set to auth-required=true
- `fgr_notifications`: 3463 records, sort by created=true, expand=`user`, listRule set to auth-required=true

## read-list

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conc=50 | 191.7 | 0 | 281.9 | 572.0 | 626.9 | 702 | 128/818/863 | 809 | 7.57 5.26 2.12 |
| default | conc=200 | 160.0 | 0 | 1204.6 | 2233.5 | 2584.8 | 3614 | 818/1496/1567 | 867 | 8.13 5.91 2.57 |
| default | conc=500 | 151.4 | 0 | 2969.6 | 5395.3 | 6602.7 | 10435 | 1496/1563/1583 | 875 | 9.30 6.73 3.10 |

Per op, default conc=50:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 3832 | 63.9 | 8.8 | 12.8 | 15.1 | 0 |
| fgr_run_event_changes | 3737 | 62.3 | 292.1 | 377.0 | 407.8 | 0 |
| fgr_workout_exercises | 3933 | 65.5 | 504.0 | 611.3 | 648.0 | 0 |

- **FLAG** (default conc=50): RSS grew 6.4x (128 -> 818 MB)

Per op, default conc=200:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 3285 | 54.7 | 527.5 | 1042.6 | 1346.9 | 0 |
| fgr_run_event_changes | 3141 | 52.3 | 1261.3 | 1859.7 | 2180.7 | 0 |
| fgr_workout_exercises | 3174 | 52.9 | 1851.4 | 2488.9 | 2813.9 | 0 |


Per op, default conc=500:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 3096 | 51.6 | 2398.4 | 4624.6 | 5625.6 | 0 |
| fgr_run_event_changes | 3012 | 50.2 | 2968.5 | 5247.7 | 6449.5 | 0 |
| fgr_workout_exercises | 2979 | 49.6 | 3523.6 | 5793.5 | 7292.0 | 0 |


DB files after last case of this scenario:

- auxiliary.db: 158.1M
- auxiliary.db-wal: 4.9M
- data.db: 55.4M
- data.db-wal: 2.2M

## write-create

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| audit-off | conc=1 | 2007.4 | 0 | 0.4 | 0.6 | 0.9 | 31 | 132/221/221 | 107 | 9.56 7.43 3.63 |
| audit-off | conc=8 | 5102.9 | 0 | 0.9 | 2.0 | 22.5 | 51 | 224/237/238 | 322 | 4.01 6.19 3.48 |
| audit-off | conc=32 | 5212.7 | 0 | 3.2 | 24.9 | 33.5 | 80 | 249/342/345 | 326 | 4.19 5.79 3.54 |
| audit-on(default) | conc=1 | 2013.8 | 0 | 0.4 | 0.6 | 1.0 | 30 | 197/272/272 | 109 | 4.15 5.49 3.60 |
| audit-on(default) | conc=8 | 5315.6 | 0 | 0.9 | 2.1 | 19.6 | 37 | 275/284/286 | 332 | 2.51 4.70 3.46 |
| audit-on(default) | conc=32 | 4734.6 | 0 | 3.7 | 26.5 | 36.4 | 80 | 297/389/392 | 338 | 3.44 4.55 3.50 |
| audit-off,superuser | conc=1 | 1946.0 | 0 | 0.4 | 0.7 | 1.0 | 38 | 195/310/310 | 107 | 5.18 4.88 3.69 |
| audit-off,superuser | conc=8 | 5279.4 | 0 | 0.9 | 2.2 | 20.2 | 47 | 314/331/331 | 287 | 3.25 4.37 3.60 |
| audit-off,superuser | conc=32 | 5505.0 | 0 | 2.9 | 24.5 | 33.1 | 62 | 332/408/410 | 275 | 4.32 4.47 3.69 |
| audit-on,superuser | conc=1 | 1553.1 | 0 | 0.4 | 0.7 | 5.1 | 39 | 198/317/317 | 92 | 4.14 4.37 3.72 |
| audit-on,superuser | conc=8 | 3169.8 | 0 | 1.1 | 16.1 | 25.6 | 45 | 353/471/471 | 211 | 2.34 3.78 3.56 |
| audit-on,superuser | conc=32 | 3356.9 | 0 | 4.3 | 29.7 | 40.2 | 71 | 482/579/580 | 228 | 2.58 3.57 3.50 |
| walreplica-file | conc=1 | 1580.1 | 0 | 0.6 | 0.9 | 1.1 | 104 | 202/101/216 | 139 | 2.90 3.52 3.50 |
| walreplica-file | conc=8 | 2476.9 | 0 | 2.0 | 8.6 | 14.8 | 146 | 158/119/120 | 233 | 1.65 3.01 3.32 |
| walreplica-file | conc=32 | 2484.0 | 0 | 7.5 | 41.8 | 89.3 | 218 | 177/157/158 | 236 | 2.78 3.04 3.30 |
| hook-50ms-cpu | conc=1 | 19.9 | 0 | 50.0 | 50.2 | 50.9 | 68 | 207/227/227 | 105 | 2.48 2.90 3.23 |
| hook-50ms-cpu | conc=8 | 158.4 | 0 | 50.0 | 51.2 | 64.1 | 75 | 230/273/273 | 816 | 1.46 2.50 3.07 |
| hook-50ms-cpu | conc=32 | 325.1 | 0 | 87.6 | 178.0 | 238.3 | 738 | 269/325/325 | 1159 | 6.30 3.78 3.48 |
| hook-50ms-io | conc=1 | 19.6 | 0 | 50.9 | 51.2 | 51.5 | 73 | 168/172/172 | 2 | 10.48 5.56 4.12 |
| hook-50ms-io | conc=8 | 155.2 | 0 | 51.2 | 51.9 | 64.8 | 78 | 174/176/178 | 12 | 3.25 4.40 3.82 |
| hook-50ms-io | conc=32 | 614.2 | 0 | 51.2 | 53.0 | 74.0 | 90 | 182/244/244 | 68 | 1.23 3.56 3.57 |

Details, audit-off conc=1:

- records_in_scratch_after: `140256`
- writes_per_s: `2007.4`


Details, audit-off conc=8:

- records_in_scratch_after: `360841`
- writes_per_s: `5102.9`


Details, audit-off conc=32:

- records_in_scratch_after: `363398`
- writes_per_s: `5212.7`


Details, audit-on(default) conc=1:

- records_in_scratch_after: `141195`
- writes_per_s: `2013.8`


Details, audit-on(default) conc=8:

- records_in_scratch_after: `374056`
- writes_per_s: `5315.6`


Details, audit-on(default) conc=32:

- records_in_scratch_after: `335075`
- writes_per_s: `4734.6`


Details, audit-off,superuser conc=1:

- records_in_scratch_after: `137264`
- writes_per_s: `1946.0`


Details, audit-off,superuser conc=8:

- records_in_scratch_after: `367613`
- writes_per_s: `5279.4`


Details, audit-off,superuser conc=32:

- records_in_scratch_after: `383314`
- writes_per_s: `5505.0`


Details, audit-on,superuser conc=1:

- records_in_scratch_after: `108732`
- writes_per_s: `1553.1`


Details, audit-on,superuser conc=8:

- records_in_scratch_after: `222398`
- writes_per_s: `3169.8`


Details, audit-on,superuser conc=32:

- records_in_scratch_after: `234277`
- writes_per_s: `3356.9`


Details, walreplica-file conc=1:

- records_in_scratch_after: `111102`
- writes_per_s: `1580.1`


Details, walreplica-file conc=8:

- records_in_scratch_after: `175932`
- writes_per_s: `2476.9`


Details, walreplica-file conc=32:

- records_in_scratch_after: `176631`
- writes_per_s: `2484.0`


Details, hook-50ms-cpu conc=1:

- records_in_scratch_after: `1397`
- writes_per_s: `19.9`


Details, hook-50ms-cpu conc=8:

- records_in_scratch_after: `11089`
- writes_per_s: `158.4`


Details, hook-50ms-cpu conc=32:

- records_in_scratch_after: `22782`
- writes_per_s: `325.1`


Details, hook-50ms-io conc=1:

- records_in_scratch_after: `1371`
- writes_per_s: `19.6`


Details, hook-50ms-io conc=8:

- records_in_scratch_after: `10860`
- writes_per_s: `155.2`


Details, hook-50ms-io conc=32:

- records_in_scratch_after: `42893`
- writes_per_s: `614.2`


DB files after last case of this scenario:

- auxiliary.db: 2301.9M
- auxiliary.db-wal: 5.0M
- data.db: 177.4M
- data.db-wal: 4.0M
- replica_dir: 5372.7M

## realtime

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conns=200 | 0.0 | 0 | 4.0 | 9.3 | 10.5 | 12 | 172/263/268 | 0 | 1.71 3.16 3.42 |
| default | conns=500 | 0.0 | 0 | 7.2 | 13.6 | 17.0 | 34 | 257/277/282 | 0 | 1.35 2.98 3.36 |
| default | conns=1000 | 0.0 | 0 | 14.3 | 108.1 | 121.8 | 161 | 253/308/320 | 0 | 1.30 2.87 3.31 |

Phases, default conns=200:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| burst fan-out latency | 20000 | 0.0 | 4.0 | 9.3 | 10.5 | 12 | 0 | 263 | 3.9M | 4.2M | expected=20000 received=20000 dropped=0 clients_short=0 duplicates=0 dead_conns=0 |
| paced20ps fan-out latency | 20000 | 0.0 | 2.7 | 7.8 | 12.4 | 44 | 0 | 263 | 3.9M | 4.2M | expected=20000 received=20000 dropped=0 clients_short=0 duplicates=0 dead_conns=0 |

Details, default conns=200:

- burst_write_seconds: `0.28`
- connect_failures: `0`
- connect_seconds: `0.4`
- dropped_burst: `0`
- dropped_paced20ps: `0`
- rss_after_close_mb: `263`
- rss_after_connect_mb: `257`
- rss_after_writes_mb: `263`
- server_cpu_s_during_case: `4.5`
- server_fds_after_close: `99`
- server_fds_after_connect: `295`
- server_threads_after_connect: `39`
- write_failed: `burst 0 paced 0`
- write_latency_ms(p50/p99): `burst 9.8/52.5 paced 5.2/12.6`


Phases, default conns=500:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| burst fan-out latency | 50000 | 0.0 | 7.2 | 13.6 | 17.0 | 34 | 0 | 277 | 3.9M | 4.2M | expected=50000 received=50000 dropped=0 clients_short=0 duplicates=0 dead_conns=0 |
| paced20ps fan-out latency | 50000 | 0.0 | 4.2 | 8.7 | 10.0 | 12 | 0 | 277 | 3.9M | 4.2M | expected=50000 received=50000 dropped=0 clients_short=0 duplicates=0 dead_conns=0 |

Details, default conns=500:

- burst_write_seconds: `0.41`
- connect_failures: `0`
- connect_seconds: `0.4`
- dropped_burst: `0`
- dropped_paced20ps: `0`
- rss_after_close_mb: `277`
- rss_after_connect_mb: `282`
- rss_after_writes_mb: `277`
- server_cpu_s_during_case: `9.7`
- server_fds_after_close: `103`
- server_fds_after_connect: `599`
- server_threads_after_connect: `39`
- write_failed: `burst 0 paced 0`
- write_latency_ms(p50/p99): `burst 14.2/57.4 paced 9.8/12.6`


Phases, default conns=1000:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| burst fan-out latency | 100000 | 0.0 | 14.3 | 108.1 | 121.8 | 161 | 0 | 308 | 4.0M | 4.3M | expected=100000 received=100000 dropped=0 clients_short=0 duplicates=0 dead_conns=0 |
| paced20ps fan-out latency | 100000 | 0.0 | 4.8 | 9.8 | 11.7 | 14 | 0 | 308 | 4.0M | 4.3M | expected=100000 received=100000 dropped=0 clients_short=0 duplicates=0 dead_conns=0 |

Details, default conns=1000:

- burst_write_seconds: `2.20`
- connect_failures: `0`
- connect_seconds: `0.5`
- dropped_burst: `0`
- dropped_paced20ps: `0`
- rss_after_close_mb: `308`
- rss_after_connect_mb: `309`
- rss_after_writes_mb: `308`
- server_cpu_s_during_case: `23.0`
- server_fds_after_close: `107`
- server_fds_after_connect: `1103`
- server_threads_after_connect: `39`
- write_failed: `burst 0 paced 0`
- write_latency_ms(p50/p99): `burst 102.5/153.5 paced 10.7/15.5`


DB files after last case of this scenario:

- auxiliary.db: 2303.3M
- auxiliary.db-wal: 4.3M
- data.db: 177.4M
- data.db-wal: 4.0M
- replica_dir: 5372.7M

## batch

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| no-rules | conc=4 subrequests=50 | 112.6 | 0 | 28.7 | 78.8 | 114.6 | 226 | 150/269/269 | 138 | 0.93 2.68 3.24 |
| rule-assert-true | conc=4 subrequests=50 | 109.0 | 0 | 30.1 | 81.1 | 111.4 | 198 | 270/337/337 | 138 | 1.41 2.46 3.12 |
| rule-assert_post-true | conc=4 subrequests=50 | 91.9 | 0 | 34.9 | 98.6 | 143.8 | 376 | 336/389/389 | 135 | 1.22 2.18 2.98 |
| no-rules | conc=4 subrequests=500 | 13.6 | 0 | 222.2 | 636.4 | 933.8 | 1308 | 394/404/411 | 145 | 2.11 2.24 2.93 |
| rule-assert-true | conc=4 subrequests=500 | 13.8 | 0 | 221.3 | 588.0 | 860.3 | 1392 | 400/412/416 | 150 | 2.21 2.32 2.91 |
| rule-assert_post-true | conc=4 subrequests=500 | 12.2 | 0 | 245.8 | 734.9 | 1047.5 | 1546 | 409/419/422 | 146 | 1.53 2.10 2.79 |

Details, no-rules conc=4 subrequests=50:

- records_per_s: `5630`

- note (no-rules conc=4 subrequests=50): probe batch status=200 rows 0->50 (expected +50) err=<nil>

Details, rule-assert-true conc=4 subrequests=50:

- records_per_s: `5452`

- note (rule-assert-true conc=4 subrequests=50): probe batch status=200 rows 0->50 (expected +50) err=<nil>

Details, rule-assert_post-true conc=4 subrequests=50:

- records_per_s: `4595`

- note (rule-assert_post-true conc=4 subrequests=50): probe batch status=200 rows 0->50 (expected +50) err=<nil>

Details, no-rules conc=4 subrequests=500:

- records_per_s: `6800`

- note (no-rules conc=4 subrequests=500): probe batch status=200 rows 0->500 (expected +500) err=<nil>

Details, rule-assert-true conc=4 subrequests=500:

- records_per_s: `6925`

- note (rule-assert-true conc=4 subrequests=500): probe batch status=200 rows 0->500 (expected +500) err=<nil>

Details, rule-assert_post-true conc=4 subrequests=500:

- records_per_s: `6083`

- note (rule-assert_post-true conc=4 subrequests=500): probe batch status=200 rows 0->500 (expected +500) err=<nil>
- note (rule-assert_post-true conc=4 subrequests=500): rejecting rule check: status=400 rows committed=0 (want 400 and 0) body={"data":{"batch":{"code":"validation_batch_rule","message":"load reject","rule":"load-reject"}},"message":"Batch rejected.","status":400}

DB files after last case of this scenario:

- auxiliary.db: 2312.1M
- auxiliary.db-wal: 4.5M
- data.db: 202.8M
- data.db-wal: 5.9M
- replica_dir: 5372.7M

## backup-under-load

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conc=200 | 167.4 | 0 | 1123.5 | 2106.9 | 2540.2 | 3315 | 145/2412/2414 | 0 | 1.48 1.95 2.69 |

Phases, default conc=200:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1-baseline | 5023 | 167.4 | 1123.5 | 2106.9 | 2540.2 | 3315 | 0 | 0 | 0B | 0B |  |
| 2-cli-backup-create | 7271 | 156.6 | 1269.6 | 2309.3 | 2655.5 | 3317 | 0 | 0 | 0B | 0B | p99 x1.0 vs baseline |
| 3-cli-backup-verify | 5359 | 141.0 | 1385.3 | 2692.4 | 3268.4 | 4362 | 0 | 0 | 0B | 0B | p99 x1.3 vs baseline |
| 4-api-backup | 16296 | 135.8 | 1442.5 | 2765.7 | 3325.4 | 4593 | 0 | 0 | 0B | 0B | p99 x1.3 vs baseline |
| 5-after | 3100 | 155.0 | 1285.2 | 2324.7 | 2661.6 | 3316 | 0 | 0 | 0B | 0B | p99 x1.0 vs baseline |

Details, default conc=200:

- note_2-cli-backup-create: `err=<nil> size=364.2M out=WARNING ruleguard: 215 public rule(s) not allowlisted (run `rule lint` for details, `rule allow` to accept)
Backup created.`
- note_3-cli-backup-verify: `err=<nil> ok=true`
- note_4-api-backup: `err=Post "http://127.0.0.1:8097/api/backups": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`
- seconds_1-baseline: `30.0`
- seconds_2-cli-backup-create: `46.4`
- seconds_3-cli-backup-verify: `38.0`
- seconds_4-api-backup: `120.0`
- seconds_5-after: `20.0`

- **FLAG** (default conc=200): RSS grew 16.6x (145 -> 2412 MB)

DB files after last case of this scenario:

- auxiliary.db: 2328.2M
- auxiliary.db-wal: 157.2M
- data.db: 202.8M
- data.db-wal: 817K
- replica_dir: 5372.7M

## mixed-soak

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | mix=70r/25w/5rt think_avg=50ms workers=32 | 253.0 | 0 | 12.0 | 245.0 | 294.6 | 743 | 156/371/392 | 672 | 19.29 12.47 6.90 |

Per op, default mix=70r/25w/5rt think_avg=50ms workers=32:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| create | 72395 | 60.3 | 2.7 | 0.0 | 177.4 | 0 |
| rt-connect | 14473 | 12.1 | 2.0 | 0.0 | 193.3 | 0 |
| rt-event | 14473 | 12.1 | 6.7 | 0.0 | 178.7 | 0 |

Phases, default mix=70r/25w/5rt think_avg=50ms workers=32:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| min01 | 15200 | 253.3 | 12.0 | 245.0 | 294.6 | 526 | 0 | 310 | 60.8M | 159.3M |  |
| min02 | 13716 | 228.6 | 16.1 | 291.7 | 430.2 | 743 | 0 | 338 | 107.2M | 159.3M |  |
| min03 | 13982 | 233.0 | 18.0 | 279.3 | 377.3 | 565 | 0 | 317 | 154.0M | 159.3M |  |
| min04 | 14371 | 239.5 | 13.3 | 264.7 | 316.1 | 513 | 0 | 321 | 202.2M | 159.3M |  |
| min05 | 13615 | 226.9 | 14.0 | 284.4 | 349.9 | 621 | 0 | 328 | 247.5M | 159.3M |  |
| min06 | 13427 | 223.8 | 15.4 | 294.2 | 355.0 | 598 | 0 | 331 | 292.7M | 159.3M |  |
| min07 | 15212 | 253.5 | 11.5 | 250.5 | 303.5 | 556 | 0 | 349 | 344.7M | 159.3M |  |
| min08 | 14211 | 236.8 | 14.1 | 271.5 | 328.6 | 565 | 0 | 352 | 393.1M | 159.3M |  |
| min09 | 14809 | 246.8 | 12.0 | 254.8 | 304.4 | 527 | 0 | 351 | 443.3M | 159.3M |  |
| min10 | 14956 | 249.3 | 13.1 | 254.9 | 311.6 | 530 | 0 | 350 | 491.8M | 159.3M |  |
| min11 | 15777 | 262.9 | 10.9 | 232.3 | 277.2 | 543 | 0 | 355 | 545.6M | 159.3M |  |
| min12 | 15474 | 257.9 | 13.0 | 234.3 | 281.5 | 485 | 0 | 356 | 597.0M | 159.3M |  |
| min13 | 15582 | 259.7 | 10.9 | 234.8 | 279.1 | 485 | 0 | 358 | 651.1M | 159.3M |  |
| min14 | 15374 | 256.2 | 12.0 | 234.5 | 294.0 | 544 | 0 | 358 | 700.4M | 159.3M |  |
| min15 | 15592 | 259.9 | 12.5 | 233.5 | 276.4 | 469 | 0 | 360 | 752.3M | 159.3M |  |
| min16 | 15948 | 265.8 | 10.9 | 228.6 | 270.8 | 497 | 0 | 362 | 807.5M | 159.3M |  |
| min17 | 16013 | 266.9 | 11.6 | 227.4 | 280.6 | 475 | 0 | 351 | 862.0M | 159.3M |  |
| min18 | 17961 | 299.4 | 10.5 | 189.5 | 253.2 | 463 | 0 | 360 | 921.7M | 159.3M |  |
| min19 | 16296 | 271.6 | 10.9 | 221.9 | 267.8 | 455 | 0 | 370 | 977.0M | 159.3M |  |
| min20 | 16113 | 268.6 | 11.4 | 227.3 | 276.0 | 460 | 0 | 384 | 1031.9M | 159.3M |  |

Details, default mix=70r/25w/5rt think_avg=50ms workers=32:

- p99_drift: `x0.72`
- p99_first3min_ms: `367.4`
- p99_last3min_ms: `265.6`
- rss_last_mb: `384`
- rss_min01_mb: `310`
- scratch_records_at_end: `73011`

- **FLAG** (default mix=70r/25w/5rt think_avg=50ms workers=32): RSS grew 2.4x (156 -> 371 MB)

DB files after last case of this scenario:

- auxiliary.db: 2533.3M
- auxiliary.db-wal: 159.3M
- data.db: 202.8M
- data.db-wal: 1031.9M
- replica_dir: 5372.7M

