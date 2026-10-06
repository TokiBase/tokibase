#!/usr/bin/env bash
# End-to-end compat: upstream-created pb_data served by TokiBase + official JS SDK.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
TOKI=/tmp/toki

cd "$ROOT"
go build -o "$TOKI" ./examples/base

PB_DATA="$("$HERE/seed.sh")"
RUN="$(mktemp -d "${TMPDIR:-/tmp}/toki-e2e-run.XXXXXX")"
cp -R "$PB_DATA" "$RUN/pb_data"

PORT=$((20000 + RANDOM % 20000))
"$TOKI" serve --dir "$RUN/pb_data" --http "127.0.0.1:$PORT" >"$RUN/toki.log" 2>&1 &
PID=$!
cleanup() { kill $PID 2>/dev/null || true; wait $PID 2>/dev/null || true; rm -rf "$RUN"; }
trap cleanup EXIT

for _ in $(seq 1 60); do
  curl -fs "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 && break
  sleep 0.5
done
curl -fs "http://127.0.0.1:$PORT/api/health" >/dev/null || { echo "toki did not start"; cat "$RUN/toki.log"; exit 1; }

cd "$HERE/sdk"
npm ci --no-audit --no-fund
if ! TOKI_URL="http://127.0.0.1:$PORT" node --test; then
  echo "--- toki log ---"; tail -50 "$RUN/toki.log"; exit 1
fi
