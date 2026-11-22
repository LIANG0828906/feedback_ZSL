from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
    if not (ROOT / "samples" / "index.json").exists():
        subprocess.run(
            [sys.executable, "-m", "mediaforge.samplegen", "--out", "samples", "--seed", "20261123"],
            cwd=ROOT,
            check=True,
        )
    return subprocess.run(
        [sys.executable, "-m", "unittest", "discover", "-v"],
        cwd=ROOT,
    ).returncode


if __name__ == "__main__":
    raise SystemExit(main())
