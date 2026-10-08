#!/usr/bin/env bash
# Load/soak harness driver. Copies a pb_data with `sqlite3 .backup` (never touches the source),
# builds toki, runs tests/load and writes tests/load/results/<date>-<tag>.{md,json}.
# NOT run in CI. usage: tests/load/run.sh [--src DIR (env LOAD_SRC)] [--work DIR (env LOAD_WORK)]
#                       [--scenarios all] [--tag wpe] [--quick] [-- extra flags for the Go tool]
set -eu
SRC=${LOAD_SRC:-/opt/pocketbase-fgr/pb_data}; WORK=${LOAD_WORK:-/root/tokibase-load}; SCN=all; TAG=wpe; EXTRA=()
while [ $# -gt 0 ]; do case "$1" in
  --src) SRC=$2; shift 2;; --work) WORK=$2; shift 2;; --scenarios) SCN=$2; shift 2;;
  --tag) TAG=$2; shift 2;; --quick) EXTRA+=(-quick); shift;; --) shift; EXTRA+=("$@"); break;;
  *) echo "unknown arg $1"; exit 2;; esac; done
REPO=$(cd "$(dirname "$0")/../.." && pwd)
[ -d /root/gotmp ] && export GOTMPDIR=${GOTMPDIR:-/root/gotmp}
export GOFLAGS=${GOFLAGS:--mod=mod} PATH=$PATH:/usr/local/go/bin
[ -n "${GOTMPDIR:-}" ] && mkdir -p "$GOTMPDIR"; mkdir -p "$WORK"
[ -f "$SRC/data.db" ] || { echo "source pb_data missing: $SRC"; exit 2; }
command -v sqlite3 >/dev/null || { echo "sqlite3 required"; exit 2; }

# never leave a server behind: stop by PID file only (no pkill/pgrep -f)
stop() { if [ -f "$WORK/server.pid" ]; then kill "$(cat "$WORK/server.pid")" 2>/dev/null || true; fi; }
trap stop EXIT
stop; sleep 1

DATA="$WORK/pb_data"
# automigrate writes pb_migrations next to the cwd; stale ones break a fresh copy
rm -rf "$DATA" "$WORK/replica" "$WORK/pb_migrations"; mkdir -p "$DATA"
sqlite3 "$SRC/data.db" ".backup '$DATA/data.db'"
sqlite3 "$SRC/auxiliary.db" ".backup '$DATA/auxiliary.db'"
echo "copy: $(du -sh "$DATA" | cut -f1) from $SRC"

( cd "$REPO" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$WORK/toki" ./examples/base )
export TOKI_VERSION_INFO="$("$WORK/toki" --version 2>&1 | head -1) $(cd "$REPO" && git rev-parse --short HEAD)"
echo "toki: $TOKI_VERSION_INFO"

cd "$REPO"
go run ./tests/load -bin "$WORK/toki" -work "$WORK" -addr 127.0.0.1:8097 -scenarios "$SCN" -tag "$TAG" -out "$REPO/tests/load/results" ${EXTRA[@]+"${EXTRA[@]}"}
