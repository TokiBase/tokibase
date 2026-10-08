#!/usr/bin/env bash
# every 5 min: one CSV line in metrics.csv
. "$(dirname "$0")/lib.sh"
CSV=$SOAK/metrics.csv
[ -s "$CSV" ] || echo "ts,uptime_s,restarts,rss_kb,data_db_b,data_wal_b,aux_db_b,aux_wal_b,replica_b,fds,goroutines,replica_lag_s,pool_wait,p99_ms,http5xx,load_errors,disk_free_kb" > "$CSV"
sz() { stat -c %s "$1" 2>/dev/null || echo 0; }
pid=$(svc_pid)
if [ -n "$pid" ] && [ "$pid" != 0 ] && [ -d /proc/$pid ]; then
  up=$(ps -o etimes= -p "$pid" | tr -d ' ')
  rss=$(awk '/VmRSS/{print $2}' /proc/$pid/status)
  fds=$(ls /proc/$pid/fd 2>/dev/null | wc -l)
else up=0; rss=0; fds=0; fi
restarts=$(systemctl show -p NRestarts --value toki-soak)
tok=$(su_token)
lag=""; pw=""; gor=""
if [ -n "$tok" ]; then
  h=$(curl -s -m 10 "$BASE/api/health" -H "Authorization: $tok")
  lag=$(echo "$h" | jq -r '[.data.replica.databases[]?.lagSeconds] | max // empty')
  pw=$(echo "$h" | jq -r '[.data.db[]?.pool.waitCount] | add // empty')
  gor=$(echo "$h" | jq -r '.data.goroutines // .data.runtime.goroutines // empty')
fi
rep=$(du -sb "$SOAK/replica" 2>/dev/null | cut -f1)
p99=""; e5=""; le=""
if [ -s "$SOAK/state/last_load.json" ]; then
  p99=$(jq -r '.p99_ms' "$SOAK/state/last_load.json"); e5=$(jq -r '.http5xx' "$SOAK/state/last_load.json"); le=$(jq -r '.errors' "$SOAK/state/last_load.json")
fi
free=$(df --output=avail -k "$SOAK" | tail -1 | tr -d ' ')
echo "$(ts),$up,$restarts,$rss,$(sz $DATA/data.db),$(sz $DATA/data.db-wal),$(sz $DATA/auxiliary.db),$(sz $DATA/auxiliary.db-wal),${rep:-0},$fds,$gor,$lag,$pw,$p99,$e5,$le,$free" >> "$CSV"
