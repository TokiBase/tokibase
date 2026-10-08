#!/usr/bin/env bash
# Printer e2e: boots the binary with TOKI_PRINTER=on, points a printer at a TCP
# stub (answers DLE EOT, records every byte), prints a ticket through
# POST /api/print and asserts the captured bytes hold the QR payload and the cut.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
EMAIL=admin@example.com
PASS=adminpass1234
QR=QR-E2E-123456

TMP="$(mktemp -d "${TMPDIR:-/tmp}/toki-printer-e2e.XXXXXX")"
cleanup() {
  for f in "$TMP/toki.pid" "$TMP/stub.pid"; do
    if [ -f "$f" ]; then
      p="$(cat "$f")"
      kill "$p" 2>/dev/null || true
      wait "$p" 2>/dev/null || true
    fi
  done
  rm -rf "$TMP"
}
trap cleanup EXIT
log() { echo "[printer] $*" >&2; }
fail() { log "FAIL: $*"; tail -40 "$TMP/toki.log" >&2 || true; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }

cd "$ROOT"
log "building"
go build -o "$TMP/toki" ./examples/base

# --- TCP stub printer -------------------------------------------------------
cat >"$TMP/stub.py" <<'PY'
import socket, sys, threading
portfile, outfile = sys.argv[1], sys.argv[2]
Q = bytes([16, 4, 1, 16, 4, 2, 16, 4, 3, 16, 4, 4])
srv = socket.socket()
srv.bind(("127.0.0.1", 0))
srv.listen(8)
lock = threading.Lock()
out = open(outfile, "ab", buffering=0)
open(portfile, "w").write(str(srv.getsockname()[1]))

def put(b):
    with lock:
        out.write(b)

def serve(c):
    buf = b""
    while True:
        d = c.recv(4096)
        if not d:
            break
        buf += d
        while True:
            i = buf.find(Q)
            if i < 0:
                break
            put(buf[:i])
            buf = buf[i + len(Q):]
            c.sendall(bytes([0x12] * 4))
        if len(buf) > len(Q) - 1:
            put(buf[: -(len(Q) - 1)])
            buf = buf[-(len(Q) - 1):]
    put(buf)
    c.close()

while True:
    c, _ = srv.accept()
    threading.Thread(target=serve, args=(c,), daemon=True).start()
PY
python3 "$TMP/stub.py" "$TMP/stub.port" "$TMP/captured.bin" &
echo $! >"$TMP/stub.pid"
for _ in $(seq 1 50); do [ -s "$TMP/stub.port" ] && break; sleep 0.1; done
[ -s "$TMP/stub.port" ] || fail "stub printer did not start"
SPORT="$(cat "$TMP/stub.port")"

# --- server -----------------------------------------------------------------
"$TMP/toki" superuser upsert "$EMAIL" "$PASS" --dir "$TMP/pb_data" >/dev/null
PORT=$((20000 + RANDOM % 20000))
BASE="http://127.0.0.1:$PORT"
TOKI_PRINTER=on "$TMP/toki" serve --dir "$TMP/pb_data" --http "127.0.0.1:$PORT" >"$TMP/toki.log" 2>&1 &
echo $! >"$TMP/toki.pid"
for _ in $(seq 1 100); do curl -fs "$BASE/api/health" >/dev/null 2>&1 && break; sleep 0.2; done
curl -fs "$BASE/api/health" >/dev/null || fail "server did not start"

ST="$(curl -fs "$BASE/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget "d['token']")"
auth=(-H "Authorization: $ST" -H 'Content-Type: application/json')

curl -fs "${auth[@]}" "$BASE/api/collections/_printers/records" \
  -d "{\"name\":\"counter\",\"transport\":\"tcp\",\"address\":\"127.0.0.1:$SPORT\",\"enabled\":true,\"default\":true,\"qr_native\":true,\"cut\":true,\"cols\":32,\"timeout_ms\":2000}" >/dev/null \
  || fail "create printer"
curl -fs "${auth[@]}" "$BASE/api/collections/_print_templates/records" \
  -d '{"name":"ticket","body":"@center\nPARKIR\n@left\nPlat: {{.plate}}\n@qr {{.ticket_id}}\n@feed 3\n@cut"}' >/dev/null \
  || fail "create template"

# a regular user prints with the template (no superuser needed)
curl -fs "${auth[@]}" "$BASE/api/collections/users/records" \
  -d '{"email":"gate@example.com","password":"gatepass12345","passwordConfirm":"gatepass12345"}' >/dev/null \
  || fail "create user"
UT="$(curl -fs "$BASE/api/collections/users/auth-with-password" -H 'Content-Type: application/json' \
  -d '{"identity":"gate@example.com","password":"gatepass12345"}' | jget "d['token']")"

code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/print" -H 'Content-Type: application/json' -d '{"template":"ticket"}')"
[ "$code" = 401 ] || fail "guest print returned $code, want 401"

RES="$(curl -fs "$BASE/api/print" -H "Authorization: $UT" -H 'Content-Type: application/json' \
  -d "{\"template\":\"ticket\",\"data\":{\"plate\":\"B 1234 XY\",\"ticket_id\":\"$QR\"},\"idempotency_key\":\"e2e-1\"}")" \
  || fail "POST /api/print"
ID="$(echo "$RES" | jget "d['id']")"
log "queued $ID"
AGAIN="$(curl -fs "$BASE/api/print" -H "Authorization: $UT" -H 'Content-Type: application/json' \
  -d "{\"template\":\"ticket\",\"data\":{\"plate\":\"B 1234 XY\",\"ticket_id\":\"$QR\"},\"idempotency_key\":\"e2e-1\"}" | jget "d['id']")"
[ "$AGAIN" = "$ID" ] || fail "idempotency key created a second job ($AGAIN != $ID)"

state=""
for _ in $(seq 1 60); do
  state="$(curl -fs "$BASE/api/print/$ID" -H "Authorization: $UT" | jget "d['state']")"
  [ "$state" = done ] && break
  sleep 0.5
done
[ "$state" = done ] || fail "job state is '$state', want done"

python3 - "$TMP/captured.bin" "$QR" <<'PY' || fail "captured bytes"
import sys
b = open(sys.argv[1], "rb").read()
qr = sys.argv[2].encode()
assert qr in b, "QR payload missing from the captured bytes"
assert b"\x1dV\x42\x00" in b, "cut command missing"
assert b.count(qr) == 1, "ticket printed %d times" % b.count(qr)
assert b"B 1234 XY" in b, "plate missing"
PY

curl -fs "$BASE/api/health" -H "Authorization: $ST" | grep -q '"printer"' || fail "health has no printer block"
list="$(curl -fs "$BASE/api/print/printers" -H "Authorization: $UT")"
echo "$list" | grep -q counter || fail "printers list: $list"
echo "$list" | grep -q "127.0.0.1" && fail "address leaked to a regular user"

"$TMP/toki" print jobs --dir "$TMP/pb_data" | grep -q "$ID" || fail "toki print jobs"
log "OK"
