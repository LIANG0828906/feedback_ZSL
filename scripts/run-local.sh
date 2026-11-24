#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SYSROOT="${SYSROOT:-$ROOT/.sysroot/root}"
BIN="${1:-$ROOT/build/bin/morph-edge-studio}"

if [[ -d "$SYSROOT/usr/lib/x86_64-linux-gnu" ]]; then
  export LD_LIBRARY_PATH="$SYSROOT/usr/lib/x86_64-linux-gnu:$SYSROOT/lib/x86_64-linux-gnu:${LD_LIBRARY_PATH:-}"
  if [[ ! -d /usr/lib/x86_64-linux-gnu/webkit2gtk-4.1 ]]; then
    local_lib="$("$ROOT/scripts/prepare-local-webkit.sh")"
    export LD_LIBRARY_PATH="$local_lib:$LD_LIBRARY_PATH"
  fi
fi
exec "$BIN"
