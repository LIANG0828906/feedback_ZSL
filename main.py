"""ScoreForge 桌面应用入口."""

from __future__ import annotations

import argparse
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser(description="ScoreForge 桌面应用")
    parser.add_argument("--samples", default="samples")
    parser.add_argument("--checkpoints", default="checkpoints")
    args = parser.parse_args()

    from scoreforge.generator import write_samples

    if not list(Path(args.samples).glob("*.json")):
        paths = write_samples(args.samples)
        print(f"已生成 {len(paths)} 个确定性样本工程到 {args.samples}/")

    Path(args.checkpoints).mkdir(parents=True, exist_ok=True)

    from scoreforge.gui.app import run

    return run(args.samples, args.checkpoints)


if __name__ == "__main__":
    raise SystemExit(main())
