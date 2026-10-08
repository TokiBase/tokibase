#!/usr/bin/env bash
# every 24 h: audit hash chain verify. Result line: "audit_verify OK|FAIL".
. "$(dirname "$0")/lib.sh"
export TOKI_TLS_CHECK=off
cd "$SOAK" || exit 2
out=$("$TOKI" audit verify --dir "$DATA" 2>&1); rc=$?
{ echo "$out" | tail -5
  if [ $rc = 0 ] && echo "$out" | tail -1 | grep -q '^OK'; then echo "RESULT audit_verify OK"; else echo "RESULT audit_verify FAIL (rc=$rc)"; rc=1; fi
} 2>&1 | stamp >> "$SOAK/log/audit.log"
exit $rc
