#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PYTHON="${PYTHON:-$ROOT/.venv/bin/python}"
if [[ -d "$ROOT/.deps/root/usr/lib/python3.14" ]]; then
  export PYTHONPATH="$ROOT/.deps/root/usr/lib/python3.14:$ROOT/.deps/root/usr/lib/python3.14/lib-dynload${PYTHONPATH:+:$PYTHONPATH}"
  export LD_LIBRARY_PATH="$ROOT/.deps/root/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  export TCL_LIBRARY="$ROOT/.deps/root/usr/share/tcltk/tcl8.6"
  export TK_LIBRARY="$ROOT/.deps/root/usr/share/tcltk/tk8.6"
fi

exec "$PYTHON" -m unittest discover -s tests -v

