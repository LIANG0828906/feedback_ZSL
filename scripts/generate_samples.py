#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from pcwb.samplegen import generate_samples


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--out", default=str(ROOT / "artifacts" / "samples"), help="output directory"
    )
    parser.add_argument("--seed", type=int, default=20261122)
    args = parser.parse_args()
    manifest = generate_samples(args.out, seed=args.seed)
    print(json.dumps(manifest, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
