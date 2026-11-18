"""Small test-facing helper around the public comparison CLI."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path


def compare_artifacts(expected: str | Path, actual: str | Path) -> None:
    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "veilrender",
            "compare",
            "--expected",
            str(expected),
            "--actual",
            str(actual),
        ],
        capture_output=True,
        text=True,
        timeout=60,
    )
    if result.returncode != 0:
        raise AssertionError(result.stderr or result.stdout)

