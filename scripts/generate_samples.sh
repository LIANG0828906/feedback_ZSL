#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
go run ./cmd/uvtool gen -out samples -seed 20261125
go run ./cmd/uvtool inspect -sample samples/sample_01 -truth >/dev/null
go run ./cmd/uvtool inspect -sample samples/sample_02 -truth >/dev/null
echo "samples regenerated and truth-validated"
