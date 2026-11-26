#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WORKDIR="${WORKDIR:-$(mktemp -d)}"
BIN="$WORKDIR/probebench"
ADDR="${ADDR:-127.0.0.1:18088}"
mkdir -p "$WORKDIR"

echo "[1/6] go version"
go version

echo "[2/6] go vet"
go vet ./...

echo "[3/6] go test"
go test ./...

echo "[4/6] build CLI/workbench server"
go build -o "$BIN" ./cmd/probebench

echo "[5/6] deterministic acceptance"
"$BIN" verify --dir "$WORKDIR/acceptance"

echo "[6/6] SIGKILL boundary and resume"
set +e
"$BIN" crash --dir "$WORKDIR/manual"
CRASH_CODE=$?
set -e
echo "crash_exit_code=$CRASH_CODE"
if [[ "$CRASH_CODE" -ne 137 ]]; then
  echo "expected SIGKILL exit code 137, got $CRASH_CODE" >&2
  exit 1
fi
"$BIN" resume --dir "$WORKDIR/manual"

echo "workdir=$WORKDIR"
echo "Optional workbench: $BIN serve --addr $ADDR --dir $WORKDIR/workbench"
