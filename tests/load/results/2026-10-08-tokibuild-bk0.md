# TokiBase load results (bk0, 2026-10-08)

Host: auxiliary.db=`2417.2M` data.db=`55.3M` date_utc=`2026-10-08T08:30:11Z` kernel=`6.8.0-142-generic` load_generator=`same host, Go net/http` loadavg_start=`17.48 13.58 10.36` mem=`24607164 kB` toki=`toki version (untracked) 73348ea6` vcpu=`12` 

Dataset: 215 FGR collections, 56323 records. Largest base collections used for reads:

- `fgr_run_event_changes`: 23015 records, sort by created=true, expand=`event`, listRule set to auth-required=true
- `fgr_workout_exercises`: 9070 records, sort by created=true, expand=`training_day`, listRule set to auth-required=true
- `fgr_notifications`: 3463 records, sort by created=true, expand=`user`, listRule set to auth-required=true

## backup-under-load

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conc=200 | 171.3 | 0 | 995.1 | 1912.6 | 2411.1 | 3756 | 85/355/650 | 0 | 16.96 13.54 10.36 |

Phases, default conc=200:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1-baseline | 2570 | 171.3 | 995.1 | 1912.6 | 2411.1 | 3756 | 0 | 0 | 0B | 0B |  |
| 2-cli-backup-create | 7897 | 148.0 | 1250.2 | 2476.4 | 3180.5 | 5503 | 0 | 0 | 0B | 0B | p99 x1.3 vs baseline |
| 3-cli-backup-verify | 5089 | 165.3 | 1137.2 | 2170.8 | 2719.8 | 4086 | 0 | 0 | 0B | 0B | p99 x1.1 vs baseline |
| 4-api-backup | 9945 | 130.0 | 1374.7 | 2997.6 | 4117.5 | 6735 | 0 | 0 | 0B | 0B | p99 x1.7 vs baseline |
| 5-after | 1085 | 108.5 | 1849.0 | 3590.0 | 4330.0 | 6749 | 0 | 0 | 0B | 0B | p99 x1.8 vs baseline |

Details, default conc=200:

- note_2-cli-backup-create: `err=<nil> size=407.9M out=WARNING ruleguard: 215 public rule(s) not allowlisted (run `rule lint` for details, `rule allow` to accept)
Backup created.`
- note_3-cli-backup-verify: `err=<nil> ok=true`
- note_4-api-backup: `status=204 err=<nil>`
- seconds_1-baseline: `15.0`
- seconds_2-cli-backup-create: `53.4`
- seconds_3-cli-backup-verify: `30.8`
- seconds_4-api-backup: `76.5`
- seconds_5-after: `10.0`

- **FLAG** (default conc=200): RSS grew 4.2x (85 -> 355 MB)

DB files after last case of this scenario:

- auxiliary.db: 2434.4M
- auxiliary.db-wal: 4.5M
- data.db: 55.4M
- data.db-wal: 781K

