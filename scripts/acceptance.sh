#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export CGO_ENABLED="${CGO_ENABLED:-0}"
WORKDIR="${WORKDIR:-$(mktemp -d /tmp/audiocheck-accept-XXXX)}"
mkdir -p "$WORKDIR"

echo "== environment =="
go version
echo "CGO_ENABLED=$CGO_ENABLED"
echo "WORKDIR=$WORKDIR"

echo "== go test =="
go test ./...

echo "== generate deterministic sample =="
go run ./cmd/audiocheck gen \
  --dir "$WORKDIR/sample" --frames 12 --frame-size 64 --hop-size 32 \
  --channels 1 --gear 2 --seed 424242

COMMON=(--dir "$WORKDIR/sample" --chunk-frames 2 --workers 2 --pending 1 \
  --buffer-samples 262144 --window 32 --step 8)

echo "== full path =="
go run ./cmd/audiocheck run "${COMMON[@]}" --mode full \
  --db "$WORKDIR/full.db" --report "$WORKDIR/full.json"

echo "== realtime path =="
go run ./cmd/audiocheck run "${COMMON[@]}" --mode realtime \
  --db "$WORKDIR/rt.db" --report "$WORKDIR/rt.json"

echo "== injected stage failure =="
set +e
go run ./cmd/audiocheck run "${COMMON[@]}" --mode full \
  --db "$WORKDIR/resume.db" --report "$WORKDIR/failed.json" \
  --fail-block 1 --fail-times 1 --attempts 1 >"$WORKDIR/stage-failure.out" 2>&1
failure_rc=$?
set -e
if [[ "$failure_rc" -eq 0 ]] || ! grep -q "E_STAGE" "$WORKDIR/stage-failure.out"; then
  cat "$WORKDIR/stage-failure.out"
  echo "expected E_STAGE failure" >&2
  exit 1
fi
echo "observed expected E_STAGE rc=$failure_rc"

echo "== resume path =="
go run ./cmd/audiocheck run "${COMMON[@]}" --mode resume \
  --db "$WORKDIR/resume.db" --report "$WORKDIR/resume.json"

echo "== compare three paths =="
go run ./cmd/audiocheck compare --a "$WORKDIR/full.json" --b "$WORKDIR/rt.json" \
  | tee "$WORKDIR/compare-rt.json" | grep -q '"identical": true'
go run ./cmd/audiocheck compare --a "$WORKDIR/full.json" --b "$WORKDIR/resume.json" \
  | tee "$WORKDIR/compare-resume.json" | grep -q '"identical": true'

echo "== independent verifier =="
go run ./cmd/audio-verify --db "$WORKDIR/resume.db" \
  --truth "$WORKDIR/sample/truth.json" | tee "$WORKDIR/verify.json" \
  | grep -q '"ok": true'

echo "== exception injection links =="
for kind in truncation gear divisibility stage; do
  go run ./cmd/audiocheck inject --kind "$kind" \
    --dir "$WORKDIR/inject-$kind" | tee "$WORKDIR/inject-$kind.json" \
    | grep -q '"ok": true'
done

if [[ -n "${SQLITE3_BIN:-}" && -x "${SQLITE3_BIN}" ]]; then
  echo "== sqlite checkpoint rows =="
  "$SQLITE3_BIN" "$WORKDIR/resume.db" \
    "select block_index,status,commit_seq from blocks order by block_index;" \
    | tee "$WORKDIR/sqlite-blocks.txt"
elif command -v sqlite3 >/dev/null 2>&1; then
  sqlite3 "$WORKDIR/resume.db" \
    "select block_index,status,commit_seq from blocks order by block_index;" \
    | tee "$WORKDIR/sqlite-blocks.txt"
else
  echo "sqlite3 CLI not on PATH; Go verifier already inspected the database."
fi

echo "ACCEPTANCE_OK workdir=$WORKDIR"
