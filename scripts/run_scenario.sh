#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
NAMESPACE="${NAMESPACE:-webhook-lab}"
PYTHON="${PYTHON:-python3}"
PORT="${PORT:-18080}"
SCENARIO="${SCENARIO:-$ROOT/deploy/scenario-slow.json}"
OUTPUT="${OUTPUT:-$ROOT/artifacts/scenario-report.json}"

mkdir -p "$(dirname "$OUTPUT")"
kubectl -n "$NAMESPACE" port-forward svc/webhook-dispatcher "$PORT:8080" \
  >"${TMPDIR:-/tmp}/webhook-lab-port-forward.log" 2>&1 &
FORWARD_PID=$!
cleanup() {
  kill "$FORWARD_PID" >/dev/null 2>&1 || true
  wait "$FORWARD_PID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

"$PYTHON" - "$PORT" <<'PY'
import sys
import time
from urllib.request import urlopen

port = sys.argv[1]
deadline = time.time() + 20
while time.time() < deadline:
    try:
        with urlopen(f"http://127.0.0.1:{port}/readyz", timeout=1) as response:
            if response.status == 200:
                raise SystemExit(0)
    except Exception:
        time.sleep(0.25)
raise SystemExit("coordinator port-forward did not become ready")
PY

"$PYTHON" -m loadgen.generate \
  --scenario "$SCENARIO" \
  --coordinator "http://127.0.0.1:$PORT" \
  --output "$OUTPUT"
echo "Report written to $OUTPUT"

