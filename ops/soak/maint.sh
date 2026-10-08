#!/usr/bin/env bash
# every 6 h: backup create + verify latest + replica status. Result lines: "backup_verify OK|FAIL".
. "$(dirname "$0")/lib.sh"
LOG=$SOAK/log/maint.log
export TOKI_TLS_CHECK=off TOKI_REPLICA_URL=file://$SOAK/replica
cd "$SOAK" || exit 2
rc=0
tmp=$(mktemp)
{
  echo "== backup create"
  "$TOKI" backup create --dir "$DATA" "soak-$(date -u +%Y%m%dT%H%M%SZ).zip" 2>&1 | tail -3
  echo "== backup verify latest"
  out=$("$TOKI" backup verify latest --json --dir "$DATA" 2>&1); vrc=$?
  echo "$out" | tr -d '\n' | cut -c1-600; echo
  if [ $vrc = 0 ] && echo "$out" | grep -q '"integrityOk": *true'; then echo "RESULT backup_verify OK"; else echo "RESULT backup_verify FAIL"; rc=1; fi
  # keep the 6 newest backups (each is a full copy of the FGR data)
  ls -1t "$DATA"/backups/soak-*.zip 2>/dev/null | tail -n +7 | while read -r f; do rm -f "$f" && echo "pruned $(basename "$f")"; done
  echo "== replica status"
  "$TOKI" replica status --dir "$DATA" 2>&1 | head -20 || true
} > "$tmp" 2>&1
stamp < "$tmp" >> "$LOG"
grep -q "RESULT backup_verify FAIL" "$tmp" && rc=1
rm -f "$tmp"
exit $rc
