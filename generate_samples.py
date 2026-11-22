#!/usr/bin/env python3
"""Convenience entry point for deterministic sample generation."""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from spectraforge.sample_generator import generate_all


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic SpectraForge samples")
    parser.add_argument("--out", default="samples", help="Output samples directory")
    parser.add_argument("--seed", type=int, default=424242)
    parser.add_argument("--scenes", type=int, default=3)
    parser.add_argument("--width", type=int, default=128)
    parser.add_argument("--height", type=int, default=96)
    parser.add_argument("--bands", type=int, default=8)
    parser.add_argument("--exposures", type=int, default=6)
    parser.add_argument("--no-faults", action="store_true")
    args = parser.parse_args()
    catalog = generate_all(
        Path(args.out),
        seed=args.seed,
        scene_count=args.scenes,
        width=args.width,
        height=args.height,
        band_count=args.bands,
        exposure_count=args.exposures,
        with_faults=not args.no_faults,
    )
    print(json.dumps(catalog, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
