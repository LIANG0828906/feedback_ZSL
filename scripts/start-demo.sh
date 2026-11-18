#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${PORT:-18473}"
MERGE_WINDOW="${MERGE_WINDOW:-500ms}"
GO_BIN="${GO:-go}"
BASE_URL="http://127.0.0.1:${PORT}"
BIN_DIR="${ROOT}/bin"
TMP_DIR="${ROOT}/tmp"
EXE="${BIN_DIR}/configcenter"
SERVER_PID=""
CLIENT_PIDS=()

cleanup() {
  set +e
  for pid in "${CLIENT_PIDS[@]}"; do
    kill "${pid}" 2>/dev/null || true
  done
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" 2>/dev/null || true
  fi
  wait 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "${BIN_DIR}" "${TMP_DIR}"
rm -f "${TMP_DIR}/demo.db" "${TMP_DIR}/demo.db-wal" "${TMP_DIR}/demo.db-shm" \
  "${TMP_DIR}/server.out.log" "${TMP_DIR}/server.err.log" \
  "${TMP_DIR}/client-1.out.log" "${TMP_DIR}/client-1.err.log" \
  "${TMP_DIR}/client-2.out.log" "${TMP_DIR}/client-2.err.log" \
  "${TMP_DIR}/client-3.out.log" "${TMP_DIR}/client-3.err.log"

cd "${ROOT}"
"${GO_BIN}" version
CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=900MiB \
  "${GO_BIN}" build -trimpath -o "${EXE}" ./cmd/configcenter

GOMAXPROCS=2 GOMEMLIMIT=900MiB "${EXE}" server \
  --addr "127.0.0.1:${PORT}" \
  --db "${TMP_DIR}/demo.db" \
  --merge-window "${MERGE_WINDOW}" \
  --merge-max-wait 2s \
  >"${TMP_DIR}/server.out.log" 2>"${TMP_DIR}/server.err.log" &
SERVER_PID=$!

healthy=0
for _ in $(seq 1 100); do
  if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
    echo "server exited before becoming healthy" >&2
    cat "${TMP_DIR}/server.out.log" "${TMP_DIR}/server.err.log" >&2
    exit 1
  fi
  if curl -fsS --max-time 1 "${BASE_URL}/healthz" >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 0.2
done
if [[ "${healthy}" -ne 1 ]]; then
  echo "server did not become healthy within 20 seconds" >&2
  exit 1
fi

for i in 1 2 3; do
  GOMAXPROCS=1 GOMEMLIMIT=256MiB "${EXE}" client \
    --base-url "${BASE_URL}" \
    --namespace orders \
    --client-id "client-${i}" \
    --poll-timeout 10s \
    --apply-delay 20ms \
    >"${TMP_DIR}/client-${i}.out.log" 2>"${TMP_DIR}/client-${i}.err.log" &
  CLIENT_PIDS+=("$!")
done

echo "Started server PID ${SERVER_PID} and 3 client PIDs: ${CLIENT_PIDS[*]}"
sleep 1.2

set +e
"${EXE}" stress \
  --base-url "${BASE_URL}" \
  --namespace orders \
  --clients 3 \
  --merge-window "${MERGE_WINDOW}"
STRESS_CODE=$?
set -e

echo
echo "--- JSON status ---"
curl -fsS "${BASE_URL}/v1/status"
echo
echo
echo "--- Prometheus sample ---"
curl -fsS "${BASE_URL}/metrics" | grep -E '^config_(long_poll|merged|client_)' || true
echo
echo "--- Client apply logs ---"
for i in 1 2 3; do
  echo "client-${i}:"
  grep -E 'READY|APPLIED|STALE|ERROR' "${TMP_DIR}/client-${i}.out.log" || true
  cat "${TMP_DIR}/client-${i}.err.log" || true
done
echo
echo "--- Server log ---"
cat "${TMP_DIR}/server.out.log" "${TMP_DIR}/server.err.log"

if [[ "${STRESS_CODE}" -ne 0 ]]; then
  echo "stress verification failed with exit code ${STRESS_CODE}" >&2
  exit "${STRESS_CODE}"
fi
