#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

PYTHON="${PYTHON:-$ROOT/.venv/bin/python}"
if [[ ! -x "$PYTHON" ]]; then
  echo "Python environment not found. Run: uv venv .venv && uv pip install -r requirements.txt" >&2
  exit 2
fi

# The validator image ships Tk as an extracted local dependency. A normal
# Ubuntu desktop installation with python3-tk needs none of these overrides.
if [[ -d "$ROOT/.deps/root/usr/lib/python3.14" ]]; then
  export PYTHONPATH="$ROOT/.deps/root/usr/lib/python3.14:$ROOT/.deps/root/usr/lib/python3.14/lib-dynload${PYTHONPATH:+:$PYTHONPATH}"
  export LD_LIBRARY_PATH="$ROOT/.deps/root/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  export TCL_LIBRARY="$ROOT/.deps/root/usr/share/tcltk/tcl8.6"
  export TK_LIBRARY="$ROOT/.deps/root/usr/share/tcltk/tk8.6"
fi

exec "$PYTHON" -m haptic.gui "$@"

