#!/usr/bin/env bash

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -x "$ROOT/.venv/bin/python" ]; then
  LA_PYTHON="$ROOT/.venv/bin/python"
else
  LA_PYTHON="python3"
fi

PY_VERSION="$("$LA_PYTHON" -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"
TK_PYTHON_DIR="$ROOT/.vendor/tk-root/usr/lib/python${PY_VERSION}"
TK_DYNLOAD_DIR="$TK_PYTHON_DIR/lib-dynload"
TK_LIB_DIR="$ROOT/.vendor/tk-root/usr/lib/x86_64-linux-gnu"

export PYTHONPATH="$ROOT${PYTHONPATH:+:$PYTHONPATH}"
if [ -d "$TK_PYTHON_DIR/tkinter" ]; then
  export PYTHONPATH="$TK_PYTHON_DIR:$TK_DYNLOAD_DIR:$PYTHONPATH"
  export LD_LIBRARY_PATH="$TK_LIB_DIR${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  export TCL_LIBRARY="$ROOT/.vendor/tk-root/usr/share/tcltk/tcl8.6"
  export TK_LIBRARY="$ROOT/.vendor/tk-root/usr/share/tcltk/tk8.6"
fi

export LA_PYTHON
export LA_ROOT="$ROOT"

