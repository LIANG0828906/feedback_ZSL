#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASE_URL="${BASE_URL:-http://127.0.0.1:5080}"
DURATION="${DURATION:-20}"
RATE="${RATE:-300}"
SEED="${SEED:-20261119}"
REPORT="${REPORT:-$ROOT/artifacts/scenario-report.json}"

dotnet run --project "$ROOT/src/InventoryMigration.LoadGen/InventoryMigration.LoadGen.csproj" -- \
  --base-url "$BASE_URL" \
  --duration "$DURATION" \
  --rate "$RATE" \
  --seed "$SEED" \
  --report "$REPORT" \
  "$@"
