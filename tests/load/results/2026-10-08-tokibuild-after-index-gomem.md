# TokiBase load results (after-index-gomem, 2026-10-08)

Host: auxiliary.db=`143.0M` data.db=`56.0M` date_utc=`2026-10-08T09:11:22Z` kernel=`6.8.0-142-generic` load_generator=`same host, Go net/http` loadavg_start=`21.31 19.96 15.33` mem=`24607164 kB` toki=`toki version (untracked) 2f791ad9` vcpu=`12` 

Dataset: 215 FGR collections, 56323 records. Largest base collections used for reads:

- `fgr_run_event_changes`: 23015 records, sort by created=true, expand=`event`, listRule set to auth-required=true
- `fgr_workout_exercises`: 9070 records, sort by created=true, expand=`training_day`, listRule set to auth-required=true
- `fgr_notifications`: 3463 records, sort by created=true, expand=`user`, listRule set to auth-required=true

## read-list

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conc=50 | 1884.5 | 0 | 25.4 | 43.2 | 53.4 | 158 | 86/422/422 | 860 | 21.31 19.96 15.33 |
| default | conc=200 | 1695.8 | 0 | 109.0 | 208.5 | 261.1 | 482 | 421/514/521 | 889 | 19.18 19.56 15.52 |
| default | conc=500 | 1125.4 | 0 | 411.6 | 803.4 | 1019.1 | 1721 | 514/513/521 | 899 | 17.92 19.12 15.67 |

Per op, default conc=50:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 37634 | 627.2 | 29.3 | 46.7 | 57.2 | 0 |
| fgr_run_event_changes | 37602 | 626.7 | 24.5 | 41.4 | 51.3 | 0 |
| fgr_workout_exercises | 37837 | 630.6 | 22.7 | 39.3 | 49.0 | 0 |

- **FLAG** (default conc=50): RSS grew 4.9x (86 -> 422 MB)

Per op, default conc=200:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 33912 | 565.2 | 113.2 | 211.6 | 262.5 | 0 |
| fgr_run_event_changes | 34289 | 571.5 | 108.1 | 207.1 | 260.5 | 0 |
| fgr_workout_exercises | 33551 | 559.2 | 105.5 | 206.1 | 258.7 | 0 |


Per op, default conc=500:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 22583 | 376.4 | 419.2 | 808.9 | 1027.2 | 0 |
| fgr_run_event_changes | 22571 | 376.2 | 408.9 | 800.9 | 1026.3 | 0 |
| fgr_workout_exercises | 22376 | 372.9 | 405.5 | 798.5 | 1008.1 | 0 |


DB files after last case of this scenario:

- auxiliary.db: 295.2M
- auxiliary.db-wal: 5.0M
- data.db: 56.1M
- data.db-wal: 1.6M

