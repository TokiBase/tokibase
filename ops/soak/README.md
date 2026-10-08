# 7-day solo soak

Production proof for profile `solo` (Phase 1 exit gate): a stripped `toki` built from main serves a copy of the FGR `pb_data` under systemd with replication, audit and a low hourly load, while timers collect metrics, run backup/audit checks and produce a daily report. Pass/fail rules: [CRITERIA.md](CRITERIA.md). Everything lives on the VM under `/home/ubuntu/soak`, listens on `127.0.0.1:8099`, and uses about 1 core only during the 10 min of load each hour.

## Install (on the VM, user `ubuntu` with sudo)

```sh
cd /home/ubuntu/work/tokibase && git pull
ops/soak/setup.sh      # builds toki, VERSION, online copy of /home/ubuntu/qc/src (sqlite3 .backup), superuser soak@local.test
ops/soak/install.sh    # copies scripts, installs units, creates the scratch collection soak_items, starts everything, writes START
```

`setup.sh` refuses to overwrite an existing `pb_data`. The source dir has no `pb_hooks`, so the soak runs without hooks. The superuser password is in `/home/ubuntu/soak/.su-pass` (mode 0600).

## What runs

| Unit | When | What |
| --- | --- | --- |
| `toki-soak.service` | always (`Restart=always`) | `toki serve`, `GOMEMLIMIT=700MiB`, `TOKI_REPLICA_URL=file:///home/ubuntu/soak/replica`, `TOKI_WAL_MAX_MB=256`, `TOKI_LOGS_MAX_MB=512`, audit default (on). While replication is on the minutely WAL truncation is skipped (Litestream governs the WAL). |
| `toki-soak-load.timer` | hourly | `load.py run`: 20 clients, 70% list reads on the 8 biggest FGR collections, 25% creates in `soak_items`, 5% realtime (SSE, subscribe, own write, wait for event), 300 ms average think time, 10 min |
| `toki-soak-maint.timer` | every 6 h | `toki backup create`, `backup verify latest` (keeps the 6 newest `soak-*.zip`), `replica status` |
| `toki-soak-audit.timer` | every 24 h | `toki audit verify` |
| `toki-soak-metrics.timer` | every 5 min | one line in `metrics.csv` |
| `toki-soak-report.timer` | daily 00:05 UTC | `report.sh` |

## Files in /home/ubuntu/soak

`START`, `VERSION` (commit, version, build time), `metrics.csv`, `loadruns.csv` (one row per load run), `log/{serve,load,maint,audit}.log` (UTC timestamps; result lines are `RESULT backup_verify OK|FAIL` and `RESULT audit_verify OK|FAIL`), `reports/<date>.md`, `state/last_load.json`.

`metrics.csv` columns: `ts,uptime_s,restarts,rss_kb,data_db_b,data_wal_b,aux_db_b,aux_wal_b,replica_b,fds,goroutines,replica_lag_s,pool_wait,p99_ms,http5xx,load_errors,disk_free_kb`. `restarts` is `NRestarts` of the service; `p99_ms`/`http5xx`/`load_errors` repeat the last load run; `goroutines` stays empty (not in `/api/health`); `replica_lag_s` is the maximum `lagSeconds` of `data.replica`.

## Reading reports

```sh
/home/ubuntu/soak/report.sh              # prints the last 24 h now and writes reports/<today>.md
cat /home/ubuntu/soak/reports/$(date -u +%F).md
systemctl list-timers 'toki-soak-*'; systemctl status toki-soak
tail -f /home/ubuntu/soak/log/serve.log
```

The report ends with the criteria table (PASS/FAIL per row) and an overall verdict. A planned restart (`systemctl restart`) does not raise `NRestarts`, but note it in CRITERIA.md.

## Stop

```sh
sudo systemctl disable --now toki-soak-{load,metrics,maint,audit,report}.timer toki-soak.service
```

Data, logs and reports stay in `/home/ubuntu/soak`; delete the directory and `/etc/systemd/system/toki-soak*` to remove everything. Never use `pkill`/`pgrep -f` on this VM, other jobs run there.
