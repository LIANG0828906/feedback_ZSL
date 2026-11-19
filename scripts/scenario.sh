#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/env.sh"

GO_BIN="${GO_BIN:-go}"
SCENARIO_PORT="${DRIFT_SCENARIO_PORT:-18315}"
SCENARIO_HTTP="127.0.0.1:${SCENARIO_PORT}"
mkdir -p "$ARTIFACT_DIR"

if [[ "${1:-}" != "--skip-init" ]]; then
  "$SCRIPT_DIR/init_lab.sh"
fi

if [[ -f "$LAB_DIR/kind.kubeconfig" ]]; then
  export DRIFT_KUBECONFIG_DEV="${DRIFT_KUBECONFIG_DEV:-$LAB_DIR/kind.kubeconfig}"
  export DRIFT_KUBECONFIG_PROD="${DRIFT_KUBECONFIG_PROD:-$LAB_DIR/kind.kubeconfig}"
fi

"$GO_BIN" build -o "$LAB_DIR/driftd" ./cmd/driftd
"$GO_BIN" build -o "$LAB_DIR/reportmerge" ./cmd/reportmerge

psql_local -d "$PGDATABASE_LOCAL" -v ON_ERROR_STOP=1 \
  -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null

run_round() {
  local output="$1"
  shift
  env DRIFT_ADMISSION_TIMEOUT="${DRIFT_ADMISSION_TIMEOUT:-20s}" \
    DRIFT_ADMISSION_POLL="${DRIFT_ADMISSION_POLL:-500ms}" \
    "$LAB_DIR/driftd" --once "$@" >"$output"
}

run_round_timeout() {
  local output="$1"
  local timeout="$2"
  env DRIFT_ADMISSION_TIMEOUT="$timeout" \
    DRIFT_ADMISSION_POLL=250ms \
    "$LAB_DIR/driftd" --once >"$output"
}

started_ms="$(date +%s%3N)"

"$SCRIPT_DIR/inject.sh" reset
run_round "$ARTIFACT_DIR/01-baseline.json"

"$SCRIPT_DIR/inject.sh" policy
run_round "$ARTIFACT_DIR/02-policy.json"

"$SCRIPT_DIR/inject.sh" reset
"$SCRIPT_DIR/inject.sh" env-conflict
run_round "$ARTIFACT_DIR/03-env-conflict.json"

"$SCRIPT_DIR/inject.sh" reset
"$SCRIPT_DIR/inject.sh" stuck-rollout
run_round_timeout "$ARTIFACT_DIR/04-stuck-rollout.json" 1500ms

"$SCRIPT_DIR/inject.sh" reset
"$SCRIPT_DIR/inject.sh" env-conflict
run_round "$ARTIFACT_DIR/05-unobservable-baseline.json"
"$SCRIPT_DIR/inject.sh" api-unreachable
env DRIFT_KUBECONFIG_PROD="$LAB_DIR/prod-unreachable.kubeconfig" \
  DRIFT_CLUSTER_TIMEOUT=2s DRIFT_ADMISSION_TIMEOUT=3s DRIFT_ADMISSION_POLL=250ms \
  "$LAB_DIR/driftd" --once >"$ARTIFACT_DIR/06-unobservable.json"

run_round "$ARTIFACT_DIR/07-restart.json"

server_log="$ARTIFACT_DIR/server.log"
DRIFT_INTERVAL=1h DRIFT_HTTP_ADDR="$SCENARIO_HTTP" \
  "$LAB_DIR/driftd" >"$server_log" 2>&1 &
server_pid=$!
cleanup_server() {
  if kill -0 "$server_pid" >/dev/null 2>&1; then
    kill "$server_pid" >/dev/null 2>&1 || true
    wait "$server_pid" 2>/dev/null || true
  fi
}
trap cleanup_server EXIT

status_url="http://${SCENARIO_HTTP}/api/v1/clusters/cluster-prod/status"
for _ in $(seq 1 60); do
  if curl -fsS "$status_url" >"$ARTIFACT_DIR/08-api-prod-status.json" 2>/dev/null; then
    break
  fi
  sleep 0.5
done
if [[ ! -s "$ARTIFACT_DIR/08-api-prod-status.json" ]]; then
  echo "API status endpoint did not return a committed cluster status" >&2
  exit 1
fi
curl -fsS "http://${SCENARIO_HTTP}/metrics" >"$ARTIFACT_DIR/metrics.txt"
cleanup_server
trap - EXIT

finished_ms="$(date +%s%3N)"
duration_ms="$((finished_ms - started_ms))"

"$LAB_DIR/reportmerge" \
  --out "$ROOT_DIR/artifacts/scenario-report.json" \
  --seed "$SEED" \
  --duration-ms "$duration_ms" \
  --metrics "$ARTIFACT_DIR/metrics.txt" \
  "01-baseline=$ARTIFACT_DIR/01-baseline.json" \
  "02-policy=$ARTIFACT_DIR/02-policy.json" \
  "03-env-conflict=$ARTIFACT_DIR/03-env-conflict.json" \
  "04-stuck-rollout=$ARTIFACT_DIR/04-stuck-rollout.json" \
  "05-unobservable-baseline=$ARTIFACT_DIR/05-unobservable-baseline.json" \
  "06-unobservable=$ARTIFACT_DIR/06-unobservable.json" \
  "07-restart=$ARTIFACT_DIR/07-restart.json" \
  "08-api-prod-status=$ARTIFACT_DIR/08-api-prod-status.json"
