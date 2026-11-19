#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
NAMESPACE="${NAMESPACE:-webhook-lab}"
PYTHON="${PYTHON:-python3}"
PORT="${PORT:-18080}"
SCENARIO="${SCENARIO:-$ROOT/deploy/scenario-restart.json}"
OUTPUT="${OUTPUT:-$ROOT/artifacts/restart-report.json}"

mkdir -p "$(dirname "$OUTPUT")"
kubectl proxy --port="$PORT" \
  >"${TMPDIR:-/tmp}/webhook-lab-restart-proxy.log" 2>&1 &
PROXY_PID=$!
LOADGEN_PID=""
BASE_URL="http://127.0.0.1:$PORT/api/v1/namespaces/$NAMESPACE/services/webhook-dispatcher:8080/proxy"
cleanup() {
  if [[ -n "$LOADGEN_PID" ]]; then
    kill "$LOADGEN_PID" >/dev/null 2>&1 || true
  fi
  kill "$PROXY_PID" >/dev/null 2>&1 || true
  wait "$PROXY_PID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

"$PYTHON" - "$BASE_URL" <<'PY'
import sys
import time
from urllib.request import urlopen

base_url = sys.argv[1]
deadline = time.time() + 20
while time.time() < deadline:
    try:
        with urlopen(f"{base_url}/readyz", timeout=1) as response:
            if response.status == 200:
                raise SystemExit(0)
    except Exception:
        time.sleep(0.25)
raise SystemExit("coordinator port-forward did not become ready")
PY

"$PYTHON" -m loadgen.generate \
  --scenario "$SCENARIO" \
  --coordinator "$BASE_URL" \
  --output "$OUTPUT" &
LOADGEN_PID=$!

sleep 3
echo "Rolling the two dispatcher pods while load is active..."
kubectl -n "$NAMESPACE" rollout restart deployment/webhook-dispatcher
kubectl -n "$NAMESPACE" rollout status deployment/webhook-dispatcher --timeout=180s

wait "$LOADGEN_PID"
LOADGEN_PID=""
"$PYTHON" "$ROOT/scripts/check_restart.py" "$OUTPUT" \
  --coordinator "$BASE_URL"

