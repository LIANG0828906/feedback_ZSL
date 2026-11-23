#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${PORT:-18090}"
ADDR="127.0.0.1:${PORT}"
WORK="$(mktemp -d)"

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  rm -rf "${WORK}"
}
trap cleanup EXIT

cd "${ROOT}"
go build -o "${WORK}/workbench" ./cmd/workbench
"${WORK}/workbench" -addr "${ADDR}" -data "${WORK}/data" >"${WORK}/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS "http://${ADDR}/api/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done

CREATE="$(curl -fsS -X POST "http://${ADDR}/api/scenes" \
  -H 'Content-Type: application/json' \
  -d '{"preset":"scale8"}')"
SID="$(printf '%s' "${CREATE}" | sed -n 's/.*"id":"\(scene-[^"]*\)".*/\1/p' | head -1)"
if [[ -z "${SID}" ]]; then
  echo "failed to create scale8 scene" >&2
  exit 1
fi

echo "scene: ${SID}"
echo "--- adaptive decision ---"
printf '%s' "${CREATE}" | sed -n 's/.*"scale":\({[^}]*}\).*/\1/p'

curl -fsS -X POST "http://${ADDR}/api/scenes/${SID}/start" >/dev/null
sleep 0.08
curl -fsS -X POST "http://${ADDR}/api/scenes/${SID}/pause" >/dev/null
sleep 0.5
echo "--- paused status ---"
curl -fsS "http://${ADDR}/api/scenes/${SID}/status"
echo

curl -fsS -X POST "http://${ADDR}/api/scenes/${SID}/resume" >/dev/null
for _ in $(seq 1 200); do
  STATUS="$(curl -fsS "http://${ADDR}/api/scenes/${SID}/status")"
  if printf '%s' "${STATUS}" | grep -q '"resultReady":true'; then
    break
  fi
  sleep 0.25
done

echo "--- result ---"
curl -fsS "http://${ADDR}/api/scenes/${SID}/result"
echo
echo "--- three-path comparison ---"
curl -fsS -X POST "http://${ADDR}/api/scenes/${SID}/compare"
echo

EXPORT="${TMPDIR:-/tmp}/spectra-demo-export.zip"
curl -fsS -o "${EXPORT}" "http://${ADDR}/api/scenes/${SID}/export"
echo "export: ${EXPORT} ($(wc -c <"${EXPORT}") bytes)"
echo "UI: http://${ADDR}"
echo "Press Ctrl+C to stop the demo server."
wait "${SERVER_PID}"
