#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from vpipe.samplegen import ensure_samples


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic v1/v2 samples")
    parser.add_argument("--root", default="samples", help="sample output directory")
    args = parser.parse_args()
    manifest = ensure_samples(Path(args.root))
    print(json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

