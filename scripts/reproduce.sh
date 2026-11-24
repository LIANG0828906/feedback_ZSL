#!/usr/bin/env bash
set -euo pipefail

export CGO_ENABLED=0
export GOPROXY=off

mkdir -p artifacts

echo '== Go environment =='
go version
go env GOMODCACHE

echo '== Generate deterministic local samples =='
go run ./cmd/gensample -out data/sample

echo '== Run unit and acceptance tests =='
go test ./...

echo '== Three-path consistency and parameter invalidation =='
go run ./cmd/verify experiment \
  -data data/sample \
  -out artifacts/experiment-report.json \
  -db-dir artifacts/experiment-dbs \
  -parallelism 4 \
  -buffer-pixels-per-thread 24576 \
  -block-size 16 \
  -threshold 0.55 \
  -cancel-after 2

for clip in noise watermark clean; do
  echo "== Verify ${clip} =="
  extra=()
  if [[ "${clip}" == "noise" ]]; then
    extra=(-inject-failure-once 8:noise_variance -retry-failed=true)
  fi
  go run ./cmd/verify run \
    -sample "data/sample/${clip}" \
    -db "artifacts/${clip}.db" \
    -report "artifacts/report-${clip}.json" \
    -parallelism 4 \
    -buffer-pixels-per-thread 24576 \
    -block-size 16 \
    -window 3 \
    -threshold 0.55 \
    "${extra[@]}"

  echo "== Independent review ${clip} =="
  go run ./cmd/review \
    -db "artifacts/${clip}.db" \
    -truth "data/sample/${clip}/truth.json" \
    -out "artifacts/review-${clip}.json"
done

echo '== Parameter error code check =='
if go run ./cmd/verify run -sample data/sample/noise -db artifacts/bad-param.db -block-size 7 >/tmp/rv-bad-param.out 2>&1; then
  echo 'expected E_PARA for non-dividing block size' >&2
  exit 1
fi
grep -q 'E_PARA' /tmp/rv-bad-param.out
grep -q '"field": "block_size"' /tmp/rv-bad-param.out

echo 'All reproduction steps passed. See artifacts/experiment-report.json.'
