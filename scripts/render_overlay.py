"""把检查点中的最终掩码叠加到原图并渲染为 PNG（无头环境可用）。"""
from __future__ import annotations

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from segworkbench.gui.viewer import render_to_file  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description="渲染掩码叠加图")
    parser.add_argument("--data-dir", default="data/generated")
    parser.add_argument("--checkpoint-dir", required=True)
    parser.add_argument("--sample-id", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    path = render_to_file(args.data_dir, args.sample_id, args.checkpoint_dir, args.output)
    print(f"叠加图已写出: {os.path.abspath(path)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
