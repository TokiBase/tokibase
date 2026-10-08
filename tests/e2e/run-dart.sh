#!/usr/bin/env bash
# End-to-end compat: upstream-created pb_data served by TokiBase + official Dart SDK.
# Reuse a prebuilt binary with TOKI_BIN=/path/to/toki.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$HERE/lib.sh"

trap e2e_cleanup EXIT
e2e_start_server

cd "$HERE/dart"
dart pub get
rc=0
dart test --reporter expanded || rc=$?
if [ "$rc" -ne 0 ]; then
  echo "--- toki log ---"; tail -50 "$RUN/toki.log"
  echo "RESULT: FAIL (dart test exit $rc)"
  exit 1
fi
echo "RESULT: PASS (official Dart SDK suite)"
