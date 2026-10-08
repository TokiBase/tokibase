#!/usr/bin/env bash
# Sync e2e (docs/SYNC_DESIGN.md §9, PR7 snapshot bootstrap / stale re-bootstrap / hub restore cases at the end, PR3 + PR5 field-merge and hook/park cases + PR6 partition, purge and compaction cases): one hub and two spokes, each with its own pb_data,
# on random loopback ports. Writes on all three (including concurrent edits of one record),
# converge, compare `toki sync verify` digests, SIGKILL spoke 1 in the middle of a push,
# restart it and converge again. Everything is killed by PID file at exit.
#
# TMPDIR must allow executing binaries (the binary is built into the work dir).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
EMAIL=admin@example.com
PASS=adminpass1234
BULK="${TOKI_E2E_BULK:-200}"
LIMIT="${TOKI_E2E_TIMEOUT:-60}"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/toki-e2e-sync.XXXXXX")"
TOKI="$TMP/toki"
cleanup() {
  local f p
  for f in "$TMP"/*.pid; do
    [ -f "$f" ] || continue
    p="$(cat "$f" 2>/dev/null || true)"
    [ -n "$p" ] && { kill -9 "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; }
  done
  rm -rf "$TMP"
}
trap cleanup EXIT
log() { echo "[sync] $*" >&2; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; } # one expression over d
fail() {
  log "FAIL: $*"
  for f in "$TMP"/*.log; do echo "--- $f" >&2; tail -25 "$f" >&2; done
  exit 1
}

cd "$ROOT"
log "building"
go build -o "$TOKI" ./examples/base

PORT_HUB=$((20000 + RANDOM % 10000))
PORT_S1=$((PORT_HUB + 1000))
PORT_S2=$((PORT_HUB + 2000))
URL_HUB="http://127.0.0.1:$PORT_HUB"
URL_S1="http://127.0.0.1:$PORT_S1"
URL_S2="http://127.0.0.1:$PORT_S2"

toki() { # role dir args...
  local role="$1" dir="$2"; shift 2
  TOKI_SYNC_ROLE="$role" TOKI_SYNC_INSECURE=1 "$TOKI" "$@" --dir "$dir"
}
start() { # name role dir port
  TOKI_SYNC_ROLE="$2" TOKI_SYNC_INSECURE=1 TOKI_SYNC_INTERVAL=1s TOKI_SYNC_PAGE=25 TOKI_SYNC_SNAPSHOT_PAGE=500 \
    "$TOKI" serve --automigrate=false --dir "$3" --http "127.0.0.1:$4" >>"$TMP/$1.log" 2>&1 &
  echo $! >"$TMP/$1.pid"
}
wait_health() { # url seconds
  local i
  for ((i = 0; i < $2 * 10; i++)); do
    curl -fs "$1/api/health" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}
token() { curl -fsS "$1/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["token"]'; }
api() { # token method url path body
  local out code
  out="$(curl -sS -w '\n%{http_code}' -X "$2" "$3$4" -H "Authorization: $1" -H 'Content-Type: application/json' -d "${5:-}")" || return 1
  code="${out##*$'\n'}"
  if [ "${code:0:1}" != 2 ]; then echo "[sync] HTTP $code on $2 $3$4: ${out%$'\n'*}" >&2; return 1; fi
  printf '%s' "${out%$'\n'*}"
}

HUB="$TMP/hub"; S1="$TMP/s1"; S2="$TMP/s2"

# ---- hub ----
toki hub "$HUB" superuser upsert "$EMAIL" "$PASS" >/dev/null
start hub hub "$HUB" "$PORT_HUB"
wait_health "$URL_HUB" 30 || fail "hub did not start"
TH="$(token "$URL_HUB")"
# the service actor of the spokes: pushed changes are replayed as this hub record (docs/modules/sync.md)
SU_ID="$(curl -fsS "$URL_HUB/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["record"]["id"]')"
[ -n "$SU_ID" ] || fail "no hub superuser id"

COLL='{"id":"pbc_e2eitems","name":"e2eitems","type":"base","listRule":"","viewRule":"","createRule":"","updateRule":"","deleteRule":"","fields":[{"name":"title","type":"text"},{"name":"qty","type":"number"},{"name":"note","type":"text"},{"name":"created","type":"autodate","onCreate":true},{"name":"updated","type":"autodate","onCreate":true,"onUpdate":true}]}'
api "$TH" POST "$URL_HUB" /api/collections "$COLL" >/dev/null
api "$TH" POST "$URL_HUB" /api/collections/_sync_policies/records \
  '{"collection":"e2eitems","direction":"both","enabled":true,"field_types":{"qty":"counter"}}' >/dev/null
log "hub up on $PORT_HUB with collection e2eitems and policy direction=both"

# ---- spokes: enroll, start, create the same collection (schema bundles are PR8) ----
enroll_spoke() { # name dir port url
  local code
  code="$(toki hub "$HUB" sync enroll --name "$1" --profile edge --actor "_superusers/$SU_ID" --allow-superuser-actor | awk '/^code:/ {print $2}')"
  [ -n "$code" ] || fail "no enrollment code for $1"
  toki spoke "$2" superuser upsert "$EMAIL" "$PASS" >/dev/null
  toki spoke "$2" sync join "$URL_HUB" "$code" >/dev/null || fail "join $1"
  start "$1" spoke "$2" "$3"
  wait_health "$4" 30 || fail "$1 did not start"
  local t
  t="$(token "$4")"
  api "$t" POST "$4" /api/collections "$COLL" >/dev/null
}
enroll_spoke s1 "$S1" "$PORT_S1" "$URL_S1"
enroll_spoke s2 "$S2" "$PORT_S2" "$URL_S2"
T1="$(token "$URL_S1")"; T2="$(token "$URL_S2")"

# the policy arrives with the next handshake
for ((i = 0; i < 100; i++)); do
  n1=$(api "$T1" GET "$URL_S1" "/api/collections/_sync_policies/records" | jget 'd["totalItems"]')
  n2=$(api "$T2" GET "$URL_S2" "/api/collections/_sync_policies/records" | jget 'd["totalItems"]')
  [ "$n1" = 1 ] && [ "$n2" = 1 ] && break
  sleep 0.2
done
[ "$n1" = 1 ] && [ "$n2" = 1 ] || fail "spokes did not receive the policy"
sleep 6 # let the policy cache (5 s ttl) settle on the spokes
log "spokes enrolled and running"

create() { # token url title qty -> id
  api "$1" POST "$2" /api/collections/e2eitems/records "{\"title\":\"$3\",\"qty\":${4:-0}}" | jget 'd["id"]'
}
count() { api "$1" GET "$2" "/api/collections/e2eitems/records?perPage=1" | jget 'd["totalItems"]'; }

digest() { # role dir -> "records digest pending" (or "- - -")
  local out
  out="$(toki "$1" "$2" sync verify --json 2>/dev/null | grep '^{' | tail -1 || true)"
  [ -n "$out" ] || { echo "- - -"; return; }
  echo "$out" | jget '(lambda c: (str(c["records"])+" "+c["digest"]+" "+str(d["pending"])) if c else "- - -")(d["collections"][0] if d["collections"] else None)'
}
converged() { # prints status; returns 0 when all three agree and nothing is pending
  local h a b
  h="$(digest hub "$HUB")"; a="$(digest spoke "$S1")"; b="$(digest spoke "$S2")"
  CONV="hub[$h] s1[$a] s2[$b]"
  [ "${h%% *}" != "-" ] || return 1
  [ "${h%% *}" = "${a%% *}" ] && [ "${a%% *}" = "${b%% *}" ] || return 1
  local hd="${h#* }"; hd="${hd%% *}"; local ad="${a#* }"; ad="${ad%% *}"; local bd="${b#* }"; bd="${bd%% *}"
  [ "$hd" = "$ad" ] && [ "$ad" = "$bd" ] || return 1
  [ "${a##* }" = 0 ] && [ "${b##* }" = 0 ] || return 1
  return 0
}
wait_converged() { # label
  local t0=$SECONDS
  while ! converged; do
    [ $((SECONDS - t0)) -lt "$LIMIT" ] || fail "$1: no convergence within ${LIMIT}s: $CONV"
    sleep 1
  done
  log "$1: converged in $((SECONDS - t0))s ($CONV)"
}

# ---- 1. writes on all three ----
for i in 1 2 3 4 5; do
  create "$TH" "$URL_HUB" "hub $i" >/dev/null
  create "$T1" "$URL_S1" "s1 $i" >/dev/null
  create "$T2" "$URL_S2" "s2 $i" >/dev/null
done
wait_converged "round 1 (15 records)"
[ "$(count "$TH" "$URL_HUB")" = 15 ] || fail "hub should hold 15 records"

# ---- 2. concurrent edits of one record (lww) plus counters ----
RID="$(create "$TH" "$URL_HUB" "shared" 10)"
wait_converged "shared record replicated"
patch() { api "$1" PATCH "$2" "/api/collections/e2eitems/records/$RID" "$3" >/dev/null; }
patch "$TH" "$URL_HUB" '{"title":"edited on hub"}' &
P1=$!
patch "$T1" "$URL_S1" '{"title":"edited on s1","qty+":3}' &
P2=$!
patch "$T2" "$URL_S2" '{"title":"edited on s2","qty+":4}' &
P3=$!
wait "$P1" "$P2" "$P3"
wait_converged "round 2 (concurrent edits)"
QTY="$(api "$TH" GET "$URL_HUB" "/api/collections/e2eitems/records/$RID" | jget 'd["qty"]')"
[ "$QTY" = 17 ] || fail "counters never conflict: expected qty 17, got $QTY"
TITLE="$(api "$TH" GET "$URL_HUB" "/api/collections/e2eitems/records/$RID" | jget 'd["title"]')"
case "$TITLE" in "edited on hub"|"edited on s1"|"edited on s2") ;; *) fail "unexpected title '$TITLE'";; esac

# ---- 3. delete from a spoke ----
DEL="$(create "$T2" "$URL_S2" "to delete")"
wait_converged "delete target replicated"
api "$T2" DELETE "$URL_S2" "/api/collections/e2eitems/records/$DEL" >/dev/null
wait_converged "round 3 (delete)"
[ "$(count "$TH" "$URL_HUB")" = 16 ] || fail "hub should hold 16 records after the delete"

# ---- 4. SIGKILL spoke 1 in the middle of a push ----
log "creating $BULK records on s1 and killing it mid-push"
BASE="$(count "$TH" "$URL_HUB")"
(
  for ((i = 1; i <= BULK; i++)); do
    create "$T1" "$URL_S1" "bulk $i" >/dev/null 2>&1 || exit 0
  done
) &
BULK_PID=$!
KILLED=0
for ((i = 0; i < 600; i++)); do
  n="$(count "$TH" "$URL_HUB" 2>/dev/null || echo "$BASE")"
  if [ "$n" -gt $((BASE + 10)) ]; then
    kill -9 "$(cat "$TMP/s1.pid")"
    wait "$(cat "$TMP/s1.pid")" 2>/dev/null || true
    KILLED=1
    log "s1 killed with $((n - BASE)) of its records on the hub"
    break
  fi
  sleep 0.05
done
wait "$BULK_PID" 2>/dev/null || true
[ "$KILLED" = 1 ] || log "note: s1 finished pushing before it could be killed (fast machine); restarting it anyway"
[ "$KILLED" = 1 ] || { kill -9 "$(cat "$TMP/s1.pid")" 2>/dev/null || true; wait "$(cat "$TMP/s1.pid")" 2>/dev/null || true; }

start s1 spoke "$S1" "$PORT_S1"
wait_health "$URL_S1" 30 || fail "s1 did not restart"
wait_converged "round 4 (after SIGKILL and restart)"
HUBN="$(count "$TH" "$URL_HUB")"
S1N="$(count "$(token "$URL_S1")" "$URL_S1")"
[ "$HUBN" = "$S1N" ] || fail "record counts differ: hub $HUBN, s1 $S1N"
log "hub holds $HUBN records, identical on all three nodes"

# ---- 5. verify: a spoke compares its metadata digest with the hub ----
toki spoke "$S2" sync verify --against-hub --json 2>/dev/null | grep '^{' | tail -1 >"$TMP/against.json" || true; [ -s "$TMP/against.json" ] || fail "verify --against-hub failed: $(cat "$TMP/against.json" 2>/dev/null)"
jget 'd["against_hub"]["checked"] is True and d["against_hub"]["mismatch"] == []' <"$TMP/against.json" | grep -q True || fail "hub reported digest differences: $(cat "$TMP/against.json")"

# ---- 6. field-merge (PR5): concurrent edits of DIFFERENT fields both survive ----
PID="$(api "$TH" GET "$URL_HUB" "/api/collections/_sync_policies/records" | jget 'd["items"][0]["id"]')"
api "$TH" PATCH "$URL_HUB" "/api/collections/_sync_policies/records/$PID" '{"strategy":"field-merge"}' >/dev/null
FM="$(create "$TH" "$URL_HUB" "fm base" 0)"
wait_converged "field-merge record replicated"
api "$T1" PATCH "$URL_S1" "/api/collections/e2eitems/records/$FM" '{"title":"fm title from s1"}' >/dev/null
api "$T2" PATCH "$URL_S2" "/api/collections/e2eitems/records/$FM" '{"note":"fm note from s2"}' >/dev/null
wait_converged "round 6 (field-merge)"
FMR="$(api "$TH" GET "$URL_HUB" "/api/collections/e2eitems/records/$FM")"
[ "$(echo "$FMR" | jget 'd["title"]')" = "fm title from s1" ] || fail "field-merge lost the title edit: $FMR"
[ "$(echo "$FMR" | jget 'd["note"]')" = "fm note from s2" ] || fail "field-merge lost the note edit: $FMR"

# ---- 7. hook strategy without a wasm module fails closed (parked), CLI resolves it (PR5) ----
api "$TH" PATCH "$URL_HUB" "/api/collections/_sync_policies/records/$PID" '{"strategy":"hook"}' >/dev/null
HK="$(create "$TH" "$URL_HUB" "hook base" 0)"
wait_converged "hook record replicated"
api "$T1" PATCH "$URL_S1" "/api/collections/e2eitems/records/$HK" '{"title":"s1 side"}' >/dev/null
api "$TH" PATCH "$URL_HUB" "/api/collections/e2eitems/records/$HK" '{"title":"hub side"}' >/dev/null
OPEN=""
for ((i = 0; i < 60; i++)); do
  OPEN="$(toki hub "$HUB" sync conflicts --open --json 2>/dev/null | grep -E '^\[(\{|\])' | tail -1 || true)"
  [ -n "$OPEN" ] && [ "$(echo "$OPEN" | jget 'len(d)')" = 1 ] && break
  sleep 1
done
[ -n "$OPEN" ] && [ "$(echo "$OPEN" | jget 'len(d)')" = 1 ] || fail "expected one open conflict, got: $OPEN"
[ "$(echo "$OPEN" | jget 'd[0]["kind"]')" = hook_failed ] || fail "expected a hook_failed conflict: $OPEN"
CID="$(echo "$OPEN" | jget 'd[0]["id"]')"
[ "$(api "$TH" GET "$URL_HUB" "/api/collections/e2eitems/records/$HK" | jget 'd["title"]')" = "hub side" ] || fail "a parked change must not be applied"
toki hub "$HUB" sync conflicts --resolve "$CID" --take incoming --note "e2e" >/dev/null || fail "resolve"
wait_converged "round 7 (hook park + resolve)"
[ "$(api "$TH" GET "$URL_HUB" "/api/collections/e2eitems/records/$HK" | jget 'd["title"]')" = "s1 side" ] || fail "resolve --take incoming must apply the parked patch"
[ "$(toki hub "$HUB" sync conflicts --open --json 2>/dev/null | grep -E '^\[(\{|\])' | tail -1 | jget 'len(d)')" = 0 ] || fail "no open conflict should remain"

# ---- 8. partitions (PR6): gate-1 is branch A, gate-2 is branch B ----
wait_for() { # label expression: poll until the expression (a shell command) succeeds
  local t0=$SECONDS
  while ! eval "$2"; do
    [ $((SECONDS - t0)) -lt "$LIMIT" ] || fail "$1: timed out waiting for: $2"
    sleep 1
  done
}
http() { # token method url path [body] -> status code
  curl -s -o /dev/null -w '%{http_code}' -X "$2" "$3$4" -H "Authorization: $1" -H 'Content-Type: application/json' -d "${5:-}"
}
kill_node() { local p; p="$(cat "$TMP/$1.pid")"; kill -9 "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; }

COLL2='{"id":"pbc_e2etickets","name":"e2etickets","type":"base","listRule":"","viewRule":"","createRule":"","updateRule":"","deleteRule":"","fields":[{"name":"title","type":"text"},{"name":"branch","type":"text"},{"name":"created","type":"autodate","onCreate":true},{"name":"updated","type":"autodate","onCreate":true,"onUpdate":true}],"indexes":["CREATE INDEX idx_e2etickets_branch ON e2etickets (branch)"]}'
api "$TH" POST "$URL_HUB" /api/collections "$COLL2" >/dev/null
api "$T1" POST "$URL_S1" /api/collections "$COLL2" >/dev/null
api "$T2" POST "$URL_S2" /api/collections "$COLL2" >/dev/null
api "$TH" POST "$URL_HUB" /api/collections/_sync_policies/records \
  '{"collection":"e2etickets","direction":"both","enabled":true,"partition":"branch = @node.branch"}' >/dev/null || fail "partition policy rejected"
# a policy with a bad partition is refused by the validation
[ "$(http "$TH" POST "$URL_HUB" /api/collections/_sync_policies/records '{"collection":"e2eitems","partition":"nope = @node.x","enabled":true}')" = 400 ] \
  || fail "a policy with an unknown partition field must be refused"
# partition parameters are node data on the hub
PEERS="$(toki hub "$HUB" sync peers --json 2>/dev/null | grep -E '^\[(\{|\])' | tail -1)"
for pair in "s1:A" "s2:B"; do
  nid="$(echo "$PEERS" | jget "[p['id'] for p in d if p['name']=='${pair%%:*}'][0]")"
  api "$TH" PATCH "$URL_HUB" "/api/collections/_sync_nodes/records/$nid" "{\"params\":{\"branch\":\"${pair##*:}\"}}" >/dev/null || fail "set params of ${pair%%:*}"
done
# the spokes get the new policy with their next handshake: restart them
kill_node s1; kill_node s2
start s1 spoke "$S1" "$PORT_S1"; start s2 spoke "$S2" "$PORT_S2"
wait_health "$URL_S1" 30 || fail "s1 did not restart"; wait_health "$URL_S2" 30 || fail "s2 did not restart"
T1="$(token "$URL_S1")"; T2="$(token "$URL_S2")"
wait_for "policies on the spokes" '[ "$(api "$T1" GET "$URL_S1" "/api/collections/_sync_policies/records" | jget "d[\"totalItems\"]")" = 2 ] && [ "$(api "$T2" GET "$URL_S2" "/api/collections/_sync_policies/records" | jget "d[\"totalItems\"]")" = 2 ]'
sleep 6 # policy cache ttl on the spokes

tcreate() { # token url title branch -> id
  api "$1" POST "$2" /api/collections/e2etickets/records "{\"title\":\"$3\",\"branch\":\"$4\"}" | jget 'd["id"]'
}
tget() { http "$1" GET "$2" "/api/collections/e2etickets/records/$3"; }
TA="$(tcreate "$TH" "$URL_HUB" "ticket A" A)"
TB="$(tcreate "$TH" "$URL_HUB" "ticket B" B)"
wait_for "partition pull" '[ "$(tget "$T1" "$URL_S1" "$TA")" = 200 ] && [ "$(tget "$T2" "$URL_S2" "$TB")" = 200 ]'
[ "$(tget "$T1" "$URL_S1" "$TB")" = 404 ] || fail "gate-1 (branch A) must not receive the ticket of branch B"
[ "$(tget "$T2" "$URL_S2" "$TA")" = 404 ] || fail "gate-2 (branch B) must not receive the ticket of branch A"
log "partition pull: each gate holds only its own branch"

# an in-partition write travels up, a write outside the partition is refused and reverted
TS1="$(tcreate "$T1" "$URL_S1" "from gate-1" A)"
TOUT="$(tcreate "$T1" "$URL_S1" "gate-1 writes branch B" B)"
wait_for "partition push" '[ "$(tget "$TH" "$URL_HUB" "$TS1")" = 200 ] && [ "$(tget "$T1" "$URL_S1" "$TOUT")" = 404 ]'
[ "$(tget "$TH" "$URL_HUB" "$TOUT")" = 404 ] || fail "the hub must refuse a record outside the partition of the node"
[ "$(tget "$T2" "$URL_S2" "$TS1")" = 404 ] || fail "gate-2 must not receive a ticket of branch A"
log "partition push: in-partition accepted, out-of-partition refused and reverted"

# moving a record to the other branch evicts it on gate-1 and delivers it to gate-2
api "$TH" PATCH "$URL_HUB" "/api/collections/e2etickets/records/$TA" '{"branch":"B"}' >/dev/null
wait_for "partition move" '[ "$(tget "$T1" "$URL_S1" "$TA")" = 404 ] && [ "$(tget "$T2" "$URL_S2" "$TA")" = 200 ]'
log "partition move: evicted on gate-1, delivered to gate-2"

# ---- 9. purge (PR6): the record, its log patches and the copies on all nodes are erased ----
PRG="$(create "$TH" "$URL_HUB" "personal data" 1)"
wait_for "purge target replicated" '[ "$(http "$T1" GET "$URL_S1" "/api/collections/e2eitems/records/$PRG")" = 200 ] && [ "$(http "$T2" GET "$URL_S2" "/api/collections/e2eitems/records/$PRG")" = 200 ]'
toki hub "$HUB" sync purge e2eitems "$PRG" --legal --reason "e2e erasure" >"$TMP/purge.json" 2>/dev/null || fail "purge command failed"
wait_for "purge propagated" '[ "$(http "$TH" GET "$URL_HUB" "/api/collections/e2eitems/records/$PRG")" = 404 ] && [ "$(http "$T1" GET "$URL_S1" "/api/collections/e2eitems/records/$PRG")" = 404 ] && [ "$(http "$T2" GET "$URL_S2" "/api/collections/e2eitems/records/$PRG")" = 404 ]'
# no patch survives in the log; the legal tombstone exists on all three nodes
for dir in "$HUB" "$S1" "$S2"; do
  python3 - "$dir/data.db" "$PRG" <<'PY' || fail "purge check failed in $dir"
import sqlite3, sys
db = sqlite3.connect(sys.argv[1], timeout=10)
rid = sys.argv[2]
kinds = [r[0] for r in db.execute("select kind from _sync_tombstones where record=?", (rid,))]
assert kinds == ["legal"], kinds
for op, patch, h in db.execute("select op, patch, hash from _changes where record=? and op!='p'", (rid,)):
    assert patch == "{}" and h is None, (op, patch)
PY
done
# the id can never be created again, on the hub or on a spoke
[ "$(http "$TH" POST "$URL_HUB" /api/collections/e2eitems/records "{\"id\":\"$PRG\",\"title\":\"zombie\"}")" = 400 ] || fail "a purged id must be refused on the hub"
[ "$(http "$T1" POST "$URL_S1" /api/collections/e2eitems/records "{\"id\":\"$PRG\",\"title\":\"zombie\"}")" = 400 ] || fail "a purged id must be refused on a spoke"
log "purge: erased everywhere, legal tombstone in place"

# ---- 10. compaction (PR6): nothing is old enough, the report is sane ----
toki hub "$HUB" sync compact --json 2>/dev/null | grep '^{' | tail -1 >"$TMP/compact.json" || true
[ -s "$TMP/compact.json" ] || fail "compact printed nothing"
jget 'd["role"]=="hub" and d["changes_deleted"]==0 and d["stale_nodes"]==0' <"$TMP/compact.json" | grep -q True || fail "unexpected compaction report: $(cat "$TMP/compact.json")"
curl -fsS "$URL_HUB/api/health" -H "Authorization: $TH" | jget 'd["data"]["sync"]["role"]=="hub" and d["data"]["sync"]["stale_nodes"]==0' | grep -q True || fail "health block missing"

# ---- 11. snapshot bootstrap (PR7): compaction first, THEN a brand-new spoke enrolls ----
SEED="${TOKI_E2E_SEED:-20000}"
log "seeding $SEED records on the hub through /api/batch"
api "$TH" PATCH "$URL_HUB" /api/settings '{"batch":{"enabled":true,"maxRequests":1000,"timeout":60,"maxBodySize":0}}' >/dev/null || fail "batch settings"
python3 - "$SEED" "$TMP" <<'PY'
import json, sys
n, tmp = int(sys.argv[1]), sys.argv[2]
per = 500
for k in range(0, n, per):
    reqs = [{"method": "POST", "url": "/api/collections/e2eitems/records",
             "body": {"title": "seed %d" % i, "qty": i % 7, "note": "n" * 20}} for i in range(k, min(k + per, n))]
    json.dump({"requests": reqs}, open("%s/batch-%05d.json" % (tmp, k // per), "w"))
PY
for f in "$TMP"/batch-*.json; do
  code="$(curl -s -o "$TMP/batch.out" -w '%{http_code}' -X POST "$URL_HUB/api/batch" -H "Authorization: $TH" -H 'Content-Type: application/json' --data-binary "@$f")"
  [ "$code" = 200 ] || fail "batch seed $f: HTTP $code $(head -c 300 "$TMP/batch.out")"
  rm -f "$f"
done
LIMIT_SAVE="$LIMIT"; LIMIT=240
wait_converged "seeded records replicated to s1 and s2"
LIMIT="$LIMIT_SAVE"
HUBTOTAL="$(count "$TH" "$URL_HUB")"
[ "$HUBTOTAL" -ge "$SEED" ] || fail "hub holds only $HUBTOTAL records"

# the hub forgets its whole log (MIN_KEEP=1ms), so every NEW node starts below low_water
sleep 1
TOKI_SYNC_MIN_KEEP=1ms toki hub "$HUB" sync compact --json 2>/dev/null | grep '^{' | tail -1 >"$TMP/compact2.json" || true
jget 'd["changes_deleted"] > 0 and d["low_water"] > 0' <"$TMP/compact2.json" | grep -q True || fail "compaction did not raise low_water: $(cat "$TMP/compact2.json")"

PORT_S3=$((PORT_HUB + 3000)); URL_S3="http://127.0.0.1:$PORT_S3"; S3="$TMP/s3"
CODE3="$(toki hub "$HUB" sync enroll --name s3 --profile edge --actor "_superusers/$SU_ID" | awk '/^code:/ {print $2}')"
[ -n "$CODE3" ] || fail "no enrollment code for s3"
toki spoke "$S3" superuser upsert "$EMAIL" "$PASS" >/dev/null
toki spoke "$S3" sync join "$URL_HUB" "$CODE3" >/dev/null || fail "join s3"
# the collections do NOT exist on s3: the snapshot creates them
start s3 spoke "$S3" "$PORT_S3"
wait_health "$URL_S3" 30 || fail "s3 did not start"
T3="$(token "$URL_S3")"

# SIGKILL s3 in the middle of the snapshot
KILLED=0
for ((i = 0; i < 1200; i++)); do
  n="$(python3 -c "
import sqlite3,sys
try: print(sqlite3.connect(sys.argv[1], timeout=5).execute('select count(*) from e2eitems').fetchone()[0])
except Exception: print(0)" "$S3/data.db")"
  if [ "${n:-0}" -gt 3000 ]; then
    ST="$(toki spoke "$S3" sync status --json 2>/dev/null | grep '^{' | tail -1 | jget 'd.get("state","")' || true)"
    kill_node s3
    KILLED=1
    log "s3 killed mid-snapshot with $n records (state $ST)"
    break
  fi
  sleep 0.1
done
[ "$KILLED" = 1 ] || fail "s3 finished the snapshot before it could be killed (raise TOKI_E2E_SEED)"
[ "$n" -lt "$HUBTOTAL" ] || fail "s3 was already complete when it was killed"
[ "$ST" = bootstrapping ] || fail "s3 should be in state bootstrapping, was '$ST'"
start s3 spoke "$S3" "$PORT_S3"
wait_health "$URL_S3" 30 || fail "s3 did not restart"

converged3() {
  local h c
  h="$(digest hub "$HUB")"; c="$(digest spoke "$S3")"
  CONV="hub[$h] s3[$c]"
  [ "${h%% *}" != "-" ] && [ "${h%% *}" = "${c%% *}" ] || return 1
  local hd="${h#* }"; hd="${hd%% *}"; local cd="${c#* }"; cd="${cd%% *}"
  [ "$hd" = "$cd" ] && [ "${c##* }" = 0 ]
}
wait_for3() {
  local t0=$SECONDS
  while ! converged3; do
    [ $((SECONDS - t0)) -lt 240 ] || fail "$1: s3 did not converge: $CONV"
    sleep 1
  done
  log "$1: s3 converged in $((SECONDS - t0))s ($CONV)"
}
wait_for3 "round 11 (snapshot resumed after SIGKILL)"
[ "$(toki spoke "$S3" sync status --json 2>/dev/null | grep '^{' | tail -1 | jget 'd["state"]')" = idle ] || fail "s3 should be idle"
[ "$(toki hub "$HUB" sync peers --json 2>/dev/null | grep -E '^\[(\{|\])' | tail -1 | jget "[p['status'] for p in d if p['name']=='s3'][0]")" = active ] || fail "s3 should be active on the hub"
# it also received the schema of the collection it did not have, and keeps syncing
[ "$(api "$T3" GET "$URL_S3" "/api/collections/e2etickets" | jget 'd["name"]')" = e2etickets ] || fail "the snapshot must create the missing collection"
create "$TH" "$URL_HUB" "after bootstrap" >/dev/null
wait_for3 "round 11b (live changes after the bootstrap)"
toki spoke "$S3" sync verify --against-hub --json 2>/dev/null | grep '^{' | tail -1 >"$TMP/against3.json" || true
jget 'd["against_hub"]["mismatch"] == []' <"$TMP/against3.json" | grep -q True || fail "s3 differs from the hub: $(cat "$TMP/against3.json")"

# ---- 12. a node silent for longer than the retention (fake clock) is stale and re-bootstraps ----
log "stale node: s3 goes offline, the hub compacts with a clock 59m50s ahead and 1h retention"
kill_node s3
for i in 1 2 3; do create "$TH" "$URL_HUB" "while s3 was away $i" >/dev/null; done
wait_converged "hub, s1, s2 converged while s3 is away"
sleep 12 # s3 has been silent for more than 10 s; s1 and s2 pull every second
TOKI_SYNC_TEST=1 TOKI_SYNC_TEST_CLOCK_OFFSET=3590s TOKI_SYNC_RETENTION=1h TOKI_SYNC_MIN_KEEP=1ms \
  toki hub "$HUB" sync compact --json 2>/dev/null | grep '^{' | tail -1 >"$TMP/compact3.json" || true
jget 'd["stale_nodes"] == 1' <"$TMP/compact3.json" | grep -q True || fail "exactly s3 should be stale: $(cat "$TMP/compact3.json")"
PEERS="$(toki hub "$HUB" sync peers --json 2>/dev/null | grep -E '^\[(\{|\])' | tail -1)"
[ "$(echo "$PEERS" | jget "[p['status'] for p in d if p['name']=='s3'][0]")" = stale ] || fail "s3 should be stale: $PEERS"
start s3 spoke "$S3" "$PORT_S3"
wait_health "$URL_S3" 30 || fail "s3 did not restart"
wait_for3 "round 12 (stale node re-bootstrapped)"
PEERS="$(toki hub "$HUB" sync peers --json 2>/dev/null | grep -E '^\[(\{|\])' | tail -1)"
[ "$(echo "$PEERS" | jget "[p['status'] for p in d if p['name']=='s3'][0]")" = active ] || fail "s3 should be active again: $PEERS"
wait_converged "all four nodes agree after the stale re-bootstrap"

# ---- 13. hub restore from a backup: new epoch, the spokes send their changes again, nothing is lost ----
api "$TH" POST "$URL_HUB" /api/backups '{"name":"e2e-sync.zip"}' >/dev/null || fail "backup create"
sleep 2
EPOCH0="$(toki hub "$HUB" sync status --json 2>/dev/null | grep '^{' | tail -1 | jget 'd["epoch"]')"
for i in 1 2 3 4 5; do create "$T1" "$URL_S1" "after backup s1 $i" >/dev/null; create "$T2" "$URL_S2" "after backup s2 $i" >/dev/null; done
wait_converged "writes after the backup replicated"
TOTAL="$(count "$TH" "$URL_HUB")"
log "restoring the hub backup (hub holds $TOTAL records, 10 of them newer than the backup)"
api "$TH" POST "$URL_HUB" /api/backups/e2e-sync.zip/restore '' >/dev/null || fail "restore request"
sleep 3
wait_health "$URL_HUB" 60 || fail "hub did not come back after the restore"
TH="$(token "$URL_HUB")"
EPOCH1="$(toki hub "$HUB" sync status --json 2>/dev/null | grep '^{' | tail -1 | jget 'd["epoch"]')"
[ -n "$EPOCH1" ] && [ "$EPOCH1" != "$EPOCH0" ] || fail "the hub epoch must change after a restore ($EPOCH0 -> $EPOCH1)"
LIMIT_SAVE="$LIMIT"; LIMIT=120
wait_converged "round 13 (after the hub restore)"
LIMIT="$LIMIT_SAVE"
wait_for3 "round 13 (s3 after the hub restore)"
[ "$(count "$TH" "$URL_HUB")" = "$TOTAL" ] || fail "records were lost by the restore: $(count "$TH" "$URL_HUB") != $TOTAL"
log "epoch $EPOCH0 -> $EPOCH1, all $TOTAL records present on the hub again"


log "OK"
