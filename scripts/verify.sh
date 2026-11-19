#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/env.sh"

GO_BIN="${GO_BIN:-go}"

cd "$ROOT_DIR"
if [[ -n "$(gofmt -l cmd internal)" ]]; then
  gofmt -l cmd internal
  echo "gofmt reported unformatted files" >&2
  exit 1
fi
"$GO_BIN" test ./...

DRIFT_PGDATABASE="${PGDATABASE_LOCAL}_test" "$SCRIPT_DIR/init_db.sh"
find_psql
LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}" TEST_DATABASE_URL="postgres://${PGUSER_LOCAL}@${PGHOST_LOCAL}:${PGPORT_LOCAL}/${PGDATABASE_LOCAL}_test?sslmode=disable" \
  "$GO_BIN" test ./internal/store -v

if [[ "${1:-}" != "--no-scenario" ]]; then
  "$SCRIPT_DIR/scenario.sh"
fi

