#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD="${1:-$ROOT/build}"
CLI="$BUILD/meshrepair_cli"

if [[ ! -x "$CLI" ]]; then
  echo "missing CLI: $CLI" >&2
  echo "build first: cmake -S \"$ROOT\" -B \"$BUILD\" && cmake --build \"$BUILD\" -j" >&2
  exit 2
fi

echo "[1/6] CTest acceptance suite (includes duplicate-fill interception)"
ctest --test-dir "$BUILD" --output-on-failure

TMP="$(mktemp -d "${TMPDIR:-/tmp}/meshrepair-accept.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

echo "[2/6] deterministic sample and clean repair"
"$CLI" generate --out "$TMP/sample.mesh" --config "$TMP/config.cfg" --seed 42 >/dev/null
"$CLI" repair --input "$TMP/sample.mesh" --config "$TMP/config.cfg" \
  --out "$TMP/clean" --json >"$TMP/clean.json"

grep -q '"cross_chunk":true' "$TMP/clean/report.json"
grep -q '"filled_faces":2' "$TMP/clean/report.json"
grep -q '"split_edges":1' "$TMP/clean/report.json"
grep -q '"neighbor"' "$TMP/clean/boundary_map.json"
grep -q '"straddling_faces"' "$TMP/clean/chunk_table.json"

echo "[3/6] budget rejection"
sed 's/^block_memory_bytes=.*/block_memory_bytes=4096/' \
  "$TMP/config.cfg" >"$TMP/low-budget.cfg"
set +e
"$CLI" repair --input "$TMP/sample.mesh" --config "$TMP/low-budget.cfg" \
  --out "$TMP/rejected" >"$TMP/rejected.out" 2>"$TMP/rejected.err"
BUDGET_EXIT=$?
set -e
[[ "$BUDGET_EXIT" -eq 14 ]]
grep -q 'E_BUDGET_EXCEEDED' "$TMP/rejected.out" "$TMP/rejected.err"

echo "[4/6] hard kill after two atomic commits"
set +e
"$CLI" repair --input "$TMP/sample.mesh" --config "$TMP/config.cfg" \
  --out "$TMP/resumed" --stop-after 2 >/dev/null
KILL_EXIT=$?
set -e
[[ "$KILL_EXIT" -eq 137 ]]
[[ -f "$TMP/resumed/chunks/chunk_000000.mrchunk" ]]
[[ -f "$TMP/resumed/chunks/chunk_000001.mrchunk" ]]
[[ ! -f "$TMP/resumed/repaired.mesh" ]]

echo "[5/6] resume and verify byte identity"
"$CLI" repair --input "$TMP/sample.mesh" --config "$TMP/config.cfg" \
  --out "$TMP/resumed" >/dev/null
"$CLI" verify --input "$TMP/sample.mesh" --config "$TMP/config.cfg" \
  --out "$TMP/resumed" >/dev/null
cmp "$TMP/clean/repaired.mesh" "$TMP/resumed/repaired.mesh"
cmp "$TMP/clean/trace.log" "$TMP/resumed/trace.log"
cmp "$TMP/clean/stitch.sha256" "$TMP/resumed/stitch.sha256"

echo "[6/6] hashes"
cat "$TMP/clean/stitch.sha256"
echo "ACCEPTANCE PASS"
