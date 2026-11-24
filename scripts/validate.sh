#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WORK=".validation/e2e"
mkdir -p "$WORK"

echo "== go test =="
go test ./... -count=1

echo "== go vet =="
go vet ./...

echo "== windows pure-go cross build =="
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o "$WORK/sample-pipeline.exe" ./cmd/sample-pipeline
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o "$WORK/verify-result.exe" ./cmd/verify-result

echo "== deterministic sample generation =="
go run ./cmd/gen-samples -out samples -seed 20261125
go build -o "$WORK/sample-pipeline" ./cmd/sample-pipeline
go build -o "$WORK/verify-result" ./cmd/verify-result

for name in blocks420 rampcolor444 mixed422; do
  echo "== full run + independent verify: $name =="
  "$WORK/sample-pipeline" run \
    --project "samples/$name/project.json" \
    --db "$WORK/$name.db" \
    --out "$WORK/$name-out" \
    --workers 4 --buffer 2 --chunk-size 8 --fresh
  "$WORK/verify-result" \
    --project "samples/$name/project.json" \
    --truth "samples/$name/truth.json" \
    --db "$WORK/$name.db" | tail -1
done

echo "== precise incremental recompute + full comparison =="
"$WORK/sample-pipeline" edit \
  --in samples/blocks420/project.json \
  --out "$WORK/blocks-depth10.json" \
  --seq 3 --bit-depth 10
cp "$WORK/blocks420.db" "$WORK/blocks-incremental.db"
"$WORK/sample-pipeline" run \
  --project "$WORK/blocks-depth10.json" \
  --db "$WORK/blocks-incremental.db" \
  --out "$WORK/blocks-incremental-out" \
  --workers 3 --buffer 2 --chunk-size 8
"$WORK/sample-pipeline" run \
  --project "$WORK/blocks-depth10.json" \
  --db "$WORK/blocks-edited-full.db" \
  --out "$WORK/blocks-edited-full-out" \
  --workers 1 --buffer 1 --chunk-size 8 --fresh
"$WORK/sample-pipeline" compare \
  --a "$WORK/blocks-incremental.db" \
  --b "$WORK/blocks-edited-full.db" \
  --project-id blocks420

echo "== hard commit boundary recovery =="
set +e
"$WORK/sample-pipeline" run \
  --project samples/blocks420/project.json \
  --db "$WORK/crash-recovery.db" \
  --out "$WORK/crash-out" \
  --chunk-size 8 --crash-after-commit 3
crash_code=$?
set -e
if [[ "$crash_code" -ne 137 ]]; then
  echo "expected crash hook exit 137, got $crash_code" >&2
  exit 1
fi
"$WORK/sample-pipeline" run \
  --project samples/blocks420/project.json \
  --db "$WORK/crash-recovery.db" \
  --out "$WORK/crash-recovered-out" \
  --workers 2 --buffer 1 --chunk-size 8
"$WORK/verify-result" \
  --project samples/blocks420/project.json \
  --truth samples/blocks420/truth.json \
  --db "$WORK/crash-recovery.db" | tail -1

echo "VALIDATE_OK"
