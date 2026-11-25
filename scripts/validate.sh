#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

go build ./...
go test ./...
go run ./cmd/qkernel demo --out demo-output --frames 3 --workers 4
go run ./cmd/qkernel verify --root demo-output >/tmp/qkernel-verify.json

python3 - <<'PY'
import json
r = json.load(open('/tmp/qkernel-verify.json'))
assert r['ok'], r
print(f"verify ok: run={r['run_id']} commits={r['commit_count']}")
PY

echo "all validation checks passed"
