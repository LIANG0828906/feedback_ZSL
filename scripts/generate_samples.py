#!/usr/bin/env python3
"""Generate both local SVOL tiers and their ample-budget baseline hashes."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from volrender.demo import generate_baseline_artifacts
from volrender.samples import DEFAULT_SEED, sample_specs, write_volume


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--root",
        type=Path,
        default=ROOT / "artifacts" / "samples",
        help="sample output directory",
    )
    parser.add_argument(
        "--tiers",
        default="small,large",
        help="comma separated tiers: small,large",
    )
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    parser.add_argument(
        "--volumes-only",
        action="store_true",
        help="skip baseline rendering (baseline hashes are left untouched)",
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    selected = [item.strip() for item in args.tiers.split(",") if item.strip()]
    specs = sample_specs()
    summary = []
    for tier in selected:
        if tier not in specs:
            raise SystemExit(f"unknown tier: {tier}")
        spec = specs[tier]
        output = args.root / tier
        output.mkdir(parents=True, exist_ok=True)
        volume_path = output / "volume.svol"
        volume_meta = write_volume(
            volume_path,
            dims=spec["dims"],
            spacing=spec["spacing"],
            seed=args.seed,
        )
        row = {"tier": tier, "volume": volume_meta}
        if not args.volumes_only:
            baseline = generate_baseline_artifacts(
                volume_path=volume_path,
                output_dir=output,
                tier=tier,
                seed=args.seed,
            )
            row["baseline"] = baseline
        summary.append(row)
    print(json.dumps(summary, sort_keys=True, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

