# TokiBase load results (after-backup, 2026-10-08)

Host: auxiliary.db=`2417.2M` data.db=`55.3M` date_utc=`2026-10-08T08:40:06Z` kernel=`6.8.0-142-generic` load_generator=`same host, Go net/http` loadavg_start=`14.85 15.04 13.10` mem=`24607164 kB` toki=`toki version (untracked) 2f791ad9` vcpu=`12` 

Dataset: 215 FGR collections, 56323 records. Largest base collections used for reads:

- `fgr_run_event_changes`: 23015 records, sort by created=true, expand=`event`, listRule set to auth-required=true
- `fgr_workout_exercises`: 9070 records, sort by created=true, expand=`training_day`, listRule set to auth-required=true
- `fgr_notifications`: 3463 records, sort by created=true, expand=`user`, listRule set to auth-required=true

## backup-under-load

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | conc=200 | 189.8 | 0 | 947.4 | 1808.2 | 2258.8 | 3476 | 82/574/636 | 0 | 14.85 15.04 13.10 |

Phases, default conc=200:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1-baseline | 5693 | 189.8 | 947.4 | 1808.2 | 2258.8 | 3476 | 0 | 0 | 0B | 0B |  |
| 2-cli-backup-create | 9113 | 170.8 | 1101.1 | 2067.5 | 2638.3 | 3931 | 0 | 0 | 0B | 0B | p99 x1.2 vs baseline |
| 3-cli-backup-verify | 5421 | 93.5 | 1924.4 | 4255.9 | 5385.7 | 7829 | 0 | 0 | 0B | 0B | p99 x2.4 vs baseline |
| 4-api-backup | 12713 | 119.9 | 1496.9 | 3342.8 | 4466.1 | 9073 | 0 | 0 | 0B | 0B | p99 x2.0 vs baseline |
| 5-after | 2929 | 146.4 | 1260.6 | 2506.2 | 3096.1 | 4761 | 0 | 0 | 0B | 0B | p99 x1.4 vs baseline |

Details, default conc=200:

- note_2-cli-backup-create: `err=<nil> size=408.2M out=WARNING ruleguard: 215 public rule(s) not allowlisted (run `rule lint` for details, `rule allow` to accept)
Backup created.`
- note_3-cli-backup-verify: `err=<nil> ok=true`
- note_4-api-backup: `status=204 err=<nil>`
- seconds_1-baseline: `30.0`
- seconds_2-cli-backup-create: `53.4`
- seconds_3-cli-backup-verify: `58.0`
- seconds_4-api-backup: `106.1`
- seconds_5-after: `20.0`

- **FLAG** (default conc=200): RSS grew 7.0x (82 -> 574 MB)

DB files after last case of this scenario:

- auxiliary.db: 2420.3M
- auxiliary.db-wal: 9.3M
- data.db: 55.4M
- data.db-wal: 4K

