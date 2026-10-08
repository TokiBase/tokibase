# shared helpers, sourced by the soak scripts
SOAK=${SOAK:-/home/ubuntu/soak}
PORT=${SOAK_PORT:-8099}
BASE=http://127.0.0.1:$PORT
TOKI=$SOAK/toki
DATA=$SOAK/pb_data
ts() { date -u +%FT%TZ; }
# every output line gets a UTC timestamp: cmd 2>&1 | stamp >> log
stamp() { while IFS= read -r l; do printf '%s %s\n' "$(ts)" "$l"; done; }
su_token() {
  curl -s -m 15 "$BASE/api/collections/_superusers/auth-with-password" \
    -d "identity=$(cat "$SOAK/.su-email")" --data-urlencode "password=$(cat "$SOAK/.su-pass")" | jq -r '.token // empty'
}
svc_pid() { systemctl show -p MainPID --value toki-soak; }
