#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
export GOMAXPROCS="${GOMAXPROCS:-2}"
export PATH="${PATH}:/home/liang/go-sdk/bin"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

mkdir -p bin artifacts
go test ./...
go build -o bin/gateway ./cmd/gateway
go build -o bin/upstream ./cmd/upstream
go build -o bin/loadgen ./cmd/loadgen

cleanup() {
  if [[ -n "${GATEWAY_PID:-}" ]]; then kill "${GATEWAY_PID}" 2>/dev/null || true; fi
  if [[ -n "${UPSTREAM_PID:-}" ]]; then kill "${UPSTREAM_PID}" 2>/dev/null || true; fi
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

./bin/upstream \
  -h1 127.0.0.1:18082 \
  -h2 127.0.0.1:18083 \
  -h2c 127.0.0.1:18084 \
  >artifacts/upstream.log 2>&1 &
UPSTREAM_PID=$!

./bin/gateway -config configs/gateway.json >artifacts/gateway.log 2>&1 &
GATEWAY_PID=$!

for _ in $(seq 1 50); do
  if curl --noproxy '*' -fsS http://127.0.0.1:19091/healthz >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl --noproxy '*' -fsS http://127.0.0.1:19091/healthz >/dev/null

curl --noproxy '*' -fsS http://127.0.0.1:18082/__control/reset >/dev/null
curl --noproxy '*' -kfsS https://127.0.0.1:18083/__control/reset >/dev/null
curl --noproxy '*' -fsS http://127.0.0.1:18084/__control/reset >/dev/null

./bin/loadgen \
  -config configs/scenario.json \
  -output artifacts/scenario-report.json
