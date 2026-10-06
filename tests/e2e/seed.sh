#!/usr/bin/env bash
# Seeds a pb_data directory with the OFFICIAL PocketBase v0.40.4 binary.
# Prints the pb_data path on stdout (everything else goes to stderr). Idempotent.
set -euo pipefail

PB_VERSION=0.40.4
WORK="${TOKI_E2E_DIR:-${TMPDIR:-/tmp}/toki-e2e}"
WORK="${WORK%/}"
PB_DATA="$WORK/pb_data"
MARK="$PB_DATA/.seeded-$PB_VERSION"
ADMIN_EMAIL=admin@example.com
ADMIN_PASS=adminpass1234
USER_EMAIL=user1@example.com
USER_PASS=userpass1234

log() { echo "[seed] $*" >&2; }

if [ -f "$MARK" ]; then
  log "already seeded: $PB_DATA"
  echo "$PB_DATA"
  exit 0
fi

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) log "unsupported OS"; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) log "unsupported arch"; exit 1 ;;
esac

mkdir -p "$WORK"
BIN="$WORK/pb-$PB_VERSION/pocketbase"
if [ ! -x "$BIN" ]; then
  zip="$WORK/pb.zip"
  url="https://github.com/pocketbase/pocketbase/releases/download/v$PB_VERSION/pocketbase_${PB_VERSION}_${os}_${arch}.zip"
  log "downloading $url"
  curl -fsSL -o "$zip" "$url"
  log "download size: $(wc -c < "$zip") bytes"
  mkdir -p "$WORK/pb-$PB_VERSION"
  unzip -qo "$zip" pocketbase -d "$WORK/pb-$PB_VERSION"
  rm -f "$zip"
  chmod +x "$BIN"
fi

rm -rf "$PB_DATA"
"$BIN" superuser upsert "$ADMIN_EMAIL" "$ADMIN_PASS" --dir "$PB_DATA" >&2

PORT=$((20000 + RANDOM % 20000))
BASE="http://127.0.0.1:$PORT"
"$BIN" serve --dir "$PB_DATA" --http "127.0.0.1:$PORT" >"$WORK/pb.log" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true; wait $PID 2>/dev/null || true' EXIT

for _ in $(seq 1 60); do
  curl -fs "$BASE/api/health" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -fs "$BASE/api/health" >/dev/null || { log "upstream did not start"; cat "$WORK/pb.log" >&2; exit 1; }

api() { # method path [json]
  local m=$1 p=$2 d=${3:-}
  if [ -n "$d" ]; then
    curl -fsS -X "$m" "$BASE$p" -H "Authorization: $TOKEN" -H 'Content-Type: application/json' -d "$d"
  else
    curl -fsS -X "$m" "$BASE$p" -H "Authorization: $TOKEN"
  fi
}

TOKEN=$(curl -fsS "$BASE/api/collections/_superusers/auth-with-password" \
  -H 'Content-Type: application/json' \
  -d "{\"identity\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" | sed -E 's/.*"token":"([^"]+)".*/\1/')
[ -n "$TOKEN" ] || { log "superuser auth failed"; exit 1; }

api POST /api/collections '{
  "name":"posts","type":"base",
  "listRule":"","viewRule":"",
  "createRule":"@request.auth.id != \"\"",
  "updateRule":"@request.auth.id != \"\"",
  "deleteRule":"@request.auth.id != \"\"",
  "fields":[
    {"name":"title","type":"text","required":true},
    {"name":"body","type":"text"},
    {"name":"published","type":"bool"},
    {"name":"file","type":"file","maxSelect":1,"maxSize":5242880},
    {"name":"created","type":"autodate","onCreate":true,"onUpdate":false},
    {"name":"updated","type":"autodate","onCreate":true,"onUpdate":true}
  ]}' >/dev/null

# the default auth collection "users" ships with PocketBase
api POST /api/collections/users/records "{\"email\":\"$USER_EMAIL\",\"password\":\"$USER_PASS\",\"passwordConfirm\":\"$USER_PASS\",\"verified\":true}" >/dev/null

api POST /api/collections/posts/records '{"title":"First post","body":"hello","published":true}' >/dev/null
api POST /api/collections/posts/records '{"title":"Second post","body":"world","published":true}' >/dev/null
api POST /api/collections/posts/records '{"title":"Draft post","body":"wip","published":false}' >/dev/null

kill $PID; wait $PID 2>/dev/null || true
trap - EXIT
touch "$MARK"
log "seeded: $PB_DATA"
echo "$PB_DATA"
