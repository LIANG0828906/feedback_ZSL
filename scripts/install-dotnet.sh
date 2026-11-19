#!/usr/bin/env bash
# User-local .NET 8 SDK install (no root required). Reuses the SDK when it is
# already present in DOTNET_HOME.
set -euo pipefail

DOTNET_HOME="${DOTNET_HOME:-$HOME/.dotnet8}"
CHANNEL="${DOTNET_CHANNEL:-8.0}"

if [ -x "$DOTNET_HOME/dotnet" ] && "$DOTNET_HOME/dotnet" --list-sdks | grep -q '^8\.'; then
  echo "[dotnet] .NET 8 SDK already installed at $DOTNET_HOME"
  exit 0
fi

SCRIPT_PATH="$(mktemp -t dotnet-install.XXXXXX.sh)"
curl -fsSL https://dot.net/v1/dotnet-install.sh -o "$SCRIPT_PATH"
chmod +x "$SCRIPT_PATH"
"$SCRIPT_PATH" --channel "$CHANNEL" --install-dir "$DOTNET_HOME"
rm -f "$SCRIPT_PATH"

"$DOTNET_HOME/dotnet" --info | head -12
