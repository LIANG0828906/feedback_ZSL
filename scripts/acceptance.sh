#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== Go version =="
go version

echo "== Unit/integration tests =="
go test ./... -count=1

echo "== Deterministic sample generation =="
go run ./cmd/mocapbench generate -out samples

echo "== Schema/cycle/quaternion/truth validation =="
go run ./cmd/mocapbench validate -samples samples

echo "== Three-path verification for every clip/target =="
for clip in walk_cycle brisk_walk idle_shift; do
  for target in target_hero_tall target_compact; do
    go run ./cmd/mocapbench verify \
      -samples samples \
      -clip "$clip" \
      -target "$target" \
      -workers 3 \
      -buffer 4 \
      -out "reports/${clip}_${target}.json"
  done
done

echo "== Windows pure-Go cross build =="
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /tmp/mocapbench.exe ./cmd/mocapbench
ls -lh /tmp/mocapbench.exe

echo "ACCEPTANCE OK"
