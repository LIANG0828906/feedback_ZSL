from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from rec_gateway.assetgen import generate_assets


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic ONNX models and samples")
    parser.add_argument("--output-dir", default=str(ROOT / "generated"))
    parser.add_argument("--samples", type=int, default=10000)
    parser.add_argument("--seed", type=int, default=294)
    parser.add_argument("--inject-v4-load-failure", action="store_true")
    args = parser.parse_args()
    summary = generate_assets(
        args.output_dir,
        sample_count=args.samples,
        seed=args.seed,
        inject_v4_load_failure=args.inject_v4_load_failure,
    )
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
