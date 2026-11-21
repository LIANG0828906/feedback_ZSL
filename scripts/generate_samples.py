#!/usr/bin/env python3
"""Generate deterministic local samples and authoritative baselines."""

from __future__ import annotations

import argparse
import json

from cover_replay.generator import PROFILE_SPECS, generate_samples


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="artifacts/samples")
    parser.add_argument("--seed", type=int, default=20261122)
    parser.add_argument(
        "--profiles",
        default=",".join(PROFILE_SPECS),
        help="comma-separated profiles: small,medium,stress,conflict",
    )
    args = parser.parse_args()
    profiles = tuple(
        item.strip() for item in args.profiles.split(",") if item.strip()
    )
    manifest = generate_samples(args.out, seed=args.seed, profiles=profiles)
    print(json.dumps(manifest, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
