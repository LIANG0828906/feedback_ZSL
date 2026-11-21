from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

from tactical_review.reporting import compare_baseline
from tactical_review.simulation import Simulation


def test_recompute_fault_has_independent_code(loaded_sample, sample_dir: Path) -> None:
    initial, shots, revocations = loaded_sample
    with (sample_dir / "baseline.json").open("r", encoding="utf-8") as handle:
        baseline = json.load(handle)
    simulation = Simulation(initial, shots, revocations)
    summaries = simulation.run_to()
    errors = compare_baseline(
        baseline, summaries, recompute_fault_step=4
    )
    assert any(
        item["code"] == "E_RECOMPUTE_INJECTED" and item["step"] == 4
        for item in errors
    )


def test_cli_end_to_end_trajectory_is_reproducible(tmp_path: Path) -> None:
    sample = tmp_path / "sample"
    run_a = tmp_path / "run-a"
    run_b = tmp_path / "run-b"
    commands = [
        [
            sys.executable,
            "-m",
            "tactical_review",
            "generate",
            "--out",
            str(sample),
            "--seed",
            "9012",
            "--covers",
            "6",
            "--shots",
            "20",
            "--end-step",
            "20",
        ],
        [
            sys.executable,
            "-m",
            "tactical_review",
            "run",
            "--sample",
            str(sample),
            "--run-dir",
            str(run_a),
        ],
        [
            sys.executable,
            "-m",
            "tactical_review",
            "run",
            "--sample",
            str(sample),
            "--run-dir",
            str(run_b),
        ],
    ]
    for command in commands:
        result = subprocess.run(
            command,
            cwd=Path(__file__).resolve().parents[1],
            capture_output=True,
            text=True,
            check=False,
        )
        assert result.returncode == 0, result.stderr + result.stdout
    report_a = json.loads((run_a / "review_report.json").read_text("utf-8"))
    report_b = json.loads((run_b / "review_report.json").read_text("utf-8"))
    assert report_a["fatal_error"] is False
    assert report_a["final_hash"] == report_b["final_hash"]
    assert report_a["hash_chain"] == report_b["hash_chain"]
    assert (run_a / "trajectory.jsonl").read_bytes() == (
        run_b / "trajectory.jsonl"
    ).read_bytes()

