#!/usr/bin/env python3
"""本地确定性样本生成入口: 固定种子产出多条媒体工程与真值清单。"""

import argparse
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from a11y_media.generator import generate_all


def main() -> None:
    ap = argparse.ArgumentParser(description="生成确定性样本工程")
    ap.add_argument("--out", default="data/samples", help="输出目录")
    ap.add_argument("--count", type=int, default=4, help="工程条数")
    ap.add_argument("--seed", type=int, default=42, help="基础种子")
    args = ap.parse_args()
    dirs = generate_all(args.out, count=args.count, base_seed=args.seed)
    for d in dirs:
        print(f"generated: {d}")


if __name__ == "__main__":
    main()
