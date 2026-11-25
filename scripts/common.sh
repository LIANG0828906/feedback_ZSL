#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# The workspace vendors modernc.org/sqlite. These settings additionally make
# fresh module resolution use the prewarmed local cache and never the network.
export GOFLAGS="${GOFLAGS:-}"
export GOPROXY=off
if [ -d /home/liang/go/pkg/mod ]; then
  export GOPATH=/home/liang/go
  export GOMODCACHE=/home/liang/go/pkg/mod
fi
