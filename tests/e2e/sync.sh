#!/usr/bin/env bash
# Sync e2e (docs/SYNC_DESIGN.md §9, PR3): one hub and two spokes, each with its own pb_data,
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
  TOKI_SYNC_ROLE="$2" TOKI_SYNC_INSECURE=1 TOKI_SYNC_INTERVAL=1s TOKI_SYNC_PAGE=25 \
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

COLL='{"id":"pbc_e2eitems","name":"e2eitems","type":"base","listRule":"","viewRule":"","createRule":"","updateRule":"","deleteRule":"","fields":[{"name":"title","type":"text"},{"name":"qty","type":"number"},{"name":"note","type":"text"},{"name":"created","type":"autodate","onCreate":true},{"name":"updated","type":"autodate","onCreate":true,"onUpdate":true}]}'
api "$TH" POST "$URL_HUB" /api/collections "$COLL" >/dev/null
api "$TH" POST "$URL_HUB" /api/collections/_sync_policies/records \
  '{"collection":"e2eitems","direction":"both","enabled":true,"field_types":{"qty":"counter"}}' >/dev/null
log "hub up on $PORT_HUB with collection e2eitems and policy direction=both"

# ---- spokes: enroll, start, create the same collection (schema bundles are PR8) ----
enroll_spoke() { # name dir port url
  local code
  code="$(toki hub "$HUB" sync enroll --name "$1" --profile edge | awk '/^code:/ {print $2}')"
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
patch "$T1" "$URL_S1" '{"title":"edited on s1","qty+":3}' &
patch "$T2" "$URL_S2" '{"title":"edited on s2","qty+":4}' &
wait
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

log "OK"
