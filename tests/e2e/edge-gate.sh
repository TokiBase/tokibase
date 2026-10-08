#!/usr/bin/env bash
# Edge gate e2e (docs/EDGE_GATE.md): the hardware modules end to end on an edge spoke.
#
#   hub   = solo build (default tags), TOKI_SYNC_ROLE=hub, devicecert on
#   spoke = built from the `edge` tags of profiles.txt, TOKI_SYNC_ROLE=spoke,
#           printer + scanner + kiosk + devicecert on, TCP stub printer, pty scanner
#
# Steps: enroll; kiosk pairs and gets a session; a scan arrives through the pty and shows up on
# /api/scan/events for the kiosk session; the kiosk prints a ticket (QR + cut reach the stub); a
# gate controller calls /api/scan on the TLS port with its client certificate and no token; the
# hub revokes that certificate and the call fails after the deny list reaches the spoke; the
# spoke goes offline (hub killed) and issues 5 reserved ticket numbers, then converges
# (`toki sync verify`); finally the kiosk is locked and its token is dead.
# The sync parking scenario lives in tests/e2e/parking.sh and is not repeated here.
#
# Linux only (pty, loopback ports). Env: TOKI_BIN_HUB / TOKI_BIN_EDGE reuse prebuilt binaries.
# Every port is probed free first; everything is killed by PID file at exit.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
EMAIL=admin@example.com
PASS=adminpass1234
LIMIT="${TOKI_E2E_TIMEOUT:-90}"
T_START=$SECONDS

