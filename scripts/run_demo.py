#!/usr/bin/env python3
"""Run deterministic render trajectories or fault-injection paths."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from volrender.demo import (
    render_budget_trajectory,
    run_all_injections,
    run_one_injection,
)
from volrender.injection import INJECTIONS


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--volume",
        type=Path,
        default=ROOT / "artifacts" / "samples" / "small" / "volume.svol",
    )
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "demo_small",
    )
    parser.add_argument("--tier", default="small")
    parser.add_argument("--seed", type=int, default=20261121)
    parser.add_argument("--inject", choices=INJECTIONS, default=None)
    parser.add_argument(
        "--all-injections",
        action="store_true",
        help="run all five injections beneath --output",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.all_injections:
        rows = run_all_injections(volume_path=args.volume, root_output=args.output)
        print(json.dumps(rows, sort_keys=True, indent=2, default=str))
        return 0
    if args.inject:
        row = run_one_injection(
            injection=args.inject,
            volume_path=args.volume,
            output_dir=args.output,
        )
        print(json.dumps(row, sort_keys=True, indent=2, default=str))
        return 0
    row = render_budget_trajectory(
        volume_path=args.volume,
        output_dir=args.output,
        tier=args.tier,
        seed=args.seed,
    )
    print(
        json.dumps(
            {
                "output": str(args.output),
                "repeat_identical": row["repeat_identical"],
                "snapshot": str(row["snapshot"]),
                "stages": row["results"][-1].degradation_stages,
                "tier": args.tier,
            },
            sort_keys=True,
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

