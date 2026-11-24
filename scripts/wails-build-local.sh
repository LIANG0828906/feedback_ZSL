#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SYSROOT="${SYSROOT:-$ROOT/.sysroot/root}"

if [[ -d "$SYSROOT/usr/lib/x86_64-linux-gnu" ]]; then
  export PATH="$SYSROOT/usr/bin:$PATH"
  export LD_LIBRARY_PATH="$SYSROOT/usr/lib/x86_64-linux-gnu:$SYSROOT/lib/x86_64-linux-gnu:${LD_LIBRARY_PATH:-}"
  export PKG_CONFIG_SYSROOT_DIR="$SYSROOT"
  export PKG_CONFIG_LIBDIR="$SYSROOT/usr/lib/x86_64-linux-gnu/pkgconfig:$SYSROOT/usr/share/pkgconfig"
  export CGO_LDFLAGS="-L$SYSROOT/usr/lib/x86_64-linux-gnu -Wl,-rpath-link,$SYSROOT/usr/lib/x86_64-linux-gnu ${CGO_LDFLAGS:-}"
  # Wails removes CC while building its temporary binding generator. Disabling
  # CGO for that process avoids linking against the host's missing libc-dev.
  # The final application build explicitly re-enables CGO.
  export CGO_ENABLED=0
fi

cd "$ROOT"
exec "${WAILS:-wails}" build -tags webkit2_41 -clean "$@"
