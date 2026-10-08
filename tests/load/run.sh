#!/usr/bin/env bash
# Load/soak harness driver. Copies a pb_data with `sqlite3 .backup` (never touches the source),
# builds toki, runs tests/load and writes tests/load/results/<date>-<tag>.{md,json}.
# NOT run in CI. usage: tests/load/run.sh [--src DIR (env LOAD_SRC)] [--work DIR (env LOAD_WORK)]
#                       [--scenarios all] [--tag wpe] [--addr 127.0.0.1:8097] [--inflate-logs MB] [--quick]
# LOAD_SERVER_ENV="TOKI_X=1 TOKI_Y=2" adds env vars to every server started by the harness.
# [-- extra flags for the Go tool]
set -eu
SRC=${LOAD_SRC:-/opt/pocketbase-fgr/pb_data}; WORK=${LOAD_WORK:-/root/tokibase-load}; SCN=all; TAG=wpe; ADDR=${LOAD_ADDR:-127.0.0.1:8097}; INFLATE=${LOAD_INFLATE_LOGS_MB:-0}; EXTRA=()
while [ $# -gt 0 ]; do case "$1" in
  --src) SRC=$2; shift 2;; --work) WORK=$2; shift 2;; --scenarios) SCN=$2; shift 2;;
  --tag) TAG=$2; shift 2;; --addr) ADDR=$2; shift 2;; --inflate-logs) INFLATE=$2; shift 2;; --quick) EXTRA+=(-quick); shift;; --) shift; EXTRA+=("$@"); break;;
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
# --inflate-logs MB: add synthetic request logs (about 0.55 KB each) so that backup/prune cases start from a big auxiliary.db
if [ "$INFLATE" -gt 0 ]; then
  ROWS=$((INFLATE * 1000000 / 550))
  sqlite3 "$DATA/auxiliary.db" "PRAGMA journal_mode=WAL; WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < $ROWS) INSERT INTO _logs (id, level, message, data, created) SELECT 'inflate'||x, 0, 'GET /api/collections/fgr_notifications/records?page='||x, json_object('type','request','url','/api/collections/fgr_notifications/records?page='||x||'&sort=-created','method','GET','status',200,'execTime',1.5,'auth','users','userAgent','Mozilla/5.0 load-inflate','pad',hex(randomblob(60))), strftime('%Y-%m-%d %H:%M:%fZ','now','-1 hour') FROM c;" >/dev/null
  echo "inflated logs: $(du -sh "$DATA/auxiliary.db" | cut -f1)"
fi
echo "copy: $(du -sh "$DATA" | cut -f1) from $SRC"

( cd "$REPO" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$WORK/toki" ./examples/base )
export TOKI_VERSION_INFO="$("$WORK/toki" --version 2>&1 | head -1) $(cd "$REPO" && git rev-parse --short HEAD)"
echo "toki: $TOKI_VERSION_INFO"

cd "$REPO"
go run ./tests/load -bin "$WORK/toki" -work "$WORK" -addr "$ADDR" -scenarios "$SCN" -tag "$TAG" -out "$REPO/tests/load/results" ${EXTRA[@]+"${EXTRA[@]}"}
