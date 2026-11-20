from __future__ import annotations

import sys
import threading
from pathlib import Path

import pytest

from mocap_workbench.errors import (
    E_BUSY,
    E_RECOVERY_INVALID,
    BusinessError,
)
from mocap_workbench.locking import JobLock
from mocap_workbench.pipeline import SolvePipeline
from mocap_workbench.solver import SolverConfig


def _pipeline(record, snapshot_dir, **config):
    return SolvePipeline(
        record["bvh"],
        record["constraints"],
        snapshot_dir,
        SolverConfig(workers=1, **config),
    )


def test_cooperative_cancel_preserves_last_interval_and_resume(samples, tmp_path):
    snapshot_dir = tmp_path / "snapshots"
    cancel = threading.Event()
    first = _pipeline(samples["small"], snapshot_dir, chunk_size=30).run(
        resume=False,
        cancel_event=cancel,
        progress=lambda event: cancel.set(),
    )
    assert first.status == "cancelled"
    assert first.error_code == "E_CANCELLED"
    assert len(first.committed_ranges) == 1
    assert first.committed_ranges[0] == (0, 29)
    assert first.committed_ranges[0][1] < 89

    second = _pipeline(samples["small"], snapshot_dir, chunk_size=30).run(resume=True)
    assert second.status == "completed"
    assert second.recovered is not None
    assert second.recovered.last_end == 29
    assert sum(1 for result in second.results if result.restored) == 30
    assert len(second.results) == 90
    assert [result.frame for result in second.results] == list(range(90))
    assert any(result.frame == 30 and not result.restored for result in second.results)


def test_hard_kill_then_recover_from_atomic_snapshot(samples, tmp_path):
    snapshot_dir = tmp_path / "hard-kill-snapshots"
    command = [
        sys.executable,
        "-m",
        "mocap_workbench.cli",
        "solve",
        "--bvh",
        samples["small"]["bvh"],
        "--constraints",
        samples["small"]["constraints"],
        "--snapshot-dir",
        str(snapshot_dir),
        "--chunk-size",
        "30",
        "--workers",
        "1",
        "--crash-after-chunks",
        "1",
    ]
    import subprocess

    completed = subprocess.run(
        command,
        cwd=Path(__file__).resolve().parents[1],
        check=False,
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert completed.returncode == 23
    assert (snapshot_dir / "frame_00000000_00000029.snap").exists()
    assert (snapshot_dir / "run.json").exists()

    recovered = _pipeline(samples["small"], snapshot_dir, chunk_size=30).run(resume=True)
    assert recovered.status == "completed"
    assert recovered.recovered is not None
    assert recovered.recovered.last_end == 29
    assert recovered.recovered.snapshot_count == 1
    assert [item.frame for item in recovered.results] == list(range(90))
    assert recovered.committed_ranges == ((0, 89),)


def test_recovery_rejects_corrupt_snapshot(samples, tmp_path):
    snapshot_dir = tmp_path / "corrupt"
    report = _pipeline(samples["small"], snapshot_dir, chunk_size=90).run(resume=False)
    assert report.status == "completed"
    snapshot_file = next(snapshot_dir.glob("*.snap"))
    payload = bytearray(snapshot_file.read_bytes())
    snapshot_file.write_bytes(bytes(payload[:-8]))
    with pytest.raises(BusinessError) as caught:
        _pipeline(samples["small"], snapshot_dir, chunk_size=90).recover_only()
    assert caught.value.code == E_RECOVERY_INVALID


def test_recovery_rejects_missing_manifest(samples, tmp_path):
    snapshot_dir = tmp_path / "missing-manifest"
    _pipeline(samples["small"], snapshot_dir, chunk_size=90).run(resume=False)
    (snapshot_dir / "run.json").unlink()
    with pytest.raises(BusinessError) as caught:
        _pipeline(samples["small"], snapshot_dir, chunk_size=90).recover_only()
    assert caught.value.code == E_RECOVERY_INVALID


def test_solver_config_change_invalidates_recovery(samples, tmp_path):
    snapshot_dir = tmp_path / "versioned"
    _pipeline(samples["small"], snapshot_dir, chunk_size=90).run(resume=False)
    changed = SolvePipeline(
        samples["small"]["bvh"],
        samples["small"]["constraints"],
        snapshot_dir,
        SolverConfig(workers=1, chunk_size=90, max_iterations=99),
    )
    with pytest.raises(BusinessError) as caught:
        changed.run(resume=True)
    assert caught.value.code == E_RECOVERY_INVALID


def test_budget_failure_does_not_write_partial_snapshot(samples, tmp_path):
    snapshot_dir = tmp_path / "budget"
    report = SolvePipeline(
        samples["small"]["bvh"],
        samples["small"]["constraints"],
        snapshot_dir,
        SolverConfig(
            workers=1,
            chunk_size=30,
            memory_budget_bytes=1,
        ),
    ).run(resume=False)
    assert report.status == "budget_exceeded"
    assert report.error_code == "E_BUDGET_EXCEEDED"
    assert not list(snapshot_dir.glob("*.snap"))


def test_second_solve_for_same_bvh_returns_busy(samples, tmp_path):
    lock = JobLock(samples["small"]["bvh"])
    lock.acquire()
    try:
        with pytest.raises(BusinessError) as caught:
            _pipeline(samples["small"], tmp_path / "busy", chunk_size=30).run(
                resume=False
            )
        assert caught.value.code == E_BUSY
    finally:
        lock.release()


def test_conflict_frames_are_excluded_from_committed_ranges(compact_samples, tmp_path):
    report = _pipeline(
        compact_samples["conflict"],
        tmp_path / "conflict",
        chunk_size=20,
    ).run(resume=False)
    assert report.status == "completed_with_conflicts"
    assert report.conflict_reports
    conflict_frames = {item["frame"] for item in report.conflict_reports}
    committed = set(report.committed_frames)
    assert conflict_frames.isdisjoint(committed)
    recovery = _pipeline(
        compact_samples["conflict"],
        tmp_path / "conflict",
        chunk_size=20,
    ).recover_only()
    assert set(recovery.frames).isdisjoint(conflict_frames)
    assert recovery.next_frame == compact_samples["conflict"]["baseline_data"]["frame_count"]

