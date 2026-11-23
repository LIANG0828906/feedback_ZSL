#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
  npm --prefix frontend run build
else
  echo "Node.js/npm not found; using the committed static frontend/dist assets."
fi

if pkg-config --exists gtk+-3.0 webkit2gtk-4.1 2>/dev/null && command -v wails >/dev/null 2>&1; then
  exec wails build -tags "wails,webkit2_41" -s
fi

SYSROOT="${FORENSICLAB_SYSROOT:-$ROOT/.tools/sysroot}"
if [ -d "$SYSROOT" ] && command -v gcc >/dev/null 2>&1; then
  export PATH="$SYSROOT/usr/bin:${PATH}"
  export PKG_CONFIG_PATH="$SYSROOT/usr/lib/x86_64-linux-gnu/pkgconfig:$SYSROOT/usr/share/pkgconfig"
  export PKG_CONFIG_SYSROOT_DIR="$SYSROOT"
  export LD_LIBRARY_PATH="$SYSROOT/usr/lib/x86_64-linux-gnu:$SYSROOT/usr/lib64:$SYSROOT/lib64"
  export CC="gcc --sysroot=$SYSROOT"
  export CGO_LDFLAGS="-L$SYSROOT/usr/lib/x86_64-linux-gnu -Wl,-rpath,$SYSROOT/usr/lib/x86_64-linux-gnu"
  mkdir -p build/bin
  go build -buildvcs=false \
    -tags "desktop,wails,wv2runtime.download,production,webkit2_41" \
    -ldflags "-w -s" \
    -o build/bin/forensiclab .
  echo "Built build/bin/forensiclab using private sysroot: $SYSROOT"
  exit 0
fi

cat >&2 <<'EOF'
Desktop system dependencies are missing.

Install:
  sudo apt-get install -y libgtk-3-dev libwebkit2gtk-4.1-dev pkg-config
Then run:
  wails build -tags "wails,webkit2_41" -s

Alternatively set FORENSICLAB_SYSROOT to a prefix containing GTK/WebKit
headers, pkg-config files and shared libraries.
EOF
exit 1
