#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/env.sh"

if ! psql_local -d postgres -Atc "SELECT 1 FROM pg_database WHERE datname = '$PGDATABASE_LOCAL'" | grep -qx 1; then
  psql_local -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"$PGDATABASE_LOCAL\"" >/dev/null
  echo "created database $PGDATABASE_LOCAL"
else
  echo "database $PGDATABASE_LOCAL already exists"
fi

psql_local -d "$PGDATABASE_LOCAL" -v ON_ERROR_STOP=1 -c "SELECT current_database()" >/dev/null

