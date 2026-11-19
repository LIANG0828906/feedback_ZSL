#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from oob_rack.generate import build_config


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate a deterministic six-node rack manifest.")
    parser.add_argument("--output", default=str(ROOT / "config" / "rack.json"))
    parser.add_argument("--seed", type=int, default=2872026)
    args = parser.parse_args()
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(build_config(args.seed), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(f"wrote {output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
