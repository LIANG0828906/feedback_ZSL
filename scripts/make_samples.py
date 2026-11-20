#!/usr/bin/env python3
"""Regenerate deterministic WAV samples, truth, and workbench config."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from loudness_workbench.generator import generate_samples  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--sample-dir", default="samples")
    parser.add_argument("--config", default="samples/config.json")
    parser.add_argument("--seed", type=int, default=20261121)
    args = parser.parse_args()
    result = generate_samples(
        args.sample_dir,
        seed=args.seed,
        config_path=args.config,
    )
    print(json.dumps(result, sort_keys=True, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
