#!/usr/bin/env bash
set -euo pipefail
export CGO_ENABLED=0

go build -o bin/maskpipe ./cmd/maskpipe
./bin/maskpipe generate -root samples/local-seed -seed 424242
./bin/maskpipe validate -sample samples/local-seed -scene blocks
./bin/maskpipe run -scene blocks -params configs/params.example.json -db reports/example-blocks.db -out reports/example-blocks.json -mode full -preview
./bin/maskpipe threeway -scene blocks -out reports/threeway-blocks
./bin/maskpipe verify -db reports/threeway-blocks/dbs/full.db -manifest samples/local-seed/manifest.json -scene blocks -out reports/threeway-blocks/verify-cli.json

for scene in low-contrast embedded-noise; do
  ./bin/maskpipe threeway -scene "$scene" -out "reports/threeway-$scene"
done
