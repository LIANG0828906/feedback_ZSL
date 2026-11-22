from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path


def _run(args: list[str], cwd: Path) -> subprocess.CompletedProcess[str]:
    workspace = Path(__file__).resolve().parents[1]
    environment = os.environ.copy()
    environment["PYTHONPATH"] = str(workspace) + os.pathsep + environment.get(
        "PYTHONPATH", ""
    )
    return subprocess.run(
        [sys.executable, "-m", "glyph_workbench.cli", *args],
        cwd=cwd,
        env=environment,
        text=True,
        capture_output=True,
        check=False,
    )


def test_cli_validate_and_consistency_python_subprocess(
    sample_paths: dict[str, Path], tmp_path: Path
) -> None:
    model = sample_paths["samples"]
    validate = _run(
        ["validate", "--model", str(model), "--corrupt-dir", str(sample_paths["corrupt_dir"])],
        tmp_path,
    )
    assert validate.returncode == 0, validate.stderr
    assert json.loads(validate.stdout)["ok"] is True

    consistency = _run(
        [
            "consistency",
            "--model",
            str(model),
            "--glyph",
            "B",
            "--workers",
            "3",
            "--grid",
            "6",
            "--tolerance",
            "0.25",
        ],
        tmp_path,
    )
    assert consistency.returncode == 0, consistency.stderr
    payload = json.loads(consistency.stdout)
    assert payload["ok"] is True
    assert payload["pixel_exact"] is True
    assert payload["full_vs_live"]["pixel"] is None
    assert payload["full_vs_resume"]["pixel"] is None
    assert payload["commit_sequence"] == [0, 1, 2]

