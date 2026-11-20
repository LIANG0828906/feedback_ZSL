#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

mkdir -p data reports

if [[ -x .venv/bin/python ]] && .venv/bin/python -c 'import numpy; raise SystemExit(0 if numpy.__version__.split(".")[0] == "2" else 1)' >/dev/null 2>&1; then
  echo "offline environment ready: $(.venv/bin/python --version), NumPy $(.venv/bin/python -c 'import numpy; print(numpy.__version__)')"
  exit 0
fi

PYTHON_BIN="${PYTHON_BIN:-python3}"
if ! "$PYTHON_BIN" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 1)'; then
  echo "Python 3.11 or newer is required" >&2
  exit 1
fi

if command -v uv >/dev/null 2>&1; then
  uv venv .venv --clear --python "$PYTHON_BIN" --offline
else
  "$PYTHON_BIN" -m venv --clear .venv
fi

PY_VERSION="$(.venv/bin/python -c 'import sys; print(f"python{sys.version_info.major}.{sys.version_info.minor}")')"
SITE_DIR=".venv/lib/$PY_VERSION/site-packages"
USER_HOME="$(getent passwd "$(id -u)" | cut -d: -f6)"
NUMPY_SEARCH_ROOTS=(
  "$USER_HOME/.feedback/runtime-home"
  "$USER_HOME/.cache/uv"
  "$USER_HOME/.local"
  "$HOME/.feedback/runtime-home"
  "$HOME/.cache/uv"
  "$HOME/.local"
  "$(dirname "$HOME")"
  "/opt"
)

NUMPY_SITE=""
for root in "${NUMPY_SEARCH_ROOTS[@]}"; do
  [[ -d "$root" ]] || continue
  candidate="$(find "$root" -path "*/$PY_VERSION/site-packages/numpy/version.py" -print -quit 2>/dev/null || true)"
  if [[ -n "$candidate" ]]; then
    NUMPY_SITE="$(dirname "$(dirname "$candidate")")"
    break
  fi
done

if [[ -z "$NUMPY_SITE" ]]; then
  echo "offline NumPy 2.x cache for $PY_VERSION was not found" >&2
  echo "Set up a local wheel cache or provide a preinstalled NumPy and rerun." >&2
  exit 1
fi

NUMPY_VERSION="$(.venv/bin/python - "$NUMPY_SITE" <<'PY'
import pathlib
import re
import sys
text = (pathlib.Path(sys.argv[1]) / "numpy" / "version.py").read_text()
match = re.search(r"version\s*=\s*['\"]([^'\"]+)", text)
print(match.group(1) if match else "")
PY
)"
if [[ -z "$NUMPY_VERSION" ]]; then
  dist_info="$(find "$NUMPY_SITE" -maxdepth 1 -type d -name 'numpy-*.dist-info' -print -quit)"
  NUMPY_VERSION="$(basename "$dist_info" | sed -E 's/^numpy-([^-]+).*/\1/')"
fi
if [[ "${NUMPY_VERSION%%.*}" != "2" ]]; then
  echo "found NumPy $NUMPY_VERSION, but 2.x is required" >&2
  exit 1
fi

cp -a "$NUMPY_SITE/numpy" "$SITE_DIR/"
cp -a "$NUMPY_SITE/numpy.libs" "$SITE_DIR/" 2>/dev/null || true
dist_info="$(find "$NUMPY_SITE" -maxdepth 1 -type d -name 'numpy-*.dist-info' -print -quit)"
if [[ -n "$dist_info" ]]; then
  cp -a "$dist_info" "$SITE_DIR/"
fi

.venv/bin/python -c 'import numpy; print(f"offline environment ready: NumPy {numpy.__version__}")'
