# Local non-container toolchain: user-space sysroot (GTK3 + WebKitGTK 4.1) and gcc-15.
# Source this file before running wails build on a host without system GTK dev packages.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SYSROOT="$ROOT/.sysroot/root"
export PATH="$SYSROOT/usr/bin:$ROOT/.sysroot/bin:${PATH:-}"
export LD_LIBRARY_PATH="$SYSROOT/usr/lib/x86_64-linux-gnu:$SYSROOT/lib64:${LD_LIBRARY_PATH:-}"
export PKG_CONFIG_SYSROOT_DIR="$SYSROOT"
export PKG_CONFIG_LIBDIR="$SYSROOT/usr/lib/x86_64-linux-gnu/pkgconfig:$SYSROOT/usr/share/pkgconfig"
export CC="$ROOT/.sysroot/bin/gcc-sysroot"
export CGO_LDFLAGS="-L$SYSROOT/usr/lib/x86_64-linux-gnu -Wl,-rpath-link,$SYSROOT/usr/lib/x86_64-linux-gnu ${CGO_LDFLAGS:-}"
