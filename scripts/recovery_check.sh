#!/usr/bin/env bash
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BIN="$(mktemp /tmp/grayrelease-bin.XXXXXX)"
DB_DIR="$(mktemp -d /tmp/grayrelease-recovery.XXXXXX)"
trap 'rm -f "$BIN"; rm -rf "$DB_DIR"' EXIT

go build -o "$BIN" ./cmd/grayrelease || exit 1
"$BIN" gen --dir "$ROOT/samples" >/dev/null || exit 1

fail=0
for stage in after-routes after-queues before-commit after-commit; do
  DB="$DB_DIR/$stage.db"
  echo "== crash at $stage =="
  "$BIN" recovery-run --db "$DB" --samples "$ROOT/samples" \
    --crash-tick 1 --crash-stage "$stage" >"$DB_DIR/$stage.crash.out" 2>&1
  code=$?
  if [[ $code -ne 137 ]]; then
    echo "expected SIGKILL exit 137 for $stage, got $code"
    fail=1
    continue
  fi

  "$BIN" recovery-resume --db "$DB" --samples "$ROOT/samples" \
    >"$DB_DIR/$stage.resume.json" || {
      echo "resume failed for $stage"
      cat "$DB_DIR/$stage.resume.json" >&2
      fail=1
      continue
    }

  if ! grep -q '"ok": true' "$DB_DIR/$stage.resume.json"; then
    echo "resume report not OK for $stage"
    cat "$DB_DIR/$stage.resume.json" >&2
    fail=1
  fi
  if ! grep -q '"committed_tick": 7' "$DB_DIR/$stage.resume.json"; then
    echo "final committed tick is not 7 for $stage"
    cat "$DB_DIR/$stage.resume.json" >&2
    fail=1
  fi
  echo "  recovered $stage -> committed_tick=7"
done

if [[ $fail -ne 0 ]]; then
  echo "recovery check failed"
  exit 1
fi
echo "all recovery crash points passed"
