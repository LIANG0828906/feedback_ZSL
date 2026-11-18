from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

from minute_backfill.generator import GeneratorConfig, generate_dataset

from .helpers import run_cli


def _data(tmp_path: Path) -> Path:
    config = GeneratorConfig(
        out_dir=tmp_path / "data",
        seed=51,
        payment_rows=500,
        refund_rows=260,
        batches=10,
        minutes=16,
        delay_rate=0.1,
        out_of_order_rate=0.2,
        force=True,
    )
    generate_dataset(config)
    return config.out_dir


def test_spill_is_used_and_temp_is_cleaned(tmp_path: Path) -> None:
    data = _data(tmp_path)
    out = tmp_path / "spill-out"
    temp = tmp_path / "temp"
    result = run_cli(
        "run",
        "--data", str(data),
        "--state", str(tmp_path / "spill.db"),
        "--out", str(out),
        "--temp-dir", str(temp),
        "--workers", "0",
        "--spill-mb", "0.001",
        "--reset-state",
    )
    assert result.returncode == 0, result.stderr
    report = json.loads((out / "report.json").read_text())
    assert report["resources"]["spill_bytes"] > 0
    assert report["resources"]["temp_cleanup"] is True
    assert not list(temp.glob("run-*"))


def test_memory_error_preserves_committed_windows(tmp_path: Path) -> None:
    data = _data(tmp_path)
    state = tmp_path / "memory.db"
    out = tmp_path / "memory-out"
    killed = run_cli(
        "run",
        "--data", str(data),
        "--state", str(state),
        "--out", str(out),
        "--workers", "0",
        "--allowed-lateness-seconds", "0",
        "--terminate-after-batch", "3",
        "--reset-state",
    )
    assert killed.returncode == 137
    before = json.loads(
        subprocess.run(
            [
                sys.executable,
                "-m",
                "minute_backfill.cli",
                "inspect",
                "--data",
                str(data),
                "--state",
                str(state),
            ],
            capture_output=True,
            text=True,
            cwd=str(Path(__file__).resolve().parents[1]),
        ).stdout
    )
    committed = before["state"]["committed_minute_windows"]
    failed = run_cli(
        "run",
        "--data", str(data),
        "--state", str(state),
        "--out", str(out),
        "--workers", "0",
        "--memory-limit-mb", "0.001",
    )
    assert failed.returncode == 4
    partial = json.loads((out / "report.partial.json").read_text())
    assert partial["status"] == "failed"
    assert partial["error"]["code"] == "E_MEMORY"
    assert len(partial["windows"]) == committed
    resumed = run_cli(
        "run",
        "--data", str(data),
        "--state", str(state),
        "--out", str(out),
        "--workers", "0",
        "--allowed-lateness-seconds", "0",
    )
    assert resumed.returncode == 0, resumed.stderr
    final = json.loads((out / "report.json").read_text())
    assert len(final["windows"]) == 16
    assert final["resources"]["peak_memory_bytes"] < 1024**3
