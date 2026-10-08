# Soak pass/fail criteria (7 days, profile solo)

The soak passes when every row holds from `START` to `START + 7 days`. `report.sh` evaluates them daily.

| # | Criterion | Source |
| --- | --- | --- |
| 1 | 0 unplanned restarts (`NRestarts` of `toki-soak.service` stays 0; planned `systemctl restart` is logged below and does not count) | metrics.csv `restarts` |
| 2 | RSS <= 700 MB sustained (no sample above) | metrics.csv `rss_kb` |
| 3 | `data.db-wal` <= 300 MB | metrics.csv `data_wal_b` |
| 4 | 0 failed backup verifies (`toki backup verify latest`, every 6 h) | log/maint.log `RESULT backup_verify` |
| 5 | 0 audit chain errors (`toki audit verify`, every 24 h) | log/audit.log `RESULT audit_verify` |
| 6 | replica lag < 10 s at every sample (the RPO claim) | metrics.csv `replica_lag_s` (`/api/health` `data.replica`) |
| 7 | p99 of the hourly load run <= 2x the day-1 baseline (median p99 of the runs in the first 24 h) | loadruns.csv `p99_ms` |
| 8 | No HTTP 5xx and no failed request in any load run | loadruns.csv `http5xx`, `errors` |

Not criteria, but watch: `goroutines` is empty in metrics.csv because `/api/health` does not expose it; `pool_wait` (cumulative `waitCount` of the read pools) growing steadily means the pool is too small; `fds` should be flat.

Start: see `/home/ubuntu/soak/START`. End = start + 7 days. Mark planned restarts/changes here (date, reason):

- (none)
