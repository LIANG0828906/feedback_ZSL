from __future__ import annotations

import time
from pathlib import Path

import pytest

from tactical_review.errors import ReviewError
from tactical_review.runner import WorkbenchRunner
from tactical_review.snapshot import SnapshotStore


def test_duplicate_start_returns_busy(
    tmp_path: Path, loaded_sample
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "snapshots")
    runner = WorkbenchRunner(
        initial,
        shots,
        revocations,
        store,
        step_delay=0.05,
    )
    runner.start(end_step=12)
    try:
        with pytest.raises(ReviewError) as info:
            runner.start(end_step=12)
        assert info.value.code == "E_BUSY"
    finally:
        runner.cancel()
        assert runner.wait(3.0)
    assert store.list_steps()


def test_second_runner_on_same_branch_returns_busy(
    tmp_path: Path, loaded_sample
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "snapshots")
    first = WorkbenchRunner(initial, shots, revocations, store, step_delay=0.05)
    second = WorkbenchRunner(initial, shots, revocations, store, step_delay=0.05)
    first.start(end_step=12)
    try:
        with pytest.raises(ReviewError) as info:
            second.start(end_step=12)
        assert info.value.code == "E_BUSY"
    finally:
        first.cancel()
        assert first.wait(3.0)


def test_cancel_keeps_last_committed_snapshot(
    tmp_path: Path, loaded_sample
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "snapshots")
    runner = WorkbenchRunner(
        initial,
        shots,
        revocations,
        store,
        step_delay=0.01,
    )
    runner.start()
    deadline = time.monotonic() + 3.0
    while len(store.list_steps()) < 2 and time.monotonic() < deadline:
        time.sleep(0.005)
    runner.cancel()
    assert runner.wait(3.0)
    published = store.list_steps()
    assert published
    assert runner.status == "CANCELLED"
    time.sleep(0.05)
    assert store.list_steps() == published
    store.load_latest(initial)


def test_progress_fault_is_an_independent_business_code(
    tmp_path: Path, loaded_sample
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "snapshots")
    runner = WorkbenchRunner(
        initial,
        shots,
        revocations,
        store,
        step_delay=0.0,
        progress_fault_step=6,
    )
    runner.start(end_step=10)
    assert runner.wait(3.0)
    assert runner.status == "FAILED"
    assert runner.last_error["code"] == "E_PROGRESS_INJECTED"
    assert store.list_steps()[-1] == 5


def test_pause_only_stops_at_step_boundaries_and_resumes(
    tmp_path: Path, loaded_sample
) -> None:
    initial, shots, revocations = loaded_sample
    store = SnapshotStore(tmp_path / "snapshots")
    runner = WorkbenchRunner(
        initial,
        shots,
        revocations,
        store,
        step_delay=0.01,
    )
    runner.start()
    deadline = time.monotonic() + 3.0
    while len(store.list_steps()) < 2 and time.monotonic() < deadline:
        time.sleep(0.005)
    runner.pause()
    deadline = time.monotonic() + 2.0
    while runner.status != "PAUSED" and time.monotonic() < deadline:
        time.sleep(0.005)
    time.sleep(0.08)
    paused_steps = store.list_steps()
    time.sleep(0.15)
    assert store.list_steps() == paused_steps

    runner.resume()
    deadline = time.monotonic() + 3.0
    while len(store.list_steps()) <= len(paused_steps) and time.monotonic() < deadline:
        time.sleep(0.005)
    assert len(store.list_steps()) > len(paused_steps)
    runner.cancel()
    assert runner.wait(3.0)

