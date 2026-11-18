#!/usr/bin/env bash
# End-to-end deterministic demo on the small scene (fast, CPU only).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WORK="output/demo"
mkdir -p "$WORK"

python3 -m brocade_embroidery.cli generate-scene \
    --scene "$WORK/scene.json" --small
python3 -m brocade_embroidery.cli validate-scene \
    --scene "$WORK/scene.json" --report "$WORK/validation.json"
python3 -m brocade_embroidery.cli render \
    --scene "$WORK/scene.json" --output "$WORK/frames" --frame 0
python3 -m brocade_embroidery.cli acceptance \
    --output "$WORK/acceptance" --small

echo "Demo outputs are under $WORK"
