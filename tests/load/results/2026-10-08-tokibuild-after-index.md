# TokiBase load results (after-index, 2026-10-08)

Host: auxiliary.db=`143.0M` data.db=`56.0M` date_utc=`2026-10-08T09:07:32Z` kernel=`6.8.0-142-generic` load_generator=`same host, Go net/http` loadavg_start=`9.40 10.35 11.42` mem=`24607164 kB` toki=`toki version (untracked) 2f791ad9` vcpu=`12` 

Dataset: 215 FGR collections, 56323 records. Largest base collections used for reads:

- `fgr_run_event_changes`: 23015 records, sort by created=true, expand=`event`, listRule set to auth-required=true
- `fgr_workout_exercises`: 9070 records, sort by created=true, expand=`training_day`, listRule set to auth-required=true
- `fgr_notifications`: 3463 records, sort by created=true, expand=`user`, listRule set to auth-required=true

## read-list

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conc=50 | 1610.9 | 0 | 25.8 | 65.1 | 129.0 | 509 | 83/407/425 | 762 | 9.37 10.33 11.41 |
| default | conc=200 | 1142.8 | 0 | 153.1 | 356.9 | 495.4 | 1095 | 408/553/553 | 534 | 28.79 14.92 12.87 |
| default | conc=500 | 1519.0 | 0 | 296.5 | 632.3 | 834.8 | 1634 | 553/738/755 | 771 | 34.81 20.60 15.05 |

Per op, default conc=50:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 32204 | 536.7 | 29.3 | 72.5 | 143.5 | 0 |
| fgr_run_event_changes | 32305 | 538.4 | 24.8 | 63.9 | 125.4 | 0 |
| fgr_workout_exercises | 32146 | 535.7 | 23.0 | 60.1 | 119.8 | 0 |

- **FLAG** (default conc=50): RSS grew 4.9x (83 -> 407 MB)

Per op, default conc=200:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 22792 | 379.8 | 158.2 | 362.7 | 501.6 | 0 |
| fgr_run_event_changes | 22759 | 379.3 | 152.9 | 357.9 | 496.0 | 0 |
| fgr_workout_exercises | 23025 | 383.7 | 148.1 | 348.1 | 487.9 | 0 |


Per op, default conc=500:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| fgr_notifications | 30364 | 506.0 | 299.9 | 639.7 | 844.3 | 0 |
| fgr_run_event_changes | 30423 | 507.0 | 296.4 | 632.2 | 833.7 | 0 |
| fgr_workout_exercises | 30364 | 506.0 | 293.1 | 626.0 | 820.4 | 0 |


DB files after last case of this scenario:

- auxiliary.db: 278.6M
- auxiliary.db-wal: 6.3M
- data.db: 56.1M
- data.db-wal: 1.6M

