#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from iovflow.generator import GeneratorConfig, generate


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate the fixed-seed 96-batch JSONL workload")
    parser.add_argument("--out", default="data")
    parser.add_argument("--seed", type=int, default=3202026)
    parser.add_argument("--rows", type=int, default=80000)
    parser.add_argument("--batches", type=int, default=96)
    parser.add_argument("--vehicles", type=int, default=1200)
    parser.add_argument("--trips-per-vehicle", type=int, default=4)
    parser.add_argument("--duplicate-ratio", type=float, default=0.04)
    parser.add_argument("--delayed-ratio", type=float, default=0.03)
    parser.add_argument("--hot-vehicle-share", type=float, default=0.12)
    args = parser.parse_args()
    manifest = generate(
        GeneratorConfig(
            out_dir=args.out,
            seed=args.seed,
            rows=args.rows,
            batches=args.batches,
            vehicles=args.vehicles,
            trips_per_vehicle=args.trips_per_vehicle,
            duplicate_ratio=args.duplicate_ratio,
            delayed_ratio=args.delayed_ratio,
            hot_vehicle_share=args.hot_vehicle_share,
        )
    )
    print(json.dumps(manifest["stats"], indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

