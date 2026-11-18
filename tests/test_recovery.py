"""Kill / resume consistency: a job killed after any batch must resume
from committed state and finish with results identical to an
uninterrupted run."""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

from risklake.datagen import generate
from risklake.engine import run_job

from conftest import REPO_ROOT, run_engine_cli, windows_cents

KILL_AFTER = 14  # batch sequence number after which the job is killed


def _run_until_killed(cfg, kill_after: int) -> int:
    env = dict(os.environ, PYTHONPATH=str(REPO_ROOT))
    config_path = Path(cfg.work_dir) / "config.json"
    cfg.save(config_path)
    proc = subprocess.Popen(
        [sys.executable, "-m", "risklake.cli", "run", "--config", str(config_path)],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        cwd=REPO_ROOT,
        env=env,
    )
    for line in proc.stdout:
        if line.strip() == f"BATCH_COMMITTED {kill_after}":
            proc.kill()
            break
    proc.wait()
    return proc.returncode


def test_kill_and_resume_matches_uninterrupted(cfg_factory):
    # uninterrupted reference run
    ref_cfg = cfg_factory("ref", lateness_tolerance_seconds=0)
    generate(ref_cfg)
    reference = run_job(ref_cfg)

    # fault-injected run: external SIGKILL after a committed batch
    cfg = cfg_factory("killed", lateness_tolerance_seconds=0)
    generate(cfg)
    returncode = _run_until_killed(cfg, KILL_AFTER)
    assert returncode != 0

    proc = run_engine_cli(cfg)
    assert proc.returncode == 0, proc.stderr
    report = json.loads((Path(cfg.work_dir) / cfg.report_path).read_text())

    assert report["recovery"]["resumed_from_batch"] == KILL_AFTER
    assert report["recovery"]["recoveries"] == 1
    assert windows_cents(report["windows"]) == windows_cents(reference["windows"])
    assert report["watermark"]["late_events"] == reference["watermark"]["late_events"]
    assert report["input"]["dedup_dropped"] == reference["input"]["dedup_dropped"]
    assert report["input"]["batches_consumed"] == reference["input"]["batches_consumed"]

    # re-running the finished job must not recompute or duplicate output
    proc = run_engine_cli(cfg)
    assert proc.returncode == 0, proc.stderr
    again = json.loads((Path(cfg.work_dir) / cfg.report_path).read_text())
    assert windows_cents(again["windows"]) == windows_cents(reference["windows"])
    assert again["recovery"]["recoveries"] == 1

    # committed windows are unique per window_start in the state db
    import sqlite3

    conn = sqlite3.connect(str(Path(cfg.work_dir) / cfg.state_db))
    total, distinct = conn.execute(
        "SELECT COUNT(*), COUNT(DISTINCT window_start) FROM committed_windows"
    ).fetchone()
    conn.close()
    assert total == distinct
