from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path


def _run_cli(repo_root: Path, args: list[str], *, expected: int = 0):
    env = dict(os.environ)
    env["PYTHONPATH"] = str(repo_root / "src")
    completed = subprocess.run(
        [sys.executable, "-m", "edgepipe", *args],
        cwd=str(repo_root),
        env=env,
        text=True,
        capture_output=True,
        timeout=120,
    )
    assert completed.returncode == expected, (
        completed.stdout,
        completed.stderr,
    )
    return completed


def test_forced_termination_then_resume_from_atomic_commits(
    repo_root: Path, tmp_path: Path, first_sample
):
    sample = first_sample["name"]
    baseline_dir = tmp_path / "baseline"
    crash_dir = tmp_path / "crash"
    baseline_mask = tmp_path / "baseline.png"
    resume_mask = tmp_path / "resume.png"
    crash_report = tmp_path / "crash-report.json"
    baseline = _run_cli(
        repo_root,
        [
            "run",
            "--sample",
            sample,
            "--mode",
            "full",
            "--checkpoint-dir",
            str(baseline_dir),
            "--mask",
            str(baseline_mask),
            "--report",
            str(tmp_path / "baseline-report.json"),
        ],
    )
    baseline_payload = json.loads(baseline.stdout)
    assert baseline_payload["complete"]

    crashed = _run_cli(
        repo_root,
        [
            "run",
            "--sample",
            sample,
            "--mode",
            "full",
            "--checkpoint-dir",
            str(crash_dir),
            "--crash-after-commits",
            "7",
            "--report",
            str(crash_report),
            "--mask",
            str(tmp_path / "crash.png"),
        ],
        expected=17,
    )
    assert not crash_report.exists()

    resumed = _run_cli(
        repo_root,
        [
            "run",
            "--sample",
            sample,
            "--mode",
            "resume",
            "--checkpoint-dir",
            str(crash_dir),
            "--mask",
            str(resume_mask),
            "--report",
            str(tmp_path / "resume-report.json"),
        ],
    )
    payload = json.loads(resumed.stdout)
    assert payload["complete"]
    assert payload["restored_commits"] == 7
    assert baseline_mask.read_bytes() == resume_mask.read_bytes()

