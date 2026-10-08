# TokiBase load results (after-backup-cap, 2026-10-08)

Host: auxiliary.db=`630.6M` data.db=`55.3M` date_utc=`2026-10-08T09:05:45Z` kernel=`6.8.0-142-generic` load_generator=`same host, Go net/http` loadavg_start=`11.12 10.08 11.43` mem=`24607164 kB` toki=`toki version (untracked) 2f791ad9` vcpu=`12` 

Dataset: 215 FGR collections, 56323 records. Largest base collections used for reads:

- `fgr_run_event_changes`: 23015 records, sort by created=true, expand=`event`, listRule set to auth-required=true
- `fgr_workout_exercises`: 9070 records, sort by created=true, expand=`training_day`, listRule set to auth-required=true
- `fgr_notifications`: 3463 records, sort by created=true, expand=`user`, listRule set to auth-required=true

## backup-under-load

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conc=200 | 164.0 | 0 | 1104.7 | 2034.9 | 2542.3 | 4238 | 84/572/607 | 0 | 11.12 10.08 11.43 |

Phases, default conc=200:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1-baseline | 4919 | 164.0 | 1104.7 | 2034.9 | 2542.3 | 4238 | 0 | 0 | 0B | 0B |  |
| 2-cli-backup-create | 1807 | 175.2 | 1068.2 | 2021.1 | 2470.9 | 3490 | 0 | 0 | 0B | 0B | p99 x1.0 vs baseline |
| 3-cli-backup-verify | 989 | 179.7 | 1053.6 | 2026.0 | 2571.5 | 3344 | 0 | 0 | 0B | 0B | p99 x1.0 vs baseline |
| 4-api-backup | 1515 | 158.9 | 1185.9 | 2176.9 | 2710.6 | 3926 | 0 | 0 | 0B | 0B | p99 x1.1 vs baseline |
| 5-after | 3400 | 170.0 | 1100.6 | 2109.9 | 2635.5 | 3705 | 0 | 0 | 0B | 0B | p99 x1.0 vs baseline |

Details, default conc=200:

- note_2-cli-backup-create: `err=<nil> size=90.4M out=WARNING ruleguard: 215 public rule(s) not allowlisted (run `rule lint` for details, `rule allow` to accept)
Backup created.`
- note_3-cli-backup-verify: `err=<nil> ok=true`
- note_4-api-backup: `status=204 err=<nil>`
- seconds_1-baseline: `30.0`
- seconds_2-cli-backup-create: `10.3`
- seconds_3-cli-backup-verify: `5.5`
- seconds_4-api-backup: `9.5`
- seconds_5-after: `20.0`

- **FLAG** (default conc=200): RSS grew 6.8x (84 -> 572 MB)

DB files after last case of this scenario:

- auxiliary.db: 630.6M
- auxiliary.db-wal: 4.8M
- data.db: 55.4M
- data.db-wal: 0B

