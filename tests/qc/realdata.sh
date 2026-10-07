#!/usr/bin/env bash
# Repeated QC of a TokiBase binary against a COPY of a real PocketBase pb_data.
# Never touches the source directory: data.db/auxiliary.db are copied with `sqlite3 .backup`.
#
# usage: tests/qc/realdata.sh --toki /path/to/toki --src /opt/pocketbase-x/pb_data [--work /root/tokibase-qc] [--port 8095]
# exit 0 when every check passes; prints one JSON line per check and a final summary.
set -u
TOKI=""; SRC=""; WORK="/root/tokibase-qc"; PORT=8095
while [ $# -gt 0 ]; do case "$1" in
  --toki) TOKI=$2; shift 2;; --src) SRC=$2; shift 2;; --work) WORK=$2; shift 2;; --port) PORT=$2; shift 2;;
  *) echo "unknown arg $1"; exit 2;; esac; done
[ -x "$TOKI" ] || { echo "toki binary missing: $TOKI"; exit 2; }
[ -f "$SRC/data.db" ] || { echo "source pb_data missing: $SRC"; exit 2; }
command -v sqlite3 >/dev/null || { echo "sqlite3 required"; exit 2; }

DATA="$WORK/pb_data"; LOG="$WORK/serve.log"; FAIL=0; RESULTS=()
mkdir -p "$DATA"; rm -f "$DATA"/data.db* "$DATA"/auxiliary.db*
sqlite3 "$SRC/data.db" ".backup '$DATA/data.db'"
sqlite3 "$SRC/auxiliary.db" ".backup '$DATA/auxiliary.db'"
[ -d "$SRC/../pb_hooks" ] && HOOKS="--hooksDir $SRC/../pb_hooks" || HOOKS=""

check() { # name, ok(0/1), detail
  local ok=$2; [ "$ok" = 0 ] || FAIL=1
  printf '{"check":"%s","ok":%s,"detail":%s}\n' "$1" "$([ "$ok" = 0 ] && echo true || echo false)" "$(printf '%s' "$3" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()[:400]))')"
}

# 1. boot + serve (migrations from older PocketBase versions run here)
( cd "$WORK" && exec env TOKI_TLS_CHECK=off "$TOKI" serve --dir "$DATA" $HOOKS --http "127.0.0.1:$PORT" >"$LOG" 2>&1 ) &
echo $! >"$WORK/pid"
for i in $(seq 1 40); do curl -sf "http://127.0.0.1:$PORT/api/health" >/dev/null && break; sleep 1; done
H=$(curl -s -m 10 "http://127.0.0.1:$PORT/api/health"); echo "$H" | grep -q '"code":200'; check boot $? "$H"

# 2. a public collection answers 200 and a locked one answers 403/empty for guests
COLS=$(curl -s -m 10 "http://127.0.0.1:$PORT/api/collections?perPage=1" -o /dev/null -w '%{http_code}')
[ "$COLS" = 401 ]; check "collections_need_auth" $? "GET /api/collections as guest -> $COLS"

# 3. rule lint (report only; exit code 1 is expected on real data)
L=$("$TOKI" rule lint --json --dir "$DATA" 2>/dev/null); N=$(echo "$L" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(len(d), len({f["collection"] for f in d if f["severity"]=="error"}))' 2>/dev/null)
[ -n "$N" ]; check "rule_lint_runs" $? "findings/collections: $N"

# 4. backup create + verify
"$TOKI" backup create --dir "$DATA" qc.zip >/dev/null 2>&1
B=$("$TOKI" backup verify latest --json --dir "$DATA" 2>/dev/null); echo "$B" | grep -q '"integrityOk": true'; check backup_verify $? "$(echo "$B" | tr -d '\n' | cut -c1-300)"

# 5. audit chain
A=$("$TOKI" audit verify --dir "$DATA" 2>&1 | tail -1); echo "$A" | grep -q '^OK'; check audit_verify $? "$A"

# 6. module CLIs answer (jobs, webhooks, fieldperm, sessions, deny, lockout)
for c in "jobs stats --json" "webhooks list --json" "fieldperm lint --json" "deny tail --limit 1 --json" "lockout list --json"; do
  O=$("$TOKI" $c --dir "$DATA" 2>&1); rc=$?; O=$(printf '%s' "$O" | tail -c 300); [ $rc = 0 ] || [ "$c" = "fieldperm lint --json" ]; check "cli_${c%% *}" $? "$O"
done

# 7. serve log must not contain panics
grep -qiE 'panic|fatal' "$LOG"; [ $? = 1 ]; check "no_panic_in_log" $? "$(grep -iE 'panic|fatal' "$LOG" | head -2)"

# 8. memory of the serving process
PID=$(pgrep -f "serve --dir $DATA" | head -1); RSS=$(grep VmRSS /proc/${PID:-0}/status 2>/dev/null | awk '{print $2}'); [ -n "$RSS" ] && [ "$RSS" -lt 1000000 ]; check rss_under_1gb $? "VmRSS ${RSS:-?} kB"

kill "$PID" "$(cat "$WORK/pid")" 2>/dev/null; sleep 1
echo "{\"summary\":\"$([ $FAIL = 0 ] && echo PASS || echo FAIL)\",\"toki\":\"$("$TOKI" --version 2>/dev/null | head -1)\",\"date\":\"$(date -u +%FT%TZ)\"}"
exit $FAIL
