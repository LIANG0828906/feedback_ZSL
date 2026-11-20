#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DLL="$ROOT/src/TacticalField.WinForms/bin/Release/net8.0-windows/TacticalField.WinForms.dll"
WINDOWS_DOTNET="${WINDOWS_DOTNET:-/mnt/c/Users/LIANG/AppData/Local/Microsoft/dotnet8-desktop/dotnet.exe}"

if [[ ! -x "$WINDOWS_DOTNET" ]]; then
  echo "ERROR Windows dotnet host not found: $WINDOWS_DOTNET" >&2
  exit 1
fi

if [[ ! -f "$DLL" ]]; then
  echo "ERROR Build src/TacticalField.WinForms first." >&2
  exit 1
fi

WINDOWS_DLL="$(wslpath -w "$DLL")"
echo "WindowsDesktop runtime:"
"$WINDOWS_DOTNET" --list-runtimes | grep 'Microsoft.WindowsDesktop.App 8\.'
echo "Launching WinForms smoke mode: $WINDOWS_DLL"
timeout 30 "$WINDOWS_DOTNET" "$WINDOWS_DLL" --smoke
echo "GUI SMOKE PASS"
