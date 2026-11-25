#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
mkdir -p artifacts

LOG="artifacts/verification-run.log"
: > "$LOG"

run_step() {
  {
    echo
    echo "===== $* ====="
    "$@"
  } 2>&1 | tee -a "$LOG"
}

{
  echo "grayrelease verification"
  date -u +"%Y-%m-%dT%H:%M:%SZ"
} | tee "$LOG"

run_step go version
run_step go build ./...
run_step go vet ./...
run_step go test -count=1 ./...
run_step go run ./cmd/grayrelease gen --dir samples
run_step go run ./cmd/grayrelease demo --samples samples --out artifacts/demo-report.json
run_step go run ./cmd/grayrelease parallel-check --samples samples
run_step go run ./cmd/grayrelease backpressure-demo --samples samples --route-buffer 2 --workers 4
run_step go run ./cmd/grayrelease incremental-demo --samples samples --out artifacts/incremental-report.json

for kind in overlap bad-scale missing-service ambiguous-scale truncated-checkpoint version-mismatch worker-budget worker-budget-exhausted; do
  echo "===== inject $kind =====" | tee -a "$LOG"
  set +e
  go run ./cmd/grayrelease inject --kind "$kind" --workdir artifacts/inject \
    > "artifacts/inject-$kind.json" 2>>"$LOG"
  code=$?
  set -e
  if [[ $code -ne 1 ]]; then
    echo "injection $kind should exit 1, got $code" | tee -a "$LOG"
    exit 1
  fi
  grep -q 'E_' "artifacts/inject-$kind.json"
  cat "artifacts/inject-$kind.json" | tee -a "$LOG"
done

run_step bash scripts/recovery_check.sh

echo | tee -a "$LOG"
echo "VERIFICATION PASSED; log=$LOG" | tee -a "$LOG"
