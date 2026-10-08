#!/usr/bin/env bash
# Scanner e2e (docs/modules/scanner.md): a pty pair is the serial scanner, the
# web wedge goes through POST /api/scan, an SSE client listens on @scan.
# Everything runs on a random loopback port and is killed by PID file at exit.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
EMAIL=admin@example.com
PASS=adminpass1234

TMP="$(mktemp -d "${TMPDIR:-/tmp}/toki-e2e-scanner.XXXXXX")"
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
log() { echo "[scanner] $*" >&2; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }
fail() {
  log "FAIL: $*"
  for f in "$TMP"/*.log; do echo "--- $f" >&2; tail -25 "$f" >&2; done
  exit 1
}

cd "$ROOT"
log "building"
go build -o "$TOKI" ./examples/base

export TOKI_SCANNER=on
DATA="$TMP/pb_data"
"$TOKI" superuser upsert "$EMAIL" "$PASS" --dir "$DATA" >/dev/null

# the serial scanner: a pty
mkfifo "$TMP/scan.fifo"
python3 "$HERE/ptyscanner.py" "$TMP/slave" "$TMP/scan.fifo" >"$TMP/pty.log" 2>&1 &
echo $! >"$TMP/pty.pid"
for _ in $(seq 1 50); do [ -s "$TMP/slave" ] && break; sleep 0.1; done
SLAVE="$(cat "$TMP/slave")"
[ -n "$SLAVE" ] || fail "no pty"
log "pty scanner on $SLAVE"
send() { printf '%s\n' "$1" >"$TMP/scan.fifo"; }

PORT=$((20000 + RANDOM % 20000))
URL="http://127.0.0.1:$PORT"
"$TOKI" serve --dir "$DATA" --http "127.0.0.1:$PORT" >"$TMP/server.log" 2>&1 &
echo $! >"$TMP/server.pid"
for _ in $(seq 1 300); do curl -fs "$URL/api/health" >/dev/null 2>&1 && break; sleep 0.1; done
curl -fs "$URL/api/health" >/dev/null || fail "server did not start"

TOK="$(curl -fsS "$URL/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["token"]')"
api() { # method path [body]
  curl -fsS -X "$1" "$URL$2" -H "Authorization: $TOK" -H 'Content-Type: application/json' ${3:+-d "$3"}
}

api POST /api/collections/_scanners/records \
  "{\"name\":\"belt\",\"kind\":\"serial\",\"device\":\"$SLAVE\",\"baud\":9600,\"enabled\":true,\"charset\":\"^[A-Z0-9-]+\$\",\"min_len\":4}" >/dev/null \
  || fail "create serial scanner"
api POST /api/collections/_scanners/records '{"name":"door","kind":"web","enabled":true,"min_len":4}' >/dev/null || fail "create web scanner"
# a bad config is refused
if curl -fs -X POST "$URL/api/collections/_scanners/records" -H "Authorization: $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"bad","kind":"serial","device":"/etc/passwd","enabled":true}' >/dev/null 2>&1; then fail "a device outside /dev must be refused"; fi

# SSE client on @scan
curl -sN "$URL/api/realtime" >"$TMP/sse.log" 2>&1 &
echo $! >"$TMP/sse.pid"
for _ in $(seq 1 50); do grep -q clientId "$TMP/sse.log" && break; sleep 0.1; done
CID="$(grep -o '"clientId":"[^"]*"' "$TMP/sse.log" | head -1 | cut -d'"' -f4)"
[ -n "$CID" ] || fail "no realtime client id"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL/api/realtime" -H 'Content-Type: application/json' \
  -d "{\"clientId\":\"$CID\",\"subscriptions\":[\"@scan\"]}")"
[ "$code" = 403 ] || fail "a guest must not subscribe to @scan (got $code)"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL/api/realtime" -H "Authorization: $TOK" -H 'Content-Type: application/json' \
  -d "{\"clientId\":\"$CID\",\"subscriptions\":[\"@scan\"]}")"
[ "$code" = 204 ] || fail "authenticated subscribe got $code"

# wait for the reader
state=""
for _ in $(seq 1 100); do
  state="$(api GET /api/health | jget 'd["data"]["scanner"][0]["state"] if d.get("data",{}).get("scanner") else ""' 2>/dev/null || true)"
  [ "$state" = connected ] && break
  sleep 0.1
done
[ "$state" = connected ] || fail "serial reader state is '$state'"
log "serial reader connected"

# serial scans: a duplicate inside the window, a filtered one, a short one, a second code
send "ABC-123"; send "ABC-123"; send "lower-case"; send "AB"; send "DEF-456"
for _ in $(seq 1 100); do
  n="$(api GET "/api/scan/events?scanner=belt" | jget 'len(d["items"])')"
  [ "$n" = 2 ] && break
  sleep 0.1
done
[ "$n" = 2 ] || fail "expected 2 serial events, got $n"
api GET "/api/scan/events?scanner=belt" | jget 'd["items"][0]["code"]+" "+str(d["items"][0]["dup_count"])+" "+d["items"][1]["code"]' | grep -qx 'ABC-123 1 DEF-456' \
  || fail "unexpected serial events: $(api GET '/api/scan/events?scanner=belt')"
rej="$(api GET /api/health | jget 'd["data"]["scanner"][0]["rejected"]')"
[ "$rej" = 2 ] || fail "expected 2 rejected scans, got $rej"
log "serial dedupe and filters ok"

# web wedge: auth required, duplicate inside the window, idempotent retry
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$URL/api/scan" -H 'Content-Type: application/json' -d '{"code":"WEB0001"}')"
[ "$code" = 401 ] || fail "guest POST /api/scan got $code"
r1="$(api POST /api/scan '{"scanner":"door","code":"WEB0001","client_seq":"s-1"}')"
r2="$(api POST /api/scan '{"scanner":"door","code":"WEB0001","client_seq":"s-1"}')"
r3="$(api POST /api/scan '{"scanner":"door","code":"WEB0001"}')"
[ "$(echo "$r1" | jget 'd["duplicate"]')" = False ] || fail "first web scan: $r1"
[ "$(echo "$r2" | jget 'd["duplicate"]')" = True ] || fail "client_seq retry: $r2"
[ "$(echo "$r3" | jget 'd["duplicate"]')" = True ] || fail "dedupe window: $r3"
[ "$(api GET "/api/scan/events?scanner=door" | jget 'len(d["items"])')" = 1 ] || fail "web events"
log "web wedge ok"

# CLI
"$TOKI" scan list --dir "$DATA" | grep -q '^belt' || fail "toki scan list"
"$TOKI" scan devices --dir "$DATA" >/dev/null || fail "toki scan devices"
"$TOKI" scan simulate SIM0001 --scanner door --url "$URL" --token "$TOK" >/dev/null || fail "toki scan simulate --url"
curl -fs "$URL/scan/wedge.js" | grep -q TokiScan || fail "wedge.js"

# the SSE client got the published scans (serial, web, simulate) and nothing else
for _ in $(seq 1 50); do grep -q SIM0001 "$TMP/sse.log" && break; sleep 0.1; done
for c in ABC-123 DEF-456 WEB0001 SIM0001; do grep -q "$c" "$TMP/sse.log" || fail "@scan did not deliver $c"; done
[ "$(grep -c '"code":"ABC-123"' "$TMP/sse.log")" = 1 ] || fail "a duplicate must not be published"
grep -q 'lower-case' "$TMP/sse.log" && fail "a filtered scan was published"

send "@exit"
log "OK"
