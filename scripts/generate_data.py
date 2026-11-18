#!/usr/bin/env python3
"""Generate deterministic local SLI samples and handover forms."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from duty_handover.generator import generate_all  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="data", help="Output data directory")
    parser.add_argument("--seed", type=int, default=304, help="Fixed random seed")
    args = parser.parse_args()
    manifest = generate_all(args.out, args.seed)
    print(
        json.dumps(
            {
                "out": str(Path(args.out)),
                "seed": manifest["seed"],
                "sample_rows": manifest["sample_rows"],
                "services": manifest["services"],
                "samples_sha256": manifest["samples_sha256"],
            },
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