TMP="$(mktemp -d "${TMPDIR:-/tmp}/toki-e2e-edgegate.XXXXXX")"
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
log() { echo "[edge-gate +$((SECONDS - T_START))s] $*" >&2; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }
fail() {
  log "FAIL: $*"
  for f in "$TMP"/*.log; do echo "--- $f" >&2; tail -30 "$f" >&2; done
  echo "--- spoke health" >&2; curl -s "$URL_S1/api/health" -H "Authorization: ${TS:-}" 2>&1 | cut -c1-1500 >&2 || true; echo >&2
  echo "--- scan events (kiosk token)" >&2; curl -s "$URL_S1/api/scan/events?scanner=belt" -H "Authorization: ${TOK:-}" 2>&1 | cut -c1-600 >&2 || true; echo >&2
  echo "--- hub conflicts" >&2; hubtoki sync conflicts 2>&1 | tail -15 >&2 || true
  echo "--- spoke conflicts" >&2; spoketoki sync conflicts 2>&1 | tail -15 >&2 || true
  echo "--- spoke sync status" >&2; spoketoki sync status 2>&1 | tail -15 >&2 || true
  exit 1
}
code_of() { curl -s -o /dev/null -w '%{http_code}' "$@" || true; }
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'; }
wait_for() { # label expression
  local t0=$SECONDS
  while ! eval "$2"; do
    [ $((SECONDS - t0)) -lt "$LIMIT" ] || fail "$1: timed out waiting for: $2"
    sleep 0.5
  done
}
stop_node() { # name: SIGTERM by PID file
  local p; p="$(cat "$TMP/$1.pid" 2>/dev/null || true)"
  [ -n "$p" ] || return 0
  kill "$p" 2>/dev/null || true
  wait "$p" 2>/dev/null || true
  rm -f "$TMP/$1.pid"
}

cd "$ROOT"
[ "$(uname)" = Linux ] || { echo "edge-gate.sh needs Linux (pty scanner)"; exit 0; }
HUB_BIN="${TOKI_BIN_HUB:-$TMP/toki-hub}"
EDGE_BIN="${TOKI_BIN_EDGE:-$TMP/toki-edge}"
if [ -z "${TOKI_BIN_HUB:-}" ]; then
  log "building hub (solo tags)"
  CGO_ENABLED=0 go build -trimpath -o "$HUB_BIN" ./examples/base
fi
if [ -z "${TOKI_BIN_EDGE:-}" ]; then
  EDGE_TAGS="$(awk '$1=="edge" {for (i=3;i<=NF;i++) printf "%s%s", (i>3?",":""), $i}' profiles.txt)"
  [ -n "$EDGE_TAGS" ] || fail "no edge tags in profiles.txt"
  log "building edge spoke (tags: $EDGE_TAGS)"
  CGO_ENABLED=0 go build -trimpath -tags "$EDGE_TAGS" -ldflags "-s -w" -o "$EDGE_BIN" ./examples/base
fi

PORT_HUB="$(free_port)"; PORT_S1="$(free_port)"; TLS_HUB="$(free_port)"; TLS_S1="$(free_port)"
URL_HUB="http://127.0.0.1:$PORT_HUB"
URL_S1="http://127.0.0.1:$PORT_S1"
HUB="$TMP/hub"; S1="$TMP/s1"

# environment of the two nodes (what the systemd unit of docs/EDGE_GATE.md sets). Arrays, not
# functions: a backgrounded function is a subshell and $! would not be the toki process.
HUB_ENV=(env TOKI_SYNC_ROLE=hub TOKI_SYNC_INSECURE=1 TOKI_SYNC_INTERVAL=1s TOKI_DEVICECERT=on
  TOKI_DEVICECERT_LISTEN="127.0.0.1:$TLS_HUB" TOKI_DEVICECERT_MTLS=optional)
SPOKE_ENV=(env TOKI_SYNC_ROLE=spoke TOKI_SYNC_INSECURE=1 TOKI_SYNC_INTERVAL=1s
  TOKI_PRINTER=on TOKI_SCANNER=on TOKI_KIOSK=on TOKI_DEVICECERT=on
  TOKI_DEVICECERT_LISTEN="127.0.0.1:$TLS_S1" TOKI_DEVICECERT_MTLS=optional
  TOKI_PRINT_ALLOW_COLLECTIONS=gate_devices TOKI_SCAN_READ_AUTH=gate_devices
  TOKI_SCAN_POST_COLLECTIONS=gate_devices GOMEMLIMIT=200MiB)
hubtoki() { "${HUB_ENV[@]}" "$HUB_BIN" "$@" --dev=false --dir "$HUB"; }
spoketoki() { "${SPOKE_ENV[@]}" "$EDGE_BIN" "$@" --dev=false --dir "$S1"; }
start_hub() {
  "${HUB_ENV[@]}" "$HUB_BIN" serve --dev=false --automigrate=false --dir "$HUB" --http "127.0.0.1:$PORT_HUB" >>"$TMP/hub.log" 2>&1 &
  echo $! >"$TMP/hub.pid"
  wait_for "hub health" "curl -fs $URL_HUB/api/health >/dev/null 2>&1"
}
start_spoke() { # [role]: "off" for the setup phase before enrollment
  "${SPOKE_ENV[@]}" TOKI_SYNC_ROLE="${1:-spoke}" "$EDGE_BIN" serve --dev=false --automigrate=false --publicDir "$TMP/app" --dir "$S1" --http "127.0.0.1:$PORT_S1" >>"$TMP/s1.log" 2>&1 &
  echo $! >"$TMP/s1.pid"
  wait_for "spoke health" "curl -fs $URL_S1/api/health >/dev/null 2>&1"
}
su_token() { curl -fsS "$1/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["token"]'; }
api() { # token method url path [body]
  local out code
  out="$(curl -sS -w '\n%{http_code}' -X "$2" "$3$4" -H "Authorization: $1" -H 'Content-Type: application/json' ${5:+-d "$5"})" || return 1
  code="${out##*$'\n'}"
  if [ "${code:0:1}" != 2 ]; then echo "[edge-gate] HTTP $code on $2 $3$4: ${out%$'\n'*}" >&2; return 1; fi
  printf '%s' "${out%$'\n'*}"
}

# ---- 0. the pieces that are hardware on a real gate: pty scanner, TCP stub printer, app dir ----
mkfifo "$TMP/scan.fifo"
python3 "$HERE/ptyscanner.py" "$TMP/slave" "$TMP/scan.fifo" >"$TMP/pty.log" 2>&1 &
echo $! >"$TMP/pty.pid"
python3 "$HERE/edgegate/stubprinter.py" "$TMP/stub.port" "$TMP/captured.bin" >"$TMP/stub.log" 2>&1 &
echo $! >"$TMP/stub.pid"
wait_for "pty scanner and stub printer" '[ -s "$TMP/slave" ] && [ -s "$TMP/stub.port" ]'
SLAVE="$(cat "$TMP/slave")"; SPORT="$(cat "$TMP/stub.port")"
send_scan() { printf '%s\n' "$1" >"$TMP/scan.fifo"; }
mkdir -p "$TMP/app"
printf '<!doctype html><title>gate</title><script src="/kiosk/kiosk.js"></script><script src="/scan/wedge.js"></script>\n' >"$TMP/app/index.html"
log "pty scanner $SLAVE, stub printer :$SPORT"

# ---- 1. hub: collection, policies, sequence ----
hubtoki superuser upsert "$EMAIL" "$PASS" >/dev/null
start_hub
TH="$(su_token "$URL_HUB")"
SU_ID="$(curl -fsS "$URL_HUB/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["record"]["id"]')"
TICKETS='{"id":"pbc_edgetickets","name":"tickets","type":"base","listRule":"@request.auth.id != \"\"","viewRule":"@request.auth.id != \"\"","createRule":"@request.auth.id != \"\"","updateRule":"@request.auth.id != \"\"","deleteRule":null,"fields":[{"name":"no","type":"text","required":true},{"name":"plate","type":"text"},{"name":"created","type":"autodate","onCreate":true},{"name":"updated","type":"autodate","onCreate":true,"onUpdate":true}],"indexes":["CREATE UNIQUE INDEX idx_tickets_no ON tickets (no)"]}'
api "$TH" POST "$URL_HUB" /api/collections "$TICKETS" >/dev/null || fail "create tickets on the hub"
hubtoki sync reserve create-seq tickets --block 40 --max-open 2 >/dev/null || fail "create-seq"
api "$TH" POST "$URL_HUB" /api/collections/_sync_policies/records \
  '{"collection":"tickets","direction":"both","enabled":true,"field_types":{"no":"reserve:tickets"}}' >/dev/null || fail "tickets policy"
# the deny list: pull-only policy on _device_certs BEFORE the spoke handshakes
hubtoki sync policies set _device_certs --direction pull >/dev/null || fail "policy on _device_certs"
hubtoki devicecert ca >"$TMP/root.pem" 2>/dev/null || fail "hub CA"
grep -q 'BEGIN CERTIFICATE' "$TMP/root.pem" || fail "no root PEM"
log "hub up on :$PORT_HUB (TLS :$TLS_HUB)"

# ---- 2. spoke local config: what the app migrations and the operator do on every box ----
# (a spoke refuses new collections once enrolled, so gate_devices is created first; the printer,
#  scanner and kiosk rows are system collections that never sync)
"${SPOKE_ENV[@]}" TOKI_SYNC_ROLE=off "$EDGE_BIN" superuser upsert "$EMAIL" "$PASS" --dev=false --dir "$S1" >/dev/null
start_spoke off
TS="$(su_token "$URL_S1")"
api "$TS" POST "$URL_S1" /api/collections '{"name":"gate_devices","type":"auth","viewRule":"@request.auth.id = id"}' >/dev/null || fail "create gate_devices"
ACTOR="$(api "$TS" POST "$URL_S1" /api/collections/gate_devices/records \
  '{"email":"gate1@example.com","password":"gatepass1234","passwordConfirm":"gatepass1234"}' | jget 'd["id"]')"
api "$TS" POST "$URL_S1" /api/collections/_printers/records \
  "{\"name\":\"counter\",\"transport\":\"tcp\",\"address\":\"127.0.0.1:$SPORT\",\"enabled\":true,\"default\":true,\"qr_native\":true,\"cut\":true,\"cols\":32,\"timeout_ms\":2000}" >/dev/null || fail "create printer"
api "$TS" POST "$URL_S1" /api/collections/_print_templates/records \
  '{"name":"ticket","body":"@center\nPARKIR\n@left\nNo: {{.no}}\nPlat: {{.plate}}\n@qr {{.qr}}\n@feed 3\n@cut"}' >/dev/null || fail "create template"
api "$TS" POST "$URL_S1" /api/collections/_scanners/records \
  "{\"name\":\"belt\",\"kind\":\"serial\",\"device\":\"$SLAVE\",\"baud\":9600,\"enabled\":true,\"charset\":\"^[A-Z0-9-]+\$\",\"min_len\":4}" >/dev/null || fail "create scanner"
api "$TS" POST "$URL_S1" /api/collections/_scanners/records '{"name":"door","kind":"web","enabled":true,"min_len":4}' >/dev/null || fail "create web scanner"
stop_node s1
log "spoke configured offline (gate_devices/$ACTOR, printer, template, 2 scanners)"

# ---- 3. enroll and start the edge spoke ----
CODE="$(hubtoki sync enroll --name gate-1 --profile edge --actor "_superusers/$SU_ID" --allow-superuser-actor | awk '/^code:/ {print $2}')"
[ -n "$CODE" ] || fail "no enrollment code"
spoketoki sync join "$URL_HUB" "$CODE" >/dev/null || fail "join"
start_spoke
TS="$(su_token "$URL_S1")"
wait_for "tickets collection on the spoke" '[ "$(code_of "$URL_S1/api/collections/tickets" -H "Authorization: $TS")" = 200 ]'
wait_for "scanner reader connected" 'curl -fsS "$URL_S1/api/health" -H "Authorization: $TS" | grep -q "\"state\":\"connected\""'
curl -fsS "$URL_S1/api/health" -H "Authorization: $TS" >"$TMP/health.json"
for blk in printer scanner devicecert sync; do jget "'$blk' in d['data']" <"$TMP/health.json" | grep -q True || fail "health has no $blk block"; done
log "enrolled and running as an edge spoke"

# ---- 4. kiosk: provision, pair, session ----
PAIR_URL="$(spoketoki kiosk provision --name gate-1 --actor "gate_devices/$ACTOR" --pin 1234 --lock-after 60 --url "$URL_S1" | tail -n 1)" || fail "kiosk provision"
case "$PAIR_URL" in "$URL_S1/kiosk/pair#"*) ;; *) fail "unexpected pairing URL: $PAIR_URL" ;; esac
JAR="$TMP/jar"
curl -fsS -c "$JAR" -X POST "$URL_S1/api/kiosk/pair" -H 'Content-Type: application/json' -d "{\"code\":\"${PAIR_URL#*#}\"}" >/dev/null || fail "pair"
SESS="$(curl -fsS -b "$JAR" -X POST "$URL_S1/api/kiosk/session")" || fail "session"
TOK="$(echo "$SESS" | jget 'd["token"]')"
[ "$(echo "$SESS" | jget 'd["record"]["id"]')" = "$ACTOR" ] || fail "session actor: $SESS"
curl -fs "$URL_S1/" | grep -q kiosk.js || fail "the app page does not load kiosk.js (--publicDir)"
ST="$(curl -fsS -b "$JAR" "$URL_S1/api/kiosk/status")" || fail "kiosk status"
[ "$(echo "$ST" | jget 'd["edge"]')" = True ] || fail "kiosk status: $ST"
log "(1) kiosk paired, session for gate_devices/$ACTOR"

# ---- 5. a scan arrives through the pty and is visible to the kiosk session ----
send_scan "TKT-0001"; send_scan "lower-case"; send_scan "TKT-0002"
wait_for "scan events" '[ "$(curl -fsS "$URL_S1/api/scan/events?scanner=belt" -H "Authorization: $TOK" | jget "len(d[\"items\"])")" = 2 ]'
curl -fsS "$URL_S1/api/scan/events?scanner=belt" -H "Authorization: $TOK" | jget '",".join(i["code"] for i in d["items"])' | grep -qx 'TKT-0001,TKT-0002' || fail "scan events do not match"
[ "$(code_of "$URL_S1/api/scan/events?scanner=belt")" != 200 ] || fail "a guest read the scan events"
log "(2) pty scans visible on /api/scan/events for the kiosk session (filtered code dropped)"

# ---- 6. a ticket gets a reserved number and the kiosk prints it ----
# Tickets are written with the node's operator token (superuser of the spoke = captured as the
# `node` actor, replayed on the hub as the service actor). A write made as the kiosk actor itself
# (gate_devices/...) is captured as `rec:...` and the hub PARKS it unless the actor holds a sync
# grant (docs/EDGE_GATE.md, "Who writes"); that parked path is the parking scenario, not tested here.
NO=""
wait_for "first reserved number on the spoke" 'NO="$(curl -fsS -X POST "$URL_S1/api/collections/tickets/records" -H "Authorization: $TS" -H "Content-Type: application/json" -d "{\"plate\":\"B 1234 XY\"}" 2>/dev/null | jget "d[\"no\"]" 2>/dev/null)" && [ -n "$NO" ]'
JOB="$(curl -fsS -X POST "$URL_S1/api/print" -H "Authorization: $TOK" -H 'Content-Type: application/json' \
  -d "{\"template\":\"ticket\",\"data\":{\"no\":\"$NO\",\"plate\":\"B 1234 XY\",\"qr\":\"GATE-$NO\"},\"idempotency_key\":\"edge-gate-1\"}" | jget 'd["id"]')" || fail "POST /api/print"
wait_for "print job done" '[ "$(curl -fsS "$URL_S1/api/print/$JOB" -H "Authorization: $TOK" | jget "d[\"state\"]")" = done ]'
python3 - "$TMP/captured.bin" "GATE-$NO" <<'PY' || fail "captured printer bytes"
import sys
b = open(sys.argv[1], "rb").read()
qr = sys.argv[2].encode()
assert qr in b, "QR payload missing"
assert b"\x1d(k" in b, "native QR command missing"
assert b"\x1dV\x42\x00" in b, "cut missing"
assert b"B 1234 XY" in b, "plate missing"
assert b.count(qr) == 1, "printed %d times" % b.count(qr)
PY
log "(3) ticket $NO printed: QR + cut reached the stub printer"

# ---- 7. a gate controller with a client certificate, no token ----
OUT="$TMP/peer"
hubtoki devicecert issue --name gate-ctrl --days 90 --scope /api/scan --out "$OUT" >"$TMP/issue.txt" || fail "devicecert issue"
SERIAL="$(awk '/^serial:/ {print $2}' "$TMP/issue.txt")"
[ -n "$SERIAL" ] || fail "no serial"
gate_call() { # prints the http code (000 when the TLS handshake fails)
  curl -s -o "$TMP/gate.body" -w '%{http_code}' --cacert "$TMP/root.pem" --cert "$OUT/gate-ctrl.crt.pem" --key "$OUT/gate-ctrl.key.pem" \
    -H 'Content-Type: application/json' -d '{"scanner":"door","code":"GATE-CTRL-IN"}' "https://127.0.0.1:$TLS_S1/api/scan" 2>/dev/null || true
}
wait_for "spoke TLS listener with a leaf" "curl -fs --cacert $TMP/root.pem https://127.0.0.1:$TLS_S1/api/health >/dev/null 2>&1"
wait_for "gate controller admitted" '[ "$(gate_call)" = 200 ]'
[ "$(code_of --cacert "$TMP/root.pem" -X POST -H 'Content-Type: application/json' -d '{"scanner":"door","code":"GATE-CTRL-IN"}' "https://127.0.0.1:$TLS_S1/api/scan")" = 401 ] || fail "no client certificate must get 401"
[ "$(code_of --cacert "$TMP/root.pem" --cert "$OUT/gate-ctrl.crt.pem" --key "$OUT/gate-ctrl.key.pem" "https://127.0.0.1:$TLS_S1/api/collections/_superusers/records")" != 200 ] || fail "the certificate must not reach /api/collections"
log "(4) gate controller: /api/scan on the TLS port with its client certificate, no token -> 200"

# ---- 8. the hub revokes it ----
hubtoki devicecert revoke gate-ctrl >/dev/null || fail "revoke"
wait_for "revoked certificate refused" '[ "$(gate_call)" != 200 ]'
log "(5) revoked client certificate refused after the deny list reached the spoke"

# ---- 9. offline: reserved numbers keep the gate issuing tickets ----
stop_node hub
log "hub stopped"
OFF_NOS=""
for i in 1 2 3 4 5; do
  n="$(curl -fsS -X POST "$URL_S1/api/collections/tickets/records" -H "Authorization: $TS" -H 'Content-Type: application/json' -d "{\"plate\":\"OFF $i\"}" | jget 'd["no"]')" \
    || fail "ticket $i could not be issued offline"
  OFF_NOS="$OFF_NOS $n"
done
[ "$(printf '%s\n' $OFF_NOS | sort -u | wc -l)" = 5 ] || fail "duplicate offline ticket numbers:$OFF_NOS"
PENDING="$(spoketoki sync status --json 2>/dev/null | grep '^{' | tail -1 | jget 'd["pending"]')"
[ "$PENDING" -ge 5 ] || fail "expected >= 5 pending changes offline, got $PENDING"
KP="$(curl -fsS -b "$JAR" "$URL_S1/api/kiosk/status" | jget 'd.get("pending_changes", 0)')"
[ "$KP" -ge 5 ] || fail "kiosk status should show the pending changes, got $KP"
log "(6) offline: 5 tickets with reserved numbers ($(echo $OFF_NOS)), $PENDING changes pending"
start_hub
digest() { # role -> "records digest pending" of tickets
  local out
  if [ "$1" = hub ]; then out="$(hubtoki sync verify --json 2>/dev/null | grep '^{' | tail -1 || true)"
  else out="$(spoketoki sync verify --json 2>/dev/null | grep '^{' | tail -1 || true)"; fi
  [ -n "$out" ] || { echo "- - -"; return; }
  echo "$out" | jget '(lambda c: (str(c["records"])+" "+c["digest"]+" "+str(d["pending"])) if c else "- - -")(next((x for x in d["collections"] if x["name"]=="tickets"), None))'
}
converged() { local h s; h="$(digest hub)"; s="$(digest spoke)"; CONV="hub[$h] spoke[$s]"
  [ "${h%% *}" != "-" ] && [ "${h% *}" = "${s% *}" ] && [ "${s##* }" = 0 ]; }
wait_for "convergence after the hub came back" 'converged'
TH="$(su_token "$URL_HUB")"
api "$TH" GET "$URL_HUB" "/api/collections/tickets/records?perPage=200&fields=no" >"$TMP/tk-hub.json"
jget 'len(set(i["no"] for i in d["items"])) == len(d["items"]) >= 6' <"$TMP/tk-hub.json" | grep -q True || fail "ticket numbers on the hub: $(cat "$TMP/tk-hub.json") conv=$CONV hub-verify=$(hubtoki sync verify --json 2>&1 | grep '^{' | tail -1)"
spoketoki sync verify --against-hub --json 2>/dev/null | grep '^{' | tail -1 | jget 'd["against_hub"]["checked"] is True and d["against_hub"]["mismatch"] == []' | grep -q True || fail "verify --against-hub"
log "(6) converged after the hub returned ($CONV)"

# ---- 10. lock: the session token dies ----
[ "$(code_of "$URL_S1/api/print/printers" -H "Authorization: $TOK")" = 200 ] || fail "token should work before the lock"
curl -fsS -b "$JAR" -X POST "$URL_S1/api/kiosk/lock" -H "Authorization: $TOK" >/dev/null || fail "lock"
[ "$(code_of "$URL_S1/api/print/printers" -H "Authorization: $TOK")" != 200 ] || fail "the token still works after the lock"
[ "$(code_of -X POST "$URL_S1/api/collections/tickets/records" -H "Authorization: $TOK" -H 'Content-Type: application/json' -d '{"plate":"LOCKED"}')" != 200 ] || fail "a locked kiosk created a ticket"
[ "$(code_of -b "$JAR" -X POST "$URL_S1/api/kiosk/session")" = 423 ] || fail "session while locked"
curl -fsS -b "$JAR" -X POST "$URL_S1/api/kiosk/unlock" -H 'Content-Type: application/json' -d '{"pin":"1234"}' >/dev/null || fail "unlock"
log "(7) kiosk locked: token rejected, no new session until the PIN"

# ---- 11. the operator commands of the walkthrough ----
spoketoki print status counter >/dev/null 2>&1 || log "note: toki print status counter failed (stub answers only DLE EOT)"
spoketoki devicecert status --json | jget 'd["leaf"]["dns"][0]' | grep -q 'edge.toki.local' || fail "devicecert status"
spoketoki kiosk list | grep -q '^gate-1' || fail "kiosk list"
spoketoki sync status --json 2>/dev/null | grep '^{' | tail -1 | jget 'd["role"]' | grep -qx spoke || fail "sync status role"

log "OK (total $((SECONDS - T_START))s)"
