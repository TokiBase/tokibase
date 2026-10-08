#!/usr/bin/env bash
# Kiosk e2e (docs/modules/kiosk.md): provision a device with the CLI, pair with
# the one-time code, trade the cookie for an actor token, read status, lock and
# unlock with the PIN. curl only; random loopback port; killed by PID file.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
EMAIL=admin@example.com
PASS=adminpass1234

TMP="$(mktemp -d "${TMPDIR:-/tmp}/toki-e2e-kiosk.XXXXXX")"
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
log() { echo "[kiosk] $*" >&2; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }
fail() {
  log "FAIL: $*"
  for f in "$TMP"/*.log; do echo "--- $f" >&2; tail -25 "$f" >&2; done
  exit 1
}
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

cd "$ROOT"
log "building"
go build -o "$TOKI" ./examples/base

export TOKI_KIOSK=on
DATA="$TMP/pb_data"
"$TOKI" superuser upsert "$EMAIL" "$PASS" --dir "$DATA" >/dev/null

PORT=$((20000 + RANDOM % 20000))
URL="http://127.0.0.1:$PORT"
"$TOKI" serve --dir "$DATA" --http "127.0.0.1:$PORT" >"$TMP/server.log" 2>&1 &
echo $! >"$TMP/server.pid"
for _ in $(seq 1 300); do curl -fs "$URL/api/health" >/dev/null 2>&1 && break; sleep 0.1; done
curl -fs "$URL/api/health" >/dev/null || fail "server did not start"

SU="$(curl -fsS "$URL/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["token"]')"
api() { curl -fsS -X "$1" "$URL$2" -H "Authorization: $SU" -H 'Content-Type: application/json' ${3:+-d "$3"}; }

# the service actor: a low-privilege auth record that may only read itself
api POST /api/collections '{"name":"gate_devices","type":"auth","viewRule":"@request.auth.id = id"}' >/dev/null || fail "create gate_devices"
ACTOR="$(api POST /api/collections/gate_devices/records \
  '{"email":"gate1@example.com","password":"gatepass1234","passwordConfirm":"gatepass1234"}' | jget 'd["id"]')"
[ -n "$ACTOR" ] || fail "no actor"

# a superuser can never be the actor
if "$TOKI" kiosk provision --dir "$DATA" --name bad --actor "_superusers/x" >/dev/null 2>&1; then fail "a superuser actor must be refused"; fi

PAIR_URL="$("$TOKI" kiosk provision --dir "$DATA" --name gate-1 --actor "gate_devices/$ACTOR" --pin 1234 --lock-after 60 --url "$URL" | tail -n 1)" \
  || fail "provision"
case "$PAIR_URL" in "$URL/kiosk/pair#"*) ;; *) fail "unexpected pairing URL: $PAIR_URL" ;; esac
CODE="${PAIR_URL#*#}"
log "provisioned"

JAR="$TMP/jar"
# no cookie yet: no session
[ "$(code_of -X POST "$URL/api/kiosk/session")" = 401 ] || fail "session without a cookie"
[ "$(code_of -X POST "$URL/api/kiosk/pair" -H 'Content-Type: application/json' -d '{"code":"wrong"}')" = 403 ] || fail "wrong code"

curl -fsS -D "$TMP/pair.hdr" -c "$JAR" -X POST "$URL/api/kiosk/pair" -H 'Content-Type: application/json' \
  -d "{\"code\":\"$CODE\"}" >/dev/null || fail "pair"
grep -i '^set-cookie: toki_kiosk=' "$TMP/pair.hdr" | grep -qi 'HttpOnly' || fail "cookie must be HttpOnly"
grep -i '^set-cookie: toki_kiosk=' "$TMP/pair.hdr" | grep -qi 'SameSite=Strict' || fail "cookie must be SameSite=Strict"
grep -i '^set-cookie: toki_kiosk=' "$TMP/pair.hdr" | grep -qi 'Path=/api/kiosk' || fail "cookie path"
[ "$(code_of -X POST "$URL/api/kiosk/pair" -H 'Content-Type: application/json' -d "{\"code\":\"$CODE\"}")" = 403 ] || fail "the pairing code must be single use"
log "pairing ok"

S="$(curl -fsS -b "$JAR" -X POST "$URL/api/kiosk/session")" || fail "session"
TOK="$(echo "$S" | jget 'd["token"]')"
[ "$(echo "$S" | jget 'd["ttl_s"]')" = 43200 ] || fail "default TTL is 12 h: $S"
[ "$(echo "$S" | jget 'd["record"]["id"]')" = "$ACTOR" ] || fail "actor id: $S"
[ "$(code_of "$URL/api/collections/gate_devices/records/$ACTOR" -H "Authorization: $TOK")" = 200 ] || fail "actor token does not work"
[ "$(code_of "$URL/api/collections/gate_devices/records/$ACTOR")" != 200 ] || fail "guest read the actor"
log "session ok"

ST="$(curl -fsS -b "$JAR" "$URL/api/kiosk/status")" || fail "status"
[ "$(echo "$ST" | jget 'd["edge"]')" = True ] || fail "status: $ST"
echo "$ST" | jget '",".join(sorted(d))' | grep -q 'printers' || fail "status has no printers: $ST"
[ "$(code_of "$URL/api/kiosk/status")" = 401 ] || fail "status without cookie or token"

# lock: the token dies, no new session until the PIN
curl -fsS -b "$JAR" -X POST "$URL/api/kiosk/lock" -H "Authorization: $TOK" >/dev/null || fail "lock"
[ "$(code_of "$URL/api/collections/gate_devices/records/$ACTOR" -H "Authorization: $TOK")" != 200 ] || fail "token still works after lock"
[ "$(code_of -b "$JAR" -X POST "$URL/api/kiosk/session")" = 423 ] || fail "session while locked"
[ "$(code_of -b "$JAR" -X POST "$URL/api/kiosk/unlock" -H 'Content-Type: application/json' -d '{"pin":"0000"}')" = 401 ] || fail "wrong PIN"
U="$(curl -fsS -b "$JAR" -X POST "$URL/api/kiosk/unlock" -H 'Content-Type: application/json' -d '{"pin":"1234"}')" || fail "unlock"
TOK2="$(echo "$U" | jget 'd["token"]')"
[ "$(code_of "$URL/api/collections/gate_devices/records/$ACTOR" -H "Authorization: $TOK2")" = 200 ] || fail "token after unlock"
log "lock and unlock ok"

# brute force brake
for _ in 1 2 3 4 5; do code_of -b "$JAR" -X POST "$URL/api/kiosk/unlock" -H 'Content-Type: application/json' -d '{"pin":"9999"}' >/dev/null; done
[ "$(code_of -b "$JAR" -X POST "$URL/api/kiosk/unlock" -H 'Content-Type: application/json' -d '{"pin":"1234"}')" = 429 ] || fail "PIN lockout"

curl -fs "$URL/kiosk/kiosk.js" | grep -q pocketbase_auth || fail "kiosk.js"
curl -fs "$URL/kiosk/pair" | grep -q kiosk.js || fail "pair page"

# CLI: list, rotate drops the paired browser, revoke
"$TOKI" kiosk list --dir "$DATA" | grep -q '^gate-1' || fail "kiosk list"
NEW="$("$TOKI" kiosk rotate gate-1 --dir "$DATA" --url "$URL" | tail -n 1)" || fail "rotate"
[ "$(code_of -b "$JAR" -X POST "$URL/api/kiosk/session")" = 401 ] || fail "the old cookie must stop working after rotate"
[ "$(code_of -X POST "$URL/api/kiosk/pair" -H 'Content-Type: application/json' -d "{\"code\":\"${NEW#*#}\"}")" = 200 ] || fail "pair after rotate"
"$TOKI" kiosk revoke gate-1 --dir "$DATA" >/dev/null || fail "revoke"
"$TOKI" kiosk set-pin gate-1 --pin 4321 --dir "$DATA" || fail "set-pin"

log "OK"
