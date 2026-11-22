"""环境与离线自检：Python/依赖版本、样本完整性、关键错误码冒烟。"""

from __future__ import annotations

import json
import os
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

import numpy  # noqa: E402
import PySide6  # noqa: E402

from cwb.integrity import check_samples  # noqa: E402


def main() -> int:
    print("python:", sys.version.split()[0])
    print("numpy:", numpy.__version__)
    print("PySide6:", PySide6.__version__)
    info = check_samples(os.path.join(ROOT, "samples"))
    print("samples:", json.dumps(info, ensure_ascii=False))
    print("network at runtime: disabled (no network API is imported in cwb)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
