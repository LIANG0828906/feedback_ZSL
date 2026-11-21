#!/usr/bin/env python3
"""Generate the deterministic room/city sample suite and baseline."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from lightprobe.samples import generate_samples


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="samples")
    args = parser.parse_args()
    print(json.dumps(generate_samples(args.out), ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()

