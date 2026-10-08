#!/usr/bin/env bash
# Shared helpers for the e2e suites. Source it; do not execute.
# e2e_start_server: builds toki (or reuses $TOKI_BIN), copies the seeded pb_data,
# starts the server on a random loopback port. Sets RUN, PORT, PID, TOKI_URL.
# e2e_cleanup: kills the server by PID file and removes the run dir.

e2e_start_server() {
  local here root
  here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  root="$(cd "$here/../.." && pwd)"

  RUN="$(mktemp -d "${TMPDIR:-/tmp}/toki-e2e-run.XXXXXX")"
  if [ -n "${TOKI_BIN:-}" ]; then
    TOKI="$TOKI_BIN"
  else
    TOKI="$RUN/toki"
    (cd "$root" && go build -o "$TOKI" ./examples/base)
  fi

  local pb_data
  pb_data="$("$here/seed.sh")"
  cp -R "$pb_data" "$RUN/pb_data"

  PORT=$((20000 + RANDOM % 20000))
  "$TOKI" serve --dir "$RUN/pb_data" --http "127.0.0.1:$PORT" >"$RUN/toki.log" 2>&1 &
  echo $! >"$RUN/toki.pid"
  PID=$!

  for _ in $(seq 1 60); do
    curl -fs "http://127.0.0.1:$PORT/api/health" >/dev/null 2>&1 && break
    sleep 0.5
  done
  if ! curl -fs "http://127.0.0.1:$PORT/api/health" >/dev/null; then
    echo "toki did not start"; cat "$RUN/toki.log"; return 1
  fi
  TOKI_URL="http://127.0.0.1:$PORT"
  export TOKI_URL
}

e2e_cleanup() {
  if [ -n "${RUN:-}" ] && [ -f "$RUN/toki.pid" ]; then
    local p; p="$(cat "$RUN/toki.pid")"
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  fi
  [ -n "${RUN:-}" ] && rm -rf "$RUN"
}
