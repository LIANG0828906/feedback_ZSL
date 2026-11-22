#!/usr/bin/env python3
"""Generate the deterministic sample dataset (images, ground truth,
v1/v2/v3 OCR result description samples). Fully offline."""
import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from ocr_pipeline.generator import DEFAULT_NUM_IMAGES, DEFAULT_SEED, generate_dataset


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default="data", help="output data directory")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    parser.add_argument("--num-images", type=int, default=DEFAULT_NUM_IMAGES)
    args = parser.parse_args()
    manifests = generate_dataset(args.out, seed=args.seed, num_images=args.num_images)
    total_regions = sum(len(m["regions"]) for m in manifests)
    print(
        f"generated {len(manifests)} images ({total_regions} gt regions) "
        f"under {args.out}/ with seed {args.seed}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
