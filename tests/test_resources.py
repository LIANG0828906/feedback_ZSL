"""Memory ceiling, spill-to-disk and report determinism."""

from __future__ import annotations

import json
import shutil
from pathlib import Path

import pytest

from risklake.batchpath import baseline_windows
from risklake.datagen import generate
from risklake.engine import run_job
from risklake.errors import MemoryLimitError

from conftest import run_engine_cli, windows_cents

VOLATILE_FIELDS = {"generated_at", "duration_seconds"}


def _strip_volatile(report: dict) -> dict:
    return {k: v for k, v in report.items() if k not in VOLATILE_FIELDS}


def test_spill_to_disk_and_cleanup(cfg_factory):
    cfg = cfg_factory(
        "spill", lateness_tolerance_seconds=700, state_mem_threshold_bytes=2048
    )
    generate(cfg)
    report = run_job(cfg)
    assert report["resources"]["spill_bytes"] > 0
    assert windows_cents(report["windows"]) == baseline_windows(cfg)
    # spill files are cleaned up after the job
    temp_dir = Path(cfg.work_dir) / cfg.temp_dir
    leftovers = list(temp_dir.rglob("spill-*")) if temp_dir.exists() else []
    assert leftovers == []


def test_memory_limit_aborts_with_e_mem_without_losing_windows(cfg_factory):
    cfg = cfg_factory("oom", lateness_tolerance_seconds=700, memory_limit_bytes=1)
    generate(cfg)
    with pytest.raises(MemoryLimitError) as excinfo:
        run_job(cfg)
    assert excinfo.value.code == "E_MEM"

    # committed state is intact: raising the limit lets the same job
    # resume and finish with the correct result
    cfg.memory_limit_bytes = 1 << 30
    report = run_job(cfg)
    assert windows_cents(report["windows"]) == baseline_windows(cfg)


def test_report_is_deterministic_across_runs(cfg_factory):
    cfg = cfg_factory("det", lateness_tolerance_seconds=0)
    generate(cfg)
    reports = []
    for _ in range(2):
        # each execution is a fresh job process with fresh state
        shutil.rmtree(Path(cfg.work_dir) / "state", ignore_errors=True)
        proc = run_engine_cli(cfg)
        assert proc.returncode == 0, proc.stderr
        reports.append(
            json.loads((Path(cfg.work_dir) / cfg.report_path).read_text())
        )
    assert _strip_volatile(reports[0]) == _strip_volatile(reports[1])

    # report file itself has stable (sorted) key ordering
    raw = (Path(cfg.work_dir) / cfg.report_path).read_text()
    parsed = json.loads(raw)
    assert list(parsed.keys()) == sorted(parsed.keys())
