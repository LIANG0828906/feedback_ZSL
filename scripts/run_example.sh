#!/usr/bin/env bash
# End-to-end local example: sample generation, edit session run, tests.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== go version =="
go version

echo "== generate deterministic sample =="
go run ./cmd/levelsession sample -scenario basic -out samples/basic

echo "== run editing session (incremental + preview/finalize + three-path compare) =="
go run ./cmd/levelsession run -sample samples/basic -store artifacts/store -report artifacts/report

echo "== resume from committed checkpoints =="
go run ./cmd/levelsession resume -sample samples/basic -store artifacts/store

echo "== generate anomaly scenario samples =="
for s in budget conn conn-fail hist; do
  go run ./cmd/levelsession sample -scenario "$s" -out "samples/$s" > /dev/null
  echo "generated samples/$s"
done

echo "== unit tests (error codes, consistency, recovery) =="
go test ./...
