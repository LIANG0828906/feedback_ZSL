#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from pcwb.demo import run_demo


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--out", default=str(ROOT / "artifacts" / "demo"), help="output directory"
    )
    parser.add_argument("--seed", type=int, default=20261122)
    parser.add_argument("--threshold-ms", type=float, default=50.0)
    args = parser.parse_args()
    summary = run_demo(
        args.out,
        seed=args.seed,
        progress_threshold_ms=args.threshold_ms,
    )
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
