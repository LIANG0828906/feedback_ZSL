#!/usr/bin/env bash
set -euo pipefail

# Reproducible Wails build using the in-repo user-space sysroot and gcc.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=env-local.sh
source "$ROOT/scripts/env-local.sh"
export PATH="$(go env GOPATH)/bin:${PATH}"

cd "$ROOT"
exec wails build -tags webkit2_41 -clean "$@"
