#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from inference_router.modelgen import generate_models  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic two-tier ONNX models")
    parser.add_argument("--out", default=str(ROOT / "models"))
    parser.add_argument("--seed", type=int, default=20261119)
    args = parser.parse_args()
    manifest = generate_models(Path(args.out), args.seed)
    print(json.dumps(manifest, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
