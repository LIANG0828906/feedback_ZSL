from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PYTHON = sys.executable
ENV = os.environ.copy()
ENV["PYTHONPATH"] = str(ROOT / "src")


def run_cli(*arguments: str, timeout: float = 60.0) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [PYTHON, "-m", "biomemaster", *arguments],
        cwd=ROOT,
        env=ENV,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def render_command(
    project: Path, output: Path, report: Path, work: Path, workers: int
) -> list[str]:
    return [
        PYTHON,
        "-m",
        "biomemaster",
        "render",
        str(project),
        "--output",
        str(output),
        "--report",
        str(report),
        "--work-dir",
        str(work),
        "--workers",
        str(workers),
    ]
