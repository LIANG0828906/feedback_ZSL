#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OLD_PORT="${OLD_PORT:-55432}"
NEW_PORT="${NEW_PORT:-55433}"
PGHOST="${PGHOST:-127.0.0.1}"
PGUSER="${PGUSER:-postgres}"
PSQL="${PSQL:-psql}"
CREATEDB="${CREATEDB:-createdb}"

export PGPASSWORD="${PGPASSWORD:-postgres}"

create_database_if_missing() {
  local port="$1"
  local database="$2"
  local found
  found="$("$PSQL" -h "$PGHOST" -p "$port" -U "$PGUSER" -d postgres -tAc \
    "SELECT 1 FROM pg_database WHERE datname = '$database'")"
  if [[ "$found" != "1" ]]; then
    "$CREATEDB" -h "$PGHOST" -p "$port" -U "$PGUSER" "$database"
  fi
}

create_database_if_missing "$OLD_PORT" inventory_old
create_database_if_missing "$NEW_PORT" inventory_new

"$PSQL" -v ON_ERROR_STOP=1 -h "$PGHOST" -p "$OLD_PORT" -U "$PGUSER" \
  -d inventory_old -f "$ROOT/sql/old_schema.sql"
"$PSQL" -v ON_ERROR_STOP=1 -h "$PGHOST" -p "$OLD_PORT" -U "$PGUSER" \
  -d inventory_old -f "$ROOT/sql/seed_old.sql"
"$PSQL" -v ON_ERROR_STOP=1 -h "$PGHOST" -p "$NEW_PORT" -U "$PGUSER" \
  -d inventory_new -f "$ROOT/sql/new_schema.sql"

if [[ "${KEEP_LEDGER:-0}" != "1" ]]; then
  find "$ROOT/src/InventoryMigration.Api/data" -maxdepth 1 -type f -delete 2>/dev/null || true
fi

echo "inventory_old and inventory_new are ready."
