#!/usr/bin/env bash
set -euo pipefail

# The Wails app embeds frontend/dist while the zero-dependency browser
# workbench embeds cmd/workbench/webroot. Keep both identical without a Node
# toolchain.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cp "$ROOT/frontend/dist/index.html" "$ROOT/cmd/workbench/webroot/index.html"
cp "$ROOT/frontend/dist/app.css" "$ROOT/cmd/workbench/webroot/app.css"
cp "$ROOT/frontend/dist/app.js" "$ROOT/cmd/workbench/webroot/app.js"
echo "frontend synced to cmd/workbench/webroot"
