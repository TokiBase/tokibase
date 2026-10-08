#!/usr/bin/env bash
# One-time preparation of /home/ubuntu/soak on the VM (idempotent except the data copy, which is refused if pb_data exists).
# usage: ops/soak/setup.sh [/home/ubuntu/work/tokibase]  (repo clone, already `git pull`ed)
set -eu
REPO=${1:-/home/ubuntu/work/tokibase}; SOAK=/home/ubuntu/soak; SRC=${SOAK_SRC:-/home/ubuntu/qc/src}
export PATH=/usr/local/go/bin:$PATH GOFLAGS=-mod=mod
mkdir -p $SOAK/{log,reports,state,replica}
# 1. binary (stripped solo profile) + VERSION
( cd "$REPO" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $SOAK/toki ./examples/base )
{ git -C "$REPO" rev-parse HEAD; $SOAK/toki --version | head -1; date -u +%FT%TZ; } > $SOAK/VERSION
# 2. data: online copy of the source, never modify it
[ -e $SOAK/pb_data/data.db ] && { echo "pb_data exists, not overwriting"; exit 1; }
mkdir -p $SOAK/pb_data
sqlite3 $SRC/data.db ".backup '$SOAK/pb_data/data.db'"
sqlite3 $SRC/auxiliary.db ".backup '$SOAK/pb_data/auxiliary.db'"
[ -d $SRC/../pb_hooks ] && cp -r $SRC/../pb_hooks $SOAK/pb_hooks || echo "no pb_hooks next to the source: running without hooks"
# 3. soak superuser (credentials stay in 0600 files)
( umask 077; head -c 24 /dev/urandom | base64 | tr -d '/+=\n' > $SOAK/.su-pass; echo soak@local.test > $SOAK/.su-email )
TOKI_TLS_CHECK=off $SOAK/toki superuser upsert "$(cat $SOAK/.su-email)" "$(cat $SOAK/.su-pass)" --dir $SOAK/pb_data
