"""Run tests and write the reproducible acceptance artifact bundle."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path


def main() -> int:
    root = Path(__file__).resolve().parents[1]
    subprocess.run([sys.executable, "-m", "pytest", "-q"], cwd=root, check=True)
    subprocess.run(
        [
            sys.executable,
            "-m",
            "dustrender",
            "acceptance",
            "--out",
            str(root / "artifacts" / "acceptance-run"),
        ],
        cwd=root,
        check=True,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
