#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export CGO_ENABLED=0
export GOTOOLCHAIN=local

DATA_DIR="${DATA_DIR:-data/validation}"
ARTIFACT_DIR="${ARTIFACT_DIR:-artifacts/validation}"
PORT="${PORT:-18123}"
DB="$DATA_DIR/acoustic.db"
SERVE_DB="$DATA_DIR/serve.db"

mkdir -p "$DATA_DIR" "$ARTIFACT_DIR" bin
rm -f "$DB" "$DB-wal" "$DB-shm" "$SERVE_DB" "$SERVE_DB-wal" "$SERVE_DB-shm"

{
  echo "go=$(go version)"
  echo "goos=$(go env GOOS)"
  echo "goarch=$(go env GOARCH)"
  echo "cgo=$(go env CGO_ENABLED)"
  echo "sqlite=modernc.org/sqlite v1.34.5 (pure Go)"
} | tee "$ARTIFACT_DIR/environment.txt"

go build -o bin/acoustic ./cmd/acoustic

./bin/acoustic generate \
  --out "$DATA_DIR/sample.json" \
  --edits "$DATA_DIR/edits.json" \
  --seed 42 | tee "$ARTIFACT_DIR/00-generate.txt"

./bin/acoustic validate --input "$DATA_DIR/sample.json" | tee "$ARTIFACT_DIR/00-validate.txt"

./bin/acoustic solve \
  --input "$DATA_DIR/sample.json" \
  --db "$DB" \
  --out "$ARTIFACT_DIR/01-solve.json" | tee "$ARTIFACT_DIR/01-solve.txt"

# Intentional interruption after one checkpoint of the final edit. The edited
# project file is persisted before the solve, so a new process can resume.
./bin/acoustic edit \
  --input "$DATA_DIR/sample.json" \
  --edits "$DATA_DIR/edits.json" \
  --db "$DB" \
  --project-out "$DATA_DIR/edited.json" \
  --out "$ARTIFACT_DIR/02-edit-interrupted.json" \
  --crash-after 1 | tee "$ARTIFACT_DIR/02-edit-interrupted.txt"

./bin/acoustic resume \
  --input "$DATA_DIR/edited.json" \
  --db "$DB" \
  --out "$ARTIFACT_DIR/03-resume.json" | tee "$ARTIFACT_DIR/03-resume.txt"

./bin/acoustic verify \
  --base "$DATA_DIR/sample.json" \
  --edits "$DATA_DIR/edits.json" \
  --db "$DB" \
  --out "$ARTIFACT_DIR/04-verify.json" | tee "$ARTIFACT_DIR/04-verify.txt"

if ! grep -q '"ok": true' "$ARTIFACT_DIR/04-verify.json"; then
  echo "verification failed" >&2
  exit 1
fi
if ! grep -q '"three_paths_digest_equal": true' "$ARTIFACT_DIR/04-verify.json"; then
  echo "truth/full/incremental digests differ" >&2
  exit 1
fi

cp "$DB" "$SERVE_DB"
./bin/acoustic serve \
  --input "$DATA_DIR/edited.json" \
  --db "$SERVE_DB" \
  --project-out "$DATA_DIR/served.json" \
  --addr "127.0.0.1:$PORT" > "$ARTIFACT_DIR/05-serve.log" 2>&1 &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null || true' EXIT

for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$PORT/" -o "$ARTIFACT_DIR/05-diagnostic.html"; then
    break
  fi
  sleep 0.1
done
curl -fsS "http://127.0.0.1:$PORT/api/state" -o "$ARTIFACT_DIR/05-state.json"
grep -q "Room State Matrix" "$ARTIFACT_DIR/05-diagnostic.html"
grep -q "Per-Listener Decay Curves" "$ARTIFACT_DIR/05-diagnostic.html"
HTTP_EDIT_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
  -d 'op=set_room_param&room_id=R6&rt60_ms=680&density_milli=700&gain_milli=1050' \
  "http://127.0.0.1:$PORT/api/edit")"
if [ "$HTTP_EDIT_STATUS" != "303" ]; then
  echo "diagnostic edit endpoint returned $HTTP_EDIT_STATUS, want 303" >&2
  exit 1
fi
curl -fsS "http://127.0.0.1:$PORT/api/state" -o "$ARTIFACT_DIR/05-state-after-http-edit.json"

kill "$SERVER_PID"
wait "$SERVER_PID" 2>/dev/null || true
trap - EXIT

GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/acoustic-windows-amd64.exe ./cmd/acoustic
if [ ! -s /tmp/acoustic-windows-amd64.exe ]; then
  echo "windows cross-build produced no binary" >&2
  exit 1
fi
{
  echo "GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/acoustic"
  echo "result=ok"
  ls -lh /tmp/acoustic-windows-amd64.exe
} | tee "$ARTIFACT_DIR/06-windows-build.txt"

go test -count=1 ./... | tee "$ARTIFACT_DIR/07-tests.txt"
go vet ./... | tee "$ARTIFACT_DIR/08-vet.txt"

echo "validation artifacts: $ARTIFACT_DIR"
