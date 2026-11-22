#!/usr/bin/env python3
"""Generate the checked deterministic local WAV fixtures."""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from audio_repair.samples import generate_deterministic_samples


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--output",
        type=Path,
        default=Path("artifacts/samples"),
        help="workspace-local sample directory",
    )
    args = parser.parse_args()
    manifest = generate_deterministic_samples(args.output)
    print(json.dumps(manifest, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

