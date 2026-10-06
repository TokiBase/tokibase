#!/usr/bin/env bash
# Failover drill: primary replicates to file://, is killed (SIGKILL), the replica is
# promoted into a standby pb_data and served on another port. Prints the RTO.
# Fails when records are missing or the RTO exceeds TOKI_RTO_MAX (default 30 s).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
RTO_MAX="${TOKI_RTO_MAX:-30}"
EMAIL=admin@example.com
PASS=adminpass1234
N=10

TMP="$(mktemp -d "${TMPDIR:-/tmp}/toki-failover.XXXXXX")"
TOKI="$TMP/toki"
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && { kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; }; done
  rm -rf "$TMP"
}
trap cleanup EXIT
log() { echo "[failover] $*" >&2; }
now() { python3 -c 'import time; print(time.time())'; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; } # reads JSON on stdin; $1 is ONE expression
fail() { log "FAIL: $*"; for f in "$TMP"/*.log; do echo "--- $f"; tail -30 "$f"; done >&2; exit 1; }
wait_health() { # base-url seconds
  local i
  for ((i = 0; i < $2 * 10; i++)); do
    curl -fs "$1/api/health" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}

cd "$ROOT"
log "building"
go build -o "$TOKI" ./examples/base

REP="$TMP/rep"
PRIMARY="$TMP/primary"
STANDBY="$TMP/standby"
P1=$((20000 + RANDOM % 10000))
P2=$((P1 + 10000))
B1="http://127.0.0.1:$P1"
B2="http://127.0.0.1:$P2"

"$TOKI" superuser upsert "$EMAIL" "$PASS" --dir "$PRIMARY" >/dev/null
TOKI_REPLICA_URL="file://$REP" TOKI_REPLICA_SYNC_INTERVAL=200ms \
  "$TOKI" serve --dir "$PRIMARY" --http "127.0.0.1:$P1" >"$TMP/primary.log" 2>&1 &
PRIMARY_PID=$!
PIDS+=("$PRIMARY_PID")
wait_health "$B1" 30 || fail "primary did not start"

TOKEN=$(curl -fsS "$B1/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["token"]')
api() { curl -fsS -X "$1" "$B1$2" -H "Authorization: $TOKEN" -H 'Content-Type: application/json' -d "$3"; }

api POST /api/collections '{"name":"posts","type":"base","listRule":"","viewRule":"","fields":[{"name":"title","type":"text","required":true}]}' >/dev/null
for i in $(seq 1 $N); do api POST /api/collections/posts/records "{\"title\":\"post $i\"}" >/dev/null; done
log "seeded $N records"

# wait until the replica holds everything the primary wrote
synced=0
for _ in $(seq 1 100); do
  ok=$(curl -fsS "$B1/api/health" -H "Authorization: $TOKEN" | jget 'int(len(d["data"].get("replica",{}).get("databases",[]))==2 and d["data"]["replica"].get("healthy") is True and all(x["localTxid"]>0 and x["replicaTxid"]>=x["localTxid"] for x in d["data"]["replica"]["databases"]))' || echo 0)
  [ "$ok" = 1 ] && { synced=1; break; }
  sleep 0.2
done
[ "$synced" = 1 ] || fail "replica did not catch up"
"$TOKI" replica status --url "file://$REP" --json >/dev/null || fail "replica status failed"
log "replica in sync"

# --- disaster ---
T0=$(now)
kill -9 "$PRIMARY_PID"
wait "$PRIMARY_PID" 2>/dev/null || true
log "primary killed"

"$TOKI" replica promote --url "file://$REP" --dir "$STANDBY" >"$TMP/promote.log" 2>&1 || fail "promote failed"
[ -f "$STANDBY/.toki-promoted.json" ] || fail "promoted marker missing"

TOKI_REPLICA_URL="file://$TMP/rep-standby" "$TOKI" serve --dir "$STANDBY" --http "127.0.0.1:$P2" >"$TMP/standby.log" 2>&1 &
PIDS+=("$!")
wait_health "$B2" 60 || fail "standby did not become healthy"
T1=$(now)

count=$(curl -fsS "$B2/api/collections/posts/records?perPage=1" | jget 'd["totalItems"]')
[ "$count" = "$N" ] || fail "standby has $count records, want $N"

RTO=$(python3 -c "print(f'{$T1 - $T0:.1f}')")
log "standby serves $count/$N records"
echo "RTO: ${RTO}s (kill -> standby healthy, promote included); RPO: 0 records lost (replica was in sync)"
python3 -c "import sys; sys.exit(0 if $RTO <= $RTO_MAX else 1)" || fail "RTO ${RTO}s exceeds ${RTO_MAX}s"
