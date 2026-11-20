import json
import os
import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest

from sciworkbench.engine import Workbench
from sciworkbench.model import load_sequence
from sciworkbench.sample import PROFILES, generate_sample_bundle


def test_sample_generator_is_deterministic(tmp_path):
    first_dir = tmp_path / "first"
    second_dir = tmp_path / "second"
    first = generate_sample_bundle(
        first_dir,
        PROFILES["micro"],
        include_mode_comparison=True,
    )
    second = generate_sample_bundle(
        second_dir,
        PROFILES["micro"],
        include_mode_comparison=True,
    )
    assert (first_dir / "sequence.simg").read_bytes() == (
        second_dir / "sequence.simg"
    ).read_bytes()
    assert (first_dir / "factors.cal").read_bytes() == (
        second_dir / "factors.cal"
    ).read_bytes()
    assert first["result"].overall_digest == second["result"].overall_digest
    assert (first_dir / "baseline" / "trace.jsonl").read_bytes() == (
        second_dir / "baseline" / "trace.jsonl"
    ).read_bytes()
    assert (first_dir / "baseline" / "frames" / "frame_000000.f64").read_bytes() == (
        second_dir / "baseline" / "frames" / "frame_000000.f64"
    ).read_bytes()
    baseline = json.loads(
        (first_dir / "baseline_error_summary.json").read_text(encoding="utf-8")
    )
    assert "overall_digest" in baseline
    assert "step_summaries" in baseline
    assert "fixed_float_comparison" in baseline


def test_cli_generate_inspect_and_hard_recovery(tmp_path):
    python = sys.executable
    project = Path(__file__).resolve().parents[1]
    sample_dir = tmp_path / "sample"
    generate = subprocess.run(
        [
            python,
            "-m",
            "sciworkbench",
            "generate",
            "--out",
            str(sample_dir),
            "--preset",
            "micro",
            "--no-baseline",
        ],
        cwd=project,
        text=True,
        capture_output=True,
    )
    assert generate.returncode == 0, generate.stderr

    inspect = subprocess.run(
        [
            python,
            "-m",
            "sciworkbench",
            "inspect",
            "--sequence",
            str(sample_dir / "sequence.simg"),
            "--block-size",
            "16",
        ],
        cwd=project,
        text=True,
        capture_output=True,
    )
    assert inspect.returncode == 0, inspect.stderr
    inspected = json.loads(inspect.stdout)
    assert inspected["header"]["frame_count"] == PROFILES["micro"].frames
    assert inspected["structure"]["total_blocks"] > 0

    common = [
        python,
        "-m",
        "sciworkbench",
        "process",
        "--sequence",
        str(sample_dir / "sequence.simg"),
        "--calibration",
        str(sample_dir / "factors.cal"),
        "--block-size",
        "16",
    ]
    uninterrupted_root = tmp_path / "snap_uninterrupted"
    uninterrupted = subprocess.run(
        common
        + [
            "--snapshot-root",
            str(uninterrupted_root),
            "--output",
            str(tmp_path / "out_uninterrupted"),
        ],
        cwd=project,
        text=True,
        capture_output=True,
    )
    assert uninterrupted.returncode == 0, uninterrupted.stderr
    full_payload = json.loads(uninterrupted.stdout)

    interrupted_root = tmp_path / "snap_interrupted"
    interrupted = subprocess.run(
        common
        + [
            "--snapshot-root",
            str(interrupted_root),
            "--interrupt-after",
            "5",
        ],
        cwd=project,
        text=True,
        capture_output=True,
    )
    assert interrupted.returncode == 137
    resumed = subprocess.run(
        common
        + [
            "--snapshot-root",
            str(interrupted_root),
            "--resume",
            "--output",
            str(tmp_path / "out_resumed"),
        ],
        cwd=project,
        text=True,
        capture_output=True,
    )
    assert resumed.returncode == 0, resumed.stderr
    resumed_payload = json.loads(resumed.stdout)
    assert resumed_payload["overall_digest"] == full_payload["overall_digest"]
    assert resumed_payload["recovered_blocks"] == 5
    assert (
        tmp_path / "out_resumed" / "frames" / "frame_000000.f64"
    ).read_bytes() == (
        tmp_path / "out_uninterrupted" / "frames" / "frame_000000.f64"
    ).read_bytes()


def test_gui_smoke_non_blocking(tmp_path):
    QtWidgets = pytest.importorskip("PyQt5.QtWidgets")
    QtTest = pytest.importorskip("PyQt5.QtTest")
    from sciworkbench.gui import MainWindow

    bundle = generate_sample_bundle(
        tmp_path / "gui_sample",
        PROFILES["micro"],
        write_baseline=False,
        include_mode_comparison=False,
    )
    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    window = MainWindow(block_size=16, snapshot_root=str(tmp_path / "gui_snapshots"))
    window.sequence_path = Path(bundle["sequence"])
    window.calibration_path = Path(bundle["calibration"])
    window._load_workspace()
    assert window.frame_slider.isEnabled()
    window._start_processing()
    assert window.worker is not None
    for _ in range(200):
        QtTest.QTest.qWait(20)
        if window.worker is None:
            break
    assert window.workbench.result is not None
    assert window.view.result is not None
    assert window.progress.value() == 100
    window.close()
