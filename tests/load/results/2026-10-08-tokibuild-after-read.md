# TokiBase load results (after-read, 2026-10-08)

Host: auxiliary.db=`143.0M` data.db=`55.3M` date_utc=`2026-10-08T08:36:12Z` kernel=`6.8.0-142-generic` load_generator=`same host, Go net/http` loadavg_start=`13.19 16.40 12.96` mem=`24607164 kB` toki=`toki version (untracked) 2f791ad9` vcpu=`12` 

Dataset: 215 FGR collections, 56323 records. Largest base collections used for reads:

- `fgr_run_event_changes`: 23015 records, sort by created=true, expand=`event`, listRule set to auth-required=true
- `fgr_workout_exercises`: 9070 records, sort by created=true, expand=`training_day`, listRule set to auth-required=true
- `fgr_notifications`: 3463 records, sort by created=true, expand=`user`, listRule set to auth-required=true

## read-list

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conc=50 | 205.0 | 0 | 237.4 | 426.6 | 511.2 | 765 | 82/369/375 | 728 | 13.19 16.40 12.96 |
| default | conc=200 | 199.4 | 0 | 917.2 | 1767.3 | 2266.6 | 3688 | 369/392/411 | 759 | 11.70 15.27 12.82 |
| default | conc=500 | 159.6 | 0 | 2710.1 | 5642.3 | 7305.7 | 11209 | 392/438/453 | 586 | 11.94 14.64 12.78 |

Per op, default conc=50:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 4142 | 69.0 | 135.4 | 267.9 | 345.4 | 0 |
| fgr_run_event_changes | 4011 | 66.8 | 239.9 | 380.1 | 457.5 | 0 |
| fgr_workout_exercises | 4148 | 69.1 | 330.0 | 477.9 | 549.0 | 0 |

- **FLAG** (default conc=50): RSS grew 4.5x (82 -> 369 MB)

Per op, default conc=200:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 3962 | 66.0 | 833.7 | 1683.4 | 2176.0 | 0 |
| fgr_run_event_changes | 3989 | 66.5 | 912.2 | 1778.2 | 2309.7 | 0 |
| fgr_workout_exercises | 4011 | 66.8 | 999.5 | 1824.4 | 2315.2 | 0 |


Per op, default conc=500:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 3201 | 53.3 | 2640.7 | 5590.1 | 7171.5 | 0 |
| fgr_run_event_changes | 3190 | 53.2 | 2727.3 | 5546.1 | 7379.1 | 0 |
| fgr_workout_exercises | 3188 | 53.1 | 2759.8 | 5691.4 | 7337.6 | 0 |


DB files after last case of this scenario:

- auxiliary.db: 160.8M
- auxiliary.db-wal: 5.0M
- data.db: 55.4M
- data.db-wal: 1.6M

