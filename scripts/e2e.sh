#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! command -v go >/dev/null 2>&1; then
  export PATH="$HOME/.local/go/bin:$PATH"
fi
if ! command -v go >/dev/null 2>&1; then
  echo "go 1.22+ is required" >&2
  exit 1
fi

RUN_ID="${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}"
PORT="${PORT:-18080}"
SERVER="http://127.0.0.1:${PORT}"
DATA_DIR="data/e2e-${RUN_ID}"
LOG_DIR="logs/e2e-${RUN_ID}"
REPORT_DIR="reports/e2e-${RUN_ID}"
TMP_DIR="tmp/e2e-${RUN_ID}"

mkdir -p "$DATA_DIR" "$LOG_DIR" "$REPORT_DIR" "$TMP_DIR" bin .cache/go-build .cache/gomod
export GOCACHE="$ROOT/.cache/go-build"
export GOMODCACHE="$ROOT/.cache/gomod"
export GOTMPDIR="$TMP_DIR"
export TMPDIR="$TMP_DIR"

echo "build"
go build -trimpath -o bin/configd ./cmd/configd
go build -trimpath -o bin/simclient ./cmd/simclient
go build -trimpath -o bin/scenario ./cmd/scenario

SERVER_PID=""
SIM_PID=""
cleanup() {
  if [ -n "$SIM_PID" ] && kill -0 "$SIM_PID" 2>/dev/null; then
    kill "$SIM_PID" 2>/dev/null || true
    wait "$SIM_PID" 2>/dev/null || true
  fi
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "start config center"
GOMAXPROCS=1 GOMEMLIMIT=768MiB ./bin/configd \
  -addr "127.0.0.1:${PORT}" \
  -db "$DATA_DIR/config.db" \
  -log "$LOG_DIR/configd.log" \
  -seed 300 \
  -seed-namespace payments \
  -canary-percent 10 \
  -observation-seconds 8 \
  -observer-interval 50ms \
  -history-limit 20 \
  >"$LOG_DIR/configd.stdout.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 100); do
  if command -v curl >/dev/null 2>&1 && curl -fsS "$SERVER/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "config center exited before becoming healthy" >&2
    tail -n 100 "$LOG_DIR/configd.log" >&2 || true
    exit 1
  fi
  sleep 0.1
done
if command -v curl >/dev/null 2>&1; then
  curl -fsS "$SERVER/healthz" >/dev/null
fi

echo "start 24 simulated clients"
GOMAXPROCS=1 GOMEMLIMIT=384MiB ./bin/simclient \
  -server "$SERVER" \
  -namespace payments \
  -instances 24 \
  -instance-prefix sim \
  -poll 2s \
  -poll-count 25 \
  -duration 120s \
  -seed 263 \
  -fault-5xx-rate 0.03 \
  -network-error-rate 0.02 \
  -latency-probability 0.30 \
  -latency-min 200ms \
  -latency-max 2s \
  -read-workers 6 \
  -read-interval 200ms \
  -state-db "$DATA_DIR/simclient.db" \
  -report "$REPORT_DIR/simulation-report.json" \
  -log "$LOG_DIR/simclient.log" \
  >"$LOG_DIR/simclient.stdout.log" 2>&1 &
SIM_PID=$!

echo "drive releases and validate"
GOMAXPROCS=1 GOMEMLIMIT=64MiB ./bin/scenario \
  -server "$SERVER" \
  -namespace payments \
  -instances 24 \
  -observation 8s \
  -client-wait 30s \
  -state-wait 30s \
  -report "$REPORT_DIR/simulation-report.json" \
  -report-wait 150s \
  -summary "$REPORT_DIR/scenario-summary.json"

wait "$SIM_PID"
SIM_PID=""

echo
echo "artifacts"
echo "  report:  $REPORT_DIR/simulation-report.json"
echo "  summary: $REPORT_DIR/scenario-summary.json"
echo "  logs:    $LOG_DIR"
echo "  data:    $DATA_DIR"
