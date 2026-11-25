#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
mkdir -p artifacts/validation artifacts/bin
LOG="$ROOT/artifacts/validation/acceptance-run.log"

{
  echo "== MeshDoctor acceptance =="
  date --iso-8601=seconds
  echo "--- go env ---"
  go version
  go env GOVERSION GOOS GOARCH CGO_ENABLED GOPATH GOMODCACHE CC
  echo "--- compiler ---"
  gcc --version | head -1 || true

  echo "== [1/7] go test ./... (includes real kill recovery test) =="
  go test ./...

  echo "== [2/7] deterministic sample generation =="
  go run ./cmd/meshdoctor generate --root "$ROOT" --seed 424242

  echo "== [3/7] three-path byte comparison and error contracts =="
  go run ./cmd/meshdoctor verify --root "$ROOT" --parallel=true --workers=2 \
    --out artifacts/validation/acceptance-result.json

  echo "== [4/7] build CLI =="
  go build -o artifacts/bin/meshdoctor ./cmd/meshdoctor
  BIN="$ROOT/artifacts/bin/meshdoctor"
  WS="$(mktemp -d)"
  "$BIN" generate --root "$WS" --seed 424242 >/dev/null

  echo "== [5/7] cancel/pause after two committed stages =="
  "$BIN" run --root "$WS" --sample tetra --db checkpoints/cancel.db --stop-after 2 \
    | tee "$WS/cancel-first.json"
  "$BIN" run --root "$WS" --sample tetra --db checkpoints/cancel.db \
    | tee "$WS/cancel-resume.json"
  python3 - "$WS/cancel-resume.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
assert d["committed"] == ["diagnostic","weld","fill","orient","smooth","stats"], d
assert d["cache_hits"] == 2, d
print("cancel/resume boundary OK: 2 persisted hits, six final stages")
PY

  echo "== [6/7] SIGKILL before smooth, restart from four durable stages =="
  READY="$WS/kill-ready"
  "$BIN" run --root "$WS" --sample cube --db checkpoints/kill.db \
    --hang-before smooth --ready-file "$READY" &
  HANG_SHELL_PID=$!
  for _ in $(seq 1 100); do
    [ -f "$READY" ] && break
    sleep 0.05
  done
  [ -f "$READY" ] || { echo "ready file missing"; exit 1; }
  VICTIM_PID="$(cat "$READY")"
  echo "sending SIGKILL to pid=$VICTIM_PID (shell job=$HANG_SHELL_PID)"
  kill -9 "$VICTIM_PID"
  wait "$HANG_SHELL_PID" || true
  "$BIN" run --root "$WS" --sample cube --db checkpoints/kill.db \
    | tee "$WS/kill-resume.json"
  python3 - "$WS/kill-resume.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
assert d["committed"] == ["diagnostic","weld","fill","orient","smooth","stats"], d
assert d["cache_hits"] == 4, d
print("SIGKILL recovery OK: stale running normalized, 4 persisted hits, 2 recomputed stages")
PY

  echo "== [7/7] loopback HTTP diagnostic page =="
  FREE_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
  ADDR="127.0.0.1:${FREE_PORT}"
  echo "using temporary loopback address http://${ADDR}/"
  "$BIN" serve --root "$ROOT" --addr "$ADDR" >"$WS/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 100); do
    if curl -fsS "http://$ADDR/api/state" >/dev/null 2>&1; then break; fi
    sleep 0.1
  done
  curl -fsS "http://$ADDR/" -o "$WS/page.html"
  curl -fsS "http://$ADDR/api/state" -o "$WS/state.json"
  grep -q "MeshDoctor" "$WS/page.html"
  python3 - "$WS/state.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
assert d["ok"] is True
assert d["report"]["ok"] is True
assert len(d["report"]["samples"]) == 3
print("HTTP state OK: 3 samples and successful report on 127.0.0.1")
PY
  kill "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true

  echo "ACCEPTANCE_OK"
  echo "workspace-demo=$WS"
} 2>&1 | tee "$LOG"
