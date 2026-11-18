"""E_STATE: locked or corrupted state db aborts the job with a stable
business error code and polluted state is quarantined."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

from risklake.batchpath import baseline_windows
from risklake.datagen import generate

from conftest import REPO_ROOT, run_cli, run_engine_cli, windows_cents
import json


def _start_locker(db_path: Path, seconds: float = 30.0) -> subprocess.Popen:
    env = dict(os.environ, PYTHONPATH=str(REPO_ROOT))
    proc = subprocess.Popen(
        [
            sys.executable, "-m", "risklake.cli",
            "lock-db", "--db", str(db_path), "--seconds", str(seconds),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        cwd=REPO_ROOT,
        env=env,
    )
    assert proc.stdout.readline().startswith("LOCKED")
    return proc


def test_locked_state_db_fails_with_e_state(cfg_factory):
    cfg = cfg_factory("locked")
    generate(cfg)
    db_path = Path(cfg.work_dir) / cfg.state_db
    locker = _start_locker(db_path)
    try:
        proc = run_engine_cli(cfg)
    finally:
        locker.terminate()
        locker.wait()
    assert proc.returncode == 70
    assert "E_STATE" in proc.stderr


def test_corrupted_state_db_quarantined_then_recovers(cfg_factory):
    cfg = cfg_factory("corrupt", lateness_tolerance_seconds=700)
    generate(cfg)
    proc = run_engine_cli(cfg)
    assert proc.returncode == 0, proc.stderr

    db_path = Path(cfg.work_dir) / cfg.state_db
    proc = run_cli(["corrupt-db", "--db", str(db_path)])
    assert proc.returncode == 0

    proc = run_engine_cli(cfg)
    assert proc.returncode == 70
    assert "E_STATE" in proc.stderr
    # polluted state was moved aside, never served to later runs
    assert not db_path.exists()
    assert list(db_path.parent.glob("job_state.db.corrupt-*"))

    # a later run starts clean and still produces the correct result
    proc = run_engine_cli(cfg)
    assert proc.returncode == 0, proc.stderr
    report = json.loads((Path(cfg.work_dir) / cfg.report_path).read_text())
    assert windows_cents(report["windows"]) == baseline_windows(cfg)
