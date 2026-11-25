#!/usr/bin/env bash
source "$(dirname "$0")/common.sh"

mkdir -p bin artifacts/demo
go build -o bin/demo ./cmd/demo
db="artifacts/demo/script-crash.db"
rm -f "$db" "$db-wal" "$db-shm"

set +e
./bin/demo crash --db "$db" --after 4 --mode before_global_seq
crash_code=$?
set -e

echo "crash_exit_code=$crash_code (137 means SIGKILL)"
test "$crash_code" -eq 137

python3 - "$db" <<'PY'
import sqlite3
import sys
con = sqlite3.connect(sys.argv[1])
seq = con.execute("select committed_event_seq from connection_checkpoint").fetchone()[0]
print("visible_global_seq_after_kill=", seq)
assert seq == 3, seq
PY

./bin/demo recover --db "$db" | tee artifacts/demo/script-recovery.json
grep -q '"matches_full_run": true' artifacts/demo/script-recovery.json
