from __future__ import annotations

import json
from pathlib import Path

from minute_backfill.generator import GeneratorConfig, generate_dataset

from .helpers import business_signature, run_cli


def _data(tmp_path: Path) -> Path:
    config = GeneratorConfig(
        out_dir=tmp_path / "data",
        seed=23,
        payment_rows=700,
        refund_rows=420,
        batches=15,
        minutes=22,
        delay_rate=0.12,
        out_of_order_rate=0.3,
        force=True,
    )
    generate_dataset(config)
    return config.out_dir


def test_hard_termination_and_resume_are_equivalent(tmp_path: Path) -> None:
    data = _data(tmp_path)
    full_state = tmp_path / "full.db"
    full_out = tmp_path / "full-out"
    full = run_cli(
        "run",
        "--data", str(data),
        "--state", str(full_state),
        "--out", str(full_out),
        "--workers", "0",
        "--allowed-lateness-seconds", "0",
        "--reset-state",
    )
    assert full.returncode == 0, full.stderr
    full_report = json.loads((full_out / "report.json").read_text())

    killed_state = tmp_path / "killed.db"
    killed_out = tmp_path / "killed-out"
    killed = run_cli(
        "run",
        "--data", str(data),
        "--state", str(killed_state),
        "--out", str(killed_out),
        "--workers", "0",
        "--allowed-lateness-seconds", "0",
        "--terminate-after-batch", "6",
        "--reset-state",
    )
    assert killed.returncode == 137, killed.stderr
    assert (killed_out / "termination_injected.json").exists()

    resumed = run_cli(
        "run",
        "--data", str(data),
        "--state", str(killed_state),
        "--out", str(killed_out),
        "--workers", "0",
        "--allowed-lateness-seconds", "0",
    )
    assert resumed.returncode == 0, resumed.stderr
    resumed_report = json.loads((killed_out / "report.json").read_text())
    assert business_signature(full_report) == business_signature(resumed_report)
    assert resumed_report["recovery"]["count"] == 1
    checkpoint = resumed_report["recovery"]["checkpoints"][0]
    assert checkpoint["resumed_from_input_id"] == "batch-000006"
    assert checkpoint["resumed_from_commit_seq"] == 7

    again = run_cli(
        "run",
        "--data", str(data),
        "--state", str(killed_state),
        "--out", str(killed_out),
        "--workers", "0",
        "--allowed-lateness-seconds", "0",
    )
    assert again.returncode == 0, again.stderr
    again_report = json.loads((killed_out / "report.json").read_text())
    assert len(again_report["windows"]) == len(resumed_report["windows"])
    assert [window["commit_seq"] for window in again_report["windows"]] == [
        window["commit_seq"] for window in resumed_report["windows"]
    ]
    assert again_report["recovery"]["count"] == 2
