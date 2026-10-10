#!/usr/bin/env bash
# Parking exit gate of phase 3 (docs/SYNC_DESIGN.md §9.1): a solo hub, two edge
# gates and one nano phone (embed API), each spoke behind a syncproxy that cuts
# the network, 48 simulated hours offline (shared fake clock, 1 h steps), then
# the assertions (a) to (h). Everything is killed by PID file at exit.
#
#   bash tests/e2e/parking.sh            (HOURS=48 by default; HOURS=6 for a quick try)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
HOURS="${HOURS:-48}"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/toki-e2e-parking.XXXXXX")"
cleanup() {
  local f p
  for f in "$TMP"/*.pid "$TMP"/work/*.pid; do
    [ -f "$f" ] || continue
    p="$(cat "$f" 2>/dev/null || true)"
    [ -n "$p" ] && { kill -9 "$p" 2>/dev/null || true; }
  done
  if [ -n "${KEEP:-}" ]; then echo "[parking] kept $TMP" >&2; else rm -rf "$TMP"; fi
}
trap cleanup EXIT

tags() { awk -v p="$1" '$1==p {for (i=3;i<=NF;i++) printf "%s ", $i}' "$ROOT/profiles.txt"; }
EDGE_TAGS="$(tags edge)"
NANO_TAGS="$(tags nano)"

cd "$ROOT"
echo "[parking] building hub (solo), gate (edge), syncproxy, wasm guest, driver (nano)" >&2
go build -o "$TMP/toki-hub" ./examples/base
go build -tags "$EDGE_TAGS" -o "$TMP/toki-gate" ./examples/base
go build -o "$TMP/syncproxy" ./tests/e2e/syncproxy
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -o "$TMP/syncconflict.wasm" ./modules/wasm/testdata/syncconflict
go build -tags "$NANO_TAGS" -o "$TMP/parking" ./tests/e2e/parking

mkdir -p "$TMP/work"
set +e
"$TMP/parking" -hub-bin "$TMP/toki-hub" -gate-bin "$TMP/toki-gate" -proxy-bin "$TMP/syncproxy" \
  -wasm-guest "$TMP/syncconflict.wasm" -work "$TMP/work" -hours "$HOURS"
rc=$?
set -e
if [ "$rc" -ne 0 ]; then
  for f in "$TMP"/work/*.log; do
    [ -f "$f" ] || continue
    echo "--- $f" >&2
    tail -15 "$f" >&2
  done
  # the driver's children are killed through $TMP/work/*.pid by the trap
  for f in "$TMP"/work/*.pid; do
    p="$(cat "$f" 2>/dev/null || true)"; [ -n "$p" ] && kill -9 "$p" 2>/dev/null || true
  done
fi
exit "$rc"
