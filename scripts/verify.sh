#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "== environment =="
uname -a
go version

echo "== module =="
go mod verify

echo "== gofmt =="
FMT_FILES="$(gofmt -l main.go internal)"
if [[ -n "$FMT_FILES" ]]; then
  echo "unformatted files:"
  echo "$FMT_FILES"
  exit 1
fi
echo "gofmt clean"

echo "== vet =="
go vet ./...

echo "== unit/integration tests =="
go test ./... -count=1

echo "== race detector =="
go test -race ./internal/gateway -count=1

echo "== deterministic sample =="
go run . generate --seed 20261126 --out artifacts/sample
go run . validate --schema artifacts/sample/schema-v1.json
go run . validate --schema artifacts/sample/schema-v2.json
go run . validate --schema artifacts/sample/schema-v3.json
go run . validate --parallel --schema artifacts/sample/schema-v1.json

WORK="$(mktemp -d)"
BIN="$WORK/gatewayproto"
go build -o "$BIN" .

echo "== SIGKILL after two export batches =="
CRASH_DB="$WORK/crash.sqlite"
CRASH_EXPORT="$WORK/crash-export"
set +e
"$BIN" run --schema artifacts/sample/schema-v1.json --db "$CRASH_DB" \
  --export-dir "$CRASH_EXPORT" --crash-after-batch 2 \
  >"$WORK/crash.stdout" 2>"$WORK/crash.stderr"
CRASH_RC=$?
set -e
echo "first run exit=$CRASH_RC (SIGKILL expected 137)"
if [[ "$CRASH_RC" -ne 137 ]]; then
  cat "$WORK/crash.stderr"
  echo "expected exit 137"
  exit 1
fi
"$BIN" inspect-checkpoint --db "$CRASH_DB"
"$BIN" run --schema artifacts/sample/schema-v1.json --db "$CRASH_DB" --export-dir "$CRASH_EXPORT"
"$BIN" inspect-checkpoint --db "$CRASH_DB"

echo "== version mismatch forces full recompute =="
MISMATCH_DB="$WORK/mismatch.sqlite"
"$BIN" run --schema artifacts/sample/schema-v1.json --db "$MISMATCH_DB" --export-dir "$WORK/mismatch-export" >/dev/null
sed 's/seed-20261126-v1/seed-other-version/' artifacts/sample/schema-v1.json >"$WORK/schema-wrong-version.json"
"$BIN" run --schema "$WORK/schema-wrong-version.json" --db "$MISMATCH_DB" --export-dir "$WORK/mismatch-export-wrong" | tee "$WORK/mismatch.out"
grep -q "version_mismatch_full_recompute" "$WORK/mismatch.out"
"$BIN" inspect-checkpoint --db "$MISMATCH_DB" --expect-version seed-other-version

echo "== truncated checkpoint returns stable code =="
TRUNC_DB="$WORK/truncated.sqlite"
python3 - "$CRASH_DB" "$TRUNC_DB" <<'PY'
import os, sys
src, dst = sys.argv[1], sys.argv[2]
data = open(src, 'rb').read()
open(dst, 'wb').write(data[:len(data)//2])
PY
set +e
"$BIN" inspect-checkpoint --db "$TRUNC_DB" >"$WORK/trunc.out" 2>"$WORK/trunc.err"
TRUNC_RC=$?
set -e
if [[ "$TRUNC_RC" -eq 0 ]]; then
  echo "truncated checkpoint unexpectedly accepted"
  exit 1
fi
grep -q "E_CHECKPOINT_CORRUPT" "$WORK/trunc.err"
echo "truncated checkpoint rejected with E_CHECKPOINT_CORRUPT"

echo "VERIFY_OK work=$WORK"
