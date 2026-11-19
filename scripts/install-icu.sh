#!/usr/bin/env bash
# User-local ICU install for hosts without libicu and without root.
# Microsoft.Data.SqlClient requires real globalization support, so .NET's
# invariant mode cannot be used by the gateway.
set -euo pipefail

ICU_DIR="${ICU_DIR:-$HOME/.local/icu}"

if [ -f "$ICU_DIR/extracted/usr/lib/x86_64-linux-gnu/libicuuc.so.78" ]; then
  echo "[icu] already extracted under $ICU_DIR"
  exit 0
fi

mkdir -p "$ICU_DIR"
cd "$ICU_DIR"
apt-get download libicu78
DEB_FILE="$(ls libicu78_*.deb | head -1)"
dpkg-deb -x "$DEB_FILE" extracted
rm -f "$DEB_FILE"
echo "[icu] ready at $ICU_DIR/extracted"
