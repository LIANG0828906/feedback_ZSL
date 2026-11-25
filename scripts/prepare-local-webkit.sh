#!/usr/bin/env bash
set -euo pipefail

# On hosts without a system WebKitGTK runtime, build a local prefix under
# /tmp and binary-patch the shared library so the hard-coded Debian process
# path points at it. The replacement path is byte-length identical, so no ELF
# offsets move. Nothing under /usr is modified and no container is used.

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SYSROOT="${SYSROOT:-$ROOT/.sysroot/root}"
SRC="$SYSROOT/usr/lib/x86_64-linux-gnu"
SYS_PREFIX="/usr/lib/x86_64-linux-gnu/webkit2gtk-4.1"
LOCAL_PREFIX="/tmp/strokeforge-webkit-runtime-local-01"
LIB_DIR="/tmp/strokeforge-webkit-lib"

if [[ ! -d "$SRC/webkit2gtk-4.1" ]]; then
  echo "WebKitGTK runtime not found under $SRC" >&2
  exit 1
fi
if [[ "$(printf %s "$SYS_PREFIX" | wc -c)" != "$(printf %s "$LOCAL_PREFIX" | wc -c)" ]]; then
  echo "local WebKit prefix must have the same byte length as $SYS_PREFIX" >&2
  exit 1
fi

if [[ ! -x "$LOCAL_PREFIX/WebKitNetworkProcess" || ! -f "$LIB_DIR/libwebkit2gtk-4.1.so.0" ]]; then
  mkdir -p "$LOCAL_PREFIX" "$LIB_DIR"
  cp -a "$SRC/webkit2gtk-4.1/." "$LOCAL_PREFIX/"
  cp "$SRC/libwebkit2gtk-4.1.so.0" "$LIB_DIR/libwebkit2gtk-4.1.so.0"
  python3 - "$SYS_PREFIX" "$LOCAL_PREFIX" "$LIB_DIR/libwebkit2gtk-4.1.so.0" <<'PY'
import pathlib
import sys

old = sys.argv[1].encode()
new = sys.argv[2].encode()
target = pathlib.Path(sys.argv[3])
raw = target.read_bytes()
assert old in raw, "hard-coded prefix not found in library"
target.write_bytes(raw.replace(old, new))
PY
fi

echo "$LIB_DIR"
