# Shared helpers for the three evaluation paths and the independent verifier.
# All artifacts stay under the workspace; no containers or external services.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA_DIR="${ROOT}/data"
STORE_ROOT="${ROOT}/store"
QUALITY="${QUALITY:-70}"
WORKERS="${WORKERS:-2}"
WINDOW="${WINDOW:-7}"
STRIDE="${STRIDE:-2}"
HALO="${HALO:-8}"

go_bin() {
  if command -v go >/dev/null 2>&1; then
    echo "go"
  else
    echo "${HOME}/.local/go/bin/go"
  fi
}

GO="$(go_bin)"

build_all() {
  (cd "${ROOT}" && "${GO}" build -o bin/generator ./cmd/generator &&
    "${GO}" build -o bin/pipeline ./cmd/pipeline &&
    "${GO}" build -o bin/compare ./cmd/compare &&
    "${GO}" build -o bin/verify ./cmd/verify)
}

generate_samples() {
  if [ ! -f "${DATA_DIR}/manifest.json" ]; then
    "${ROOT}/bin/generator" -data "${DATA_DIR}"
  else
    echo "samples already present at ${DATA_DIR}"
  fi
}
