#!/usr/bin/env bash
# Devicecert e2e (docs/modules/devicecert.md): a hub with a CA and one spoke that
# fetches its edge TLS leaf through the sync session. Each serves HTTPS on a
# random loopback port; curl --cacert root.pem must succeed against both.
# Everything is killed by PID file at exit.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
EMAIL=admin@example.com
PASS=adminpass1234

TMP="$(mktemp -d "${TMPDIR:-/tmp}/toki-e2e-devicecert.XXXXXX")"
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
log() { echo "[devicecert] $*" >&2; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }
fail() {
  log "FAIL: $*"
  for f in "$TMP"/*.log; do echo "--- $f" >&2; tail -25 "$f" >&2; done
  exit 1
}

cd "$ROOT"
log "building"
go build -o "$TOKI" ./examples/base

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'; }
PORT_HUB="$(free_port)"; PORT_S1="$(free_port)"; TLS_HUB="$(free_port)"; TLS_S1="$(free_port)"
URL_HUB="http://127.0.0.1:$PORT_HUB"
URL_S1="http://127.0.0.1:$PORT_S1"
HUB="$TMP/hub"; S1="$TMP/s1"

toki() { # role dir tlsport args...
  local role="$1" dir="$2" tls="$3"; shift 3
  TOKI_SYNC_ROLE="$role" TOKI_SYNC_INSECURE=1 TOKI_SCANNER=on TOKI_DEVICECERT_MTLS=optional TOKI_DEVICECERT=on TOKI_DEVICECERT_LISTEN="127.0.0.1:$tls" "$TOKI" "$@" --dev=false --dir "$dir"
}
start() { # name role dir port tlsport
  TOKI_SYNC_ROLE="$2" TOKI_SYNC_INSECURE=1 TOKI_SYNC_INTERVAL=1s TOKI_SCANNER=on TOKI_DEVICECERT_MTLS=optional TOKI_DEVICECERT=on TOKI_DEVICECERT_LISTEN="127.0.0.1:$5" \
    "$TOKI" serve --dev=false --automigrate=false --dir "$3" --http "127.0.0.1:$4" >>"$TMP/$1.log" 2>&1 &
  echo $! >"$TMP/$1.pid"
}
wait_health() { # url seconds [curl args...]
  local i u="$1" n="$2"; shift 2
  for ((i = 0; i < n * 10; i++)); do
    curl -fs "$@" "$u/api/health" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}

# ---- hub ----
toki hub "$HUB" "$((TLS_HUB))" superuser upsert "$EMAIL" "$PASS" >/dev/null
start hub hub "$HUB" "$PORT_HUB" "$TLS_HUB"
wait_health "$URL_HUB" 30 || fail "hub did not start"
log "hub up on $PORT_HUB (TLS $TLS_HUB)"

# the root: PEM on stdout, fingerprint on stderr
toki hub "$HUB" "$TLS_HUB" devicecert ca >"$TMP/root.pem" 2>"$TMP/ca.err" || fail "toki devicecert ca"
grep -q 'BEGIN CERTIFICATE' "$TMP/root.pem" || fail "no root PEM"
FP_HUB="$(toki hub "$HUB" "$TLS_HUB" devicecert ca --fingerprint)"
[ -n "$FP_HUB" ] && grep -q "$FP_HUB" "$TMP/ca.err" || fail "fingerprint mismatch ($FP_HUB)"
openssl x509 -in "$TMP/root.pem" -noout -text | grep -q 'Public Key Algorithm: id-ecPublicKey' || fail "root is not ECDSA"
log "root $FP_HUB"

# the hub serves HTTPS with its own leaf
wait_health "https://127.0.0.1:$TLS_HUB" 20 --cacert "$TMP/root.pem" || fail "hub TLS listener does not verify against the root"
if curl -fs "https://127.0.0.1:$TLS_HUB/api/health" >/dev/null 2>&1; then fail "the TLS listener must not verify without the private root"; fi
log "hub TLS ok"

# ---- spoke ----
SU_ID="$(curl -fsS "$URL_HUB/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["record"]["id"]')"
CODE="$(toki hub "$HUB" "$TLS_HUB" sync enroll --name s1 --profile edge --actor "_superusers/$SU_ID" --allow-superuser-actor | awk '/^code:/ {print $2}')"
[ -n "$CODE" ] || fail "no enrollment code"
toki spoke "$S1" "$TLS_S1" superuser upsert "$EMAIL" "$PASS" >/dev/null
toki spoke "$S1" "$TLS_S1" sync join "$URL_HUB" "$CODE" >/dev/null || fail "join"
start s1 spoke "$S1" "$PORT_S1" "$TLS_S1"
wait_health "$URL_S1" 30 || fail "spoke did not start"

# the leaf arrives with the sync session; the key stays on the spoke
wait_health "https://127.0.0.1:$TLS_S1" 60 --cacert "$TMP/root.pem" || fail "spoke did not get its edge certificate"
log "spoke TLS ok"
[ -f "$S1/devicecert_leaf.key" ] || fail "no leaf key on the spoke"
case "$(uname)" in
  Darwin) MODE="$(stat -f %Lp "$S1/devicecert_leaf.key")" ;;
  *) MODE="$(stat -c %a "$S1/devicecert_leaf.key")" ;;
esac
[ "$MODE" = 600 ] || fail "leaf key mode $MODE"
if grep -q 'PRIVATE KEY' "$HUB/data.db" 2>/dev/null; then fail "a private key leaked into the hub database"; fi

# status shows not_after and the node name
toki spoke "$S1" "$TLS_S1" devicecert status >"$TMP/status.txt" || fail "status"
grep -q '^not_after:' "$TMP/status.txt" || { cat "$TMP/status.txt" >&2; fail "status has no not_after"; }
NODE_DNS="$(toki spoke "$S1" "$TLS_S1" devicecert status --json | jget 'd["leaf"]["dns"][0]')"
case "$NODE_DNS" in *.edge.toki.local) ;; *) fail "node DNS name $NODE_DNS";; esac
curl -fs --cacert "$TMP/root.pem" --resolve "$NODE_DNS:$TLS_S1:127.0.0.1" "https://$NODE_DNS:$TLS_S1/api/health" >/dev/null \
  || fail "the node name must verify"
# the spoke got the same root
FP_S1="$(toki spoke "$S1" "$TLS_S1" devicecert ca --fingerprint)"
[ "$FP_S1" = "$FP_HUB" ] || fail "spoke root $FP_S1 differs from the hub root $FP_HUB"

# the hub recorded both leaves
toki hub "$HUB" "$TLS_HUB" devicecert list >"$TMP/list.txt" || fail "list"
grep -q "server" "$TMP/list.txt" && [ "$(wc -l <"$TMP/list.txt")" -ge 2 ] || { cat "$TMP/list.txt" >&2; fail "list must show the hub and spoke leaves"; }
NODE_ID="${NODE_DNS%.edge.toki.local}"
grep -q "$NODE_ID" "$TMP/list.txt" || fail "the spoke leaf is not in _device_certs"

# health extra on the spoke (superuser)
TOK="$(curl -fsS "$URL_S1/api/collections/_superusers/auth-with-password" -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASS\"}" | jget 'd["token"]')"
curl -fsS "$URL_S1/api/health" -H "Authorization: $TOK" | jget 'd["data"]["devicecert"]["listening"]' | grep -q True || fail "health extra"

# ---- PR 7: client certificates, route scope, revocation through the deny list ----
# the deny list reaches the spoke through a pull-only policy on _device_certs
toki hub "$HUB" "$TLS_HUB" sync policies set _device_certs --direction pull >/dev/null || fail "policy on _device_certs"
OUT="$TMP/peer"
toki hub "$HUB" "$TLS_HUB" devicecert issue --name gate-ctrl-1 --days 90 --scope /api/scan --out "$OUT" >"$TMP/issue.txt" || fail "issue"
[ -f "$OUT/gate-ctrl-1.key.pem" ] && [ -f "$OUT/gate-ctrl-1.crt.pem" ] && [ -f "$OUT/ca.pem" ] || fail "issue wrote no files"
SERIAL="$(awk '/^serial:/ {print $2}' "$TMP/issue.txt")"
[ -n "$SERIAL" ] || fail "no serial"
if toki hub "$HUB" "$TLS_HUB" devicecert issue --name bad --scope /api/collections --out "$TMP/bad" >/dev/null 2>&1; then fail "a scope on /api/collections must be refused"; fi
if command -v openssl >/dev/null; then
  toki hub "$HUB" "$TLS_HUB" devicecert issue --name gate-ctrl-p12 --scope /api/scan --out "$TMP/p12" --p12 --p12-pass x1y2z3 >/dev/null 2>&1 \
    && [ -f "$TMP/p12/gate-ctrl-p12.p12" ] || fail "p12"
fi

pcurl() { # path [extra curl args]: client cert against the spoke's TLS port
  local path="$1"; shift
  curl -s -o "$TMP/body" -w '%{http_code}' --cacert "$TMP/root.pem" --cert "$OUT/gate-ctrl-1.crt.pem" --key "$OUT/gate-ctrl-1.key.pem" "$@" "https://127.0.0.1:$TLS_S1$path" 2>/dev/null || true
}
# the spoke learns the row (scope) with the next pull
CODE_HTTP=000
for ((i = 0; i < 120; i++)); do
  CODE_HTTP="$(pcurl /api/scan/scanners)"
  [ "$CODE_HTTP" = 200 ] && break
  sleep 0.5
done
[ "$CODE_HTTP" = 200 ] || fail "client cert inside its scope must get 200 without a token (got $CODE_HTTP)"
log "scoped client cert ok"
[ "$(pcurl /api/collections/_superusers/records)" != 200 ] || fail "outside the scope must not be served"
[ "$(pcurl /api/scan/scanners -H 'X-Toki-Device: forged')" = 200 ] || fail "scope still works with a forged header"
NOCERT="$(curl -s -o /dev/null -w '%{http_code}' --cacert "$TMP/root.pem" "https://127.0.0.1:$TLS_S1/api/scan/scanners" || true)"
[ "$NOCERT" = 401 ] || fail "no client cert must get 401 (got $NOCERT)"
FORGED="$(curl -s -o /dev/null -w '%{http_code}' --cacert "$TMP/root.pem" -H 'X-Toki-Device: gate-ctrl-1' "https://127.0.0.1:$TLS_S1/api/scan/scanners" || true)"
[ "$FORGED" = 401 ] || fail "a forged X-Toki-Device header must not authenticate (got $FORGED)"
# the spoke's own server leaf is not a client certificate
cp "$S1/devicecert_leaf.key" "$TMP/s1leaf.key"
if curl -s -o /dev/null --cacert "$TMP/root.pem" --cert "$S1/devicecert_leaf.pem" --key "$TMP/s1leaf.key" "https://127.0.0.1:$TLS_S1/api/scan/scanners" 2>/dev/null; then
  fail "a server leaf presented as a client certificate must fail at the TLS layer"
fi

# identity and attest
curl -fsS --cacert "$TMP/root.pem" "https://127.0.0.1:$TLS_S1/api/device/identity" >"$TMP/ident.json" || fail "identity"
[ "$(jget 'd["node_id"]' <"$TMP/ident.json")" = "$NODE_ID" ] || fail "identity node_id"
[ -n "$(jget 'd["cert"]' <"$TMP/ident.json")" ] || fail "identity cert"
NONCE="0123456789abcdef0123"
curl -fsS --cacert "$TMP/root.pem" -H 'Content-Type: application/json' -d "{\"nonce\":\"$NONCE\"}" "https://127.0.0.1:$TLS_S1/api/device/attest" >"$TMP/attest.json" || fail "attest"
[ "$(jget 'd["alg"]' <"$TMP/attest.json")" = Ed25519 ] || fail "attest alg"
[ "$(curl -s -o /dev/null -w '%{http_code}' --cacert "$TMP/root.pem" -H 'Content-Type: application/json' -d '{"nonce":"short"}' "https://127.0.0.1:$TLS_S1/api/device/attest")" = 400 ] || fail "short nonce must be 400"

# CA rotation: both roots are trusted during the overlap, the old-signed peer still works
toki hub "$HUB" "$TLS_HUB" devicecert rotate-ca --overlap-days 30 >"$TMP/rotate.txt" || fail "rotate-ca"
grep -q 'new root' "$TMP/rotate.txt" || fail "rotate-ca output"
[ "$(toki hub "$HUB" "$TLS_HUB" devicecert status --json | jget 'd["cas"]')" = 2 ] || fail "hub must trust 2 roots after rotation"
[ "$(pcurl /api/scan/scanners)" = 200 ] || fail "the old-signed peer must still work during the overlap"

# revoke: the deny list reaches the spoke, then the handshake fails
toki hub "$HUB" "$TLS_HUB" devicecert revoke "$SERIAL" >/dev/null || fail "revoke"
REFUSED=0
for ((i = 0; i < 120; i++)); do
  if ! curl -s -o /dev/null --fail --cacert "$TMP/root.pem" --cert "$OUT/gate-ctrl-1.crt.pem" --key "$OUT/gate-ctrl-1.key.pem" "https://127.0.0.1:$TLS_S1/api/scan/scanners" 2>/dev/null; then REFUSED=1; break; fi
  sleep 0.5
done
[ "$REFUSED" = 1 ] || fail "a revoked client cert must be refused after the pull"
log "revoked client cert refused"
curl -fsS "$URL_S1/api/health" -H "Authorization: $TOK" | jget 'd["data"]["devicecert"]["deny_list"]' | grep -qE '^[1-9]' || fail "health deny_list size"

log "OK"
