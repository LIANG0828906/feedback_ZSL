#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! command -v uv >/dev/null 2>&1; then
  echo "uv is required (https://docs.astral.sh/uv/)" >&2
  exit 1
fi

uv venv --python 3.14 .venv
uv pip install --python .venv/bin/python -r requirements.txt

PY_VERSION="$(.venv/bin/python -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"
if .venv/bin/python -c 'import tkinter' >/dev/null 2>&1; then
  echo "System Tkinter is available."
  exit 0
fi

echo "Installing a user-local Tk runtime from Ubuntu packages..."
mkdir -p .vendor/debs .vendor/tk-root
pushd .vendor/debs >/dev/null
apt-get download \
  "python${PY_VERSION}-tk" \
  libtk8.6 \
  libtcl8.6 \
  tk8.6 \
  libxft2
popd >/dev/null

for package in .vendor/debs/*.deb; do
  dpkg-deb -x "$package" .vendor/tk-root
done

source scripts/env.sh
"$LA_PYTHON" -c 'import tkinter; print("Tk", tkinter.TkVersion)'

