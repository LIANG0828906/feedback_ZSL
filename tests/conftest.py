from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from risklake.config import JobConfig
from risklake.datagen import generate

REPO_ROOT = Path(__file__).resolve().parent.parent

SMALL = {
    "seed": 20261119,
    "historical_rows": 6000,
    "incremental_batches": 16,
    "incremental_rows": 4000,
}


def make_config(work_dir: Path, **overrides) -> JobConfig:
    params = dict(SMALL)
    params.update(overrides)
    work_dir.mkdir(parents=True, exist_ok=True)
    cfg = JobConfig(work_dir=str(work_dir), **params)
    cfg.save(work_dir / "config.json")
    return cfg


@pytest.fixture
def cfg_factory(tmp_path):
    def factory(name: str = "case", **overrides) -> JobConfig:
        return make_config(tmp_path / name, **overrides)

    return factory


@pytest.fixture
def generated(cfg_factory):
    """Small dataset with a lateness tolerance larger than any disorder."""
    cfg = cfg_factory("data", lateness_tolerance_seconds=700)
    generate(cfg)
    return cfg


def run_cli(args: list[str], **kwargs) -> subprocess.CompletedProcess:
    env = dict(os.environ, PYTHONPATH=str(REPO_ROOT))
    return subprocess.run(
        [sys.executable, "-m", "risklake.cli", *args],
        capture_output=True,
        text=True,
        cwd=REPO_ROOT,
        env=env,
        **kwargs,
    )


def run_engine_cli(cfg: JobConfig) -> subprocess.CompletedProcess:
    config_path = Path(cfg.work_dir) / "config.json"
    cfg.save(config_path)
    return run_cli(["run", "--config", str(config_path)])


def windows_cents(report_windows: list[dict]) -> list[dict]:
    return [
        {
            "window_start": w["window_start"],
            "order_amount_cents": round(w["order_amount"] * 100),
            "order_count": w["order_count"],
            "refund_amount_cents": round(w["refund_amount"] * 100),
            "refund_count": w["refund_count"],
            "matched_keys": w["matched_keys"],
        }
        for w in report_windows
    ]


def load_manifest(cfg: JobConfig) -> dict:
    return json.loads(
        (Path(cfg.work_dir) / cfg.data_dir / "manifest.json").read_text()
    )
