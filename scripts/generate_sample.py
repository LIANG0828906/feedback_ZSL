#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from terrain_pipeline.generator import generate_project


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate a deterministic terrain project")
    parser.add_argument("--out", default="artifacts/sample", help="output project directory")
    parser.add_argument("--seed", type=int, default=20261123)
    parser.add_argument("--grid", type=int, nargs=2, default=(4, 3), metavar=("X", "Y"))
    parser.add_argument("--tile-size", type=int, default=16)
    parser.add_argument("--subdivisions", type=int, default=4)
    parser.add_argument("--points-per-tile", type=int, default=96)
    args = parser.parse_args()
    manifest = generate_project(
        args.out,
        seed=args.seed,
        grid_tiles=(args.grid[0], args.grid[1]),
        tile_size=args.tile_size,
        subdivisions=args.subdivisions,
        points_per_tile=args.points_per_tile,
    )
    print(
        json.dumps(
            {
                "ok": True,
                "project": str(Path(args.out).resolve()),
                "tiles": len(manifest["tiles"]),
            }
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
