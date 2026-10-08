# TokiBase load results (after-soak, 2026-10-08)

Host: auxiliary.db=`858.9M` data.db=`55.3M` date_utc=`2026-10-08T08:44:56Z` kernel=`6.8.0-142-generic` load_generator=`same host, Go net/http` loadavg_start=`19.36 22.19 16.74` mem=`24607164 kB` toki=`toki version (untracked) 2f791ad9` vcpu=`12` 

Dataset: 215 FGR collections, 56323 records. Largest base collections used for reads:

- `fgr_run_event_changes`: 23015 records, sort by created=true, expand=`event`, listRule set to auth-required=true
- `fgr_workout_exercises`: 9070 records, sort by created=true, expand=`training_day`, listRule set to auth-required=true
- `fgr_notifications`: 3463 records, sort by created=true, expand=`user`, listRule set to auth-required=true

## mixed-soak

| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |
|---|---|---|---|---|---|---|---|---|---|---|
| default | mix=70r/25w/5rt think_avg=50ms workers=32 | 284.0 | 0 | 14.8 | 188.8 | 224.3 | 906 | 88/260/293 | 718 | 19.36 22.19 16.74 |

Per op, default mix=70r/25w/5rt think_avg=50ms workers=32:

| op | n | req/s | p50 | p95 | p99 | failed |
|---|---|---|---|---|---|---|
| create | 81432 | 67.9 | 8.2 | 0.0 | 492.6 | 0 |
| rt-connect | 15846 | 13.2 | 1.8 | 0.0 | 88.9 | 0 |
| rt-event | 15846 | 13.2 | 14.9 | 0.0 | 253.2 | 0 |

Phases, default mix=70r/25w/5rt think_avg=50ms workers=32:

| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |
|---|---|---|---|---|---|---|---|---|---|---|---|
| min01 | 13560 | 226.0 | 41.1 | 298.8 | 428.9 | 906 | 0 | 283 | 36.8M | 60.9M |  |
| min02 | 14427 | 240.4 | 21.8 | 254.6 | 338.6 | 712 | 0 | 272 | 63.0M | 60.9M |  |
| min03 | 14894 | 248.2 | 20.4 | 247.9 | 276.0 | 399 | 0 | 262 | 111.6M | 60.9M |  |
| min04 | 14892 | 248.2 | 20.3 | 246.8 | 269.4 | 369 | 0 | 267 | 159.3M | 60.9M |  |
| min05 | 15551 | 259.2 | 16.5 | 235.2 | 261.7 | 425 | 0 | 271 | 210.6M | 60.9M |  |
| min06 | 17071 | 284.5 | 15.3 | 203.9 | 248.7 | 383 | 0 | 266 | 7.7M | 60.9M |  |
| min07 | 17671 | 294.5 | 14.8 | 188.8 | 216.7 | 346 | 0 | 275 | 66.5M | 60.9M |  |
| min08 | 18090 | 301.5 | 14.5 | 183.8 | 213.6 | 814 | 0 | 267 | 117.1M | 60.9M |  |
| min09 | 19000 | 316.7 | 9.1 | 168.4 | 195.0 | 284 | 0 | 276 | 117.1M | 60.9M |  |
| min10 | 18428 | 307.1 | 9.3 | 176.0 | 203.1 | 342 | 0 | 253 | 133.9M | 60.9M |  |
| min11 | 18776 | 312.9 | 9.9 | 174.0 | 199.8 | 306 | 0 | 257 | 196.6M | 60.9M |  |
| min12 | 18478 | 308.0 | 10.2 | 176.1 | 201.0 | 310 | 0 | 260 | 256.9M | 60.9M |  |
| min13 | 18717 | 311.9 | 9.9 | 174.0 | 197.2 | 310 | 0 | 254 | 8.6M | 60.9M |  |
| min14 | 18569 | 309.5 | 10.0 | 174.2 | 198.0 | 325 | 0 | 259 | 69.7M | 60.9M |  |
| min15 | 18576 | 309.6 | 10.0 | 174.3 | 198.6 | 301 | 0 | 264 | 129.3M | 60.9M |  |
| min16 | 18421 | 307.0 | 10.1 | 178.2 | 202.7 | 290 | 0 | 270 | 188.9M | 60.9M |  |
| min17 | 17983 | 299.7 | 13.3 | 182.2 | 235.1 | 552 | 0 | 271 | 250.8M | 60.9M |  |
| min18 | 17200 | 286.7 | 15.2 | 201.0 | 224.3 | 363 | 0 | 274 | 7.4M | 60.9M |  |
| min19 | 16395 | 273.2 | 15.2 | 211.8 | 333.5 | 643 | 0 | 260 | 62.8M | 60.9M |  |
| min20 | 14058 | 234.3 | 24.2 | 292.0 | 419.6 | 584 | 0 | 260 | 112.3M | 60.9M |  |

Details, default mix=70r/25w/5rt think_avg=50ms workers=32:

- p99_drift: `x0.94`
- p99_first3min_ms: `347.8`
- p99_last3min_ms: `325.8`
- rss_last_mb: `260`
- rss_min01_mb: `283`
- scratch_records_at_end: `82086`

- **FLAG** (default mix=70r/25w/5rt think_avg=50ms workers=32): RSS grew 3.0x (88 -> 260 MB)

DB files after last case of this scenario:

- auxiliary.db: 858.9M
- auxiliary.db-wal: 60.9M
- data.db: 81.4M
- data.db-wal: 112.3M

