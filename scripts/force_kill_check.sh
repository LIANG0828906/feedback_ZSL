#!/usr/bin/env bash
# Verify that SIGKILL followed by a snapshot-only restart reproduces the
# uninterrupted render byte-for-byte.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WORK="output/force_kill"
SCENE="$WORK/scene.json"
KILLED_DIR="$WORK/killed"
REFERENCE_DIR="$WORK/reference"

mkdir -p "$WORK"
python3 -m brocade_embroidery.cli generate-scene --scene "$SCENE" --small

python3 -m brocade_embroidery.cli render --scene "$SCENE" \
    --output "$REFERENCE_DIR" --frame 0

python3 -m brocade_embroidery.cli render --scene "$SCENE" \
    --output "$KILLED_DIR" --frame 0 --max-tiles 2 &
RENDER_PID=$!
for _ in $(seq 1 50); do
    if [ -f "$KILLED_DIR/frame_00/snapshot/snapshot.json" ]; then
        break
    fi
    sleep 0.05
done
sleep 0.15
kill -KILL "$RENDER_PID" 2>/dev/null || true
wait "$RENDER_PID" || true

echo "snapshot after SIGKILL:"
ls "$KILLED_DIR/frame_00/snapshot"

python3 -m brocade_embroidery.cli render --scene "$SCENE" \
    --output "$KILLED_DIR" --frame 0

python3 -m brocade_embroidery.cli compare-images \
    --a "$REFERENCE_DIR/frame_00/render.rgba.png" \
    --b "$KILLED_DIR/frame_00/render.rgba.png" \
    --report "$WORK/rgba_diff.json"

cmp "$REFERENCE_DIR/frame_00/ownership.u16" \
    "$KILLED_DIR/frame_00/ownership.u16"

python3 - "$REFERENCE_DIR" "$KILLED_DIR" <<'PY'
import json
import sys

reference = json.load(open(f"{sys.argv[1]}/frame_00/report.json"))
resumed = json.load(open(f"{sys.argv[2]}/frame_00/report.json"))
assert reference["landing_points"] == resumed["landing_points"]
print("landing coordinates identical:",
      len(reference["landing_points"]), "points")
PY

echo "FORCE-KILL RESUME CHECK PASSED"
