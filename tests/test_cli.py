from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path


def _run_cli(args, cwd):
    return subprocess.run(
        [sys.executable, "-m", "mocap_workbench.cli", *args],
        cwd=cwd,
        check=False,
        capture_output=True,
        text=True,
        timeout=120,
    )


def test_cli_generate_solve_and_recover(tmp_path, compact_samples):
    root = Path(__file__).resolve().parents[1]
    sample = compact_samples["small"]
    snapshot_dir = tmp_path / "cli-snapshots"
    trajectory = tmp_path / "trajectory.jsonl"
    summary = tmp_path / "summary.json"
    conflicts = tmp_path / "conflicts.json"
    completed = _run_cli(
        [
            "solve",
            "--bvh",
            sample["bvh"],
            "--constraints",
            sample["constraints"],
            "--snapshot-dir",
            str(snapshot_dir),
            "--chunk-size",
            "16",
            "--workers",
            "1",
            "--trajectory",
            str(trajectory),
            "--summary",
            str(summary),
            "--conflicts-out",
            str(conflicts),
        ],
        root,
    )
    assert completed.returncode == 0, completed.stderr
    parsed = json.loads(completed.stdout)
    assert parsed["status"] == "completed"
    lines = trajectory.read_text(encoding="utf-8").splitlines()
    first = json.loads(lines[0])
    assert first["strategy"] in {"batch", "window"}
    frame = json.loads(lines[1])
    assert {"frame", "strategy", "residuals", "conflict", "cache_hit", "pose_hash"} <= set(
        frame
    )
    recovered = _run_cli(
        [
            "recover",
            "--bvh",
            sample["bvh"],
            "--constraints",
            sample["constraints"],
            "--snapshot-dir",
            str(snapshot_dir),
            "--chunk-size",
            "16",
            "--workers",
            "1",
        ],
        root,
    )
    assert recovered.returncode == 0, recovered.stderr
    recovery_payload = json.loads(recovered.stdout)
    assert recovery_payload["valid"]
    assert recovery_payload["committed_frames"] == 48


def test_cli_conflict_result_has_actionable_report(tmp_path, compact_samples):
    root = Path(__file__).resolve().parents[1]
    sample = compact_samples["conflict"]
    conflicts_path = tmp_path / "conflicts.json"
    completed = _run_cli(
        [
            "solve",
            "--bvh",
            sample["bvh"],
            "--constraints",
            sample["constraints"],
            "--snapshot-dir",
            str(tmp_path / "conflict-snap"),
            "--chunk-size",
            "20",
            "--workers",
            "1",
            "--conflicts-out",
            str(conflicts_path),
        ],
        root,
    )
    assert completed.returncode == 0, completed.stderr
    report = json.loads(completed.stdout)
    assert report["conflicts"] > 0
    conflicts = json.loads(conflicts_path.read_text(encoding="utf-8"))["conflicts"]
    first = conflicts[0]
    assert {
        "frame",
        "conflict_constraints",
        "joints",
        "worst_constraint_id",
        "worst_residual",
        "worst_tolerance",
        "evidence",
    } <= set(first)

