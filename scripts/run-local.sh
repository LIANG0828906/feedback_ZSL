#!/usr/bin/env bash
set -euo pipefail

# Launch the Wails desktop binary against the user-space sysroot. On hosts
# lacking a system WebKitGTK process directory, a length-preserving binary
# patch redirects child processes to a /tmp runtime prefix.

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SYSROOT="${SYSROOT:-$ROOT/.sysroot/root}"
BIN="${1:-$ROOT/build/bin/strokeforge}"

export LD_LIBRARY_PATH="$SYSROOT/usr/lib/x86_64-linux-gnu:$SYSROOT/lib/x86_64-linux-gnu:${LD_LIBRARY_PATH:-}"
export GIO_EXTRA_MODULES="$SYSROOT/usr/lib/x86_64-linux-gnu/gio/modules"

if [[ ! -d /usr/lib/x86_64-linux-gnu/webkit2gtk-4.1 ]]; then
  local_lib="$("$ROOT/scripts/prepare-local-webkit.sh")"
  export LD_LIBRARY_PATH="$local_lib:$LD_LIBRARY_PATH"
  export WEBKIT_INJECTED_BUNDLE_PATH="/tmp/strokeforge-webkit-runtime-local-01/injected-bundle/libwebkit2gtkinjectedbundle.so"
fi

exec "$BIN"
