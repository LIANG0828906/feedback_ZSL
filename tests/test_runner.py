"""并发推进与协作式取消。"""

import time

import pytest

import replay.runner as runner_mod
from replay.errors import BusyError
from replay.generator import generate_sample
from replay.runner import MatchRunner
from replay.snapshot import SnapshotStore


class _SlowStore(SnapshotStore):
    def commit(self, payload):
        time.sleep(0.01)
        super().commit(payload)


@pytest.fixture()
def slow_store(monkeypatch):
    monkeypatch.setattr(runner_mod, "SnapshotStore", _SlowStore)


def test_busy_on_duplicate_start(tmp_path, slow_store):
    sample = generate_sample(seed=7, players=2, frames=200, disorder=0)
    runner = MatchRunner()
    handle = runner.start(sample, str(tmp_path / "snap"))
    try:
        with pytest.raises(BusyError):
            runner.start(sample, str(tmp_path / "snap2"))
    finally:
        handle.cancel()
        handle.join(timeout=10)
    assert handle.status == "CANCELLED"


def test_cooperative_cancel_keeps_last_snapshot(tmp_path, slow_store):
    sample = generate_sample(seed=7, players=2, frames=200, disorder=0)
    runner = MatchRunner()
    handle = runner.start(sample, str(tmp_path / "snap"))
    while handle.frames_done < 3:
        time.sleep(0.005)
    handle.cancel()
    handle.join(timeout=10)
    assert handle.status == "CANCELLED"
    store = SnapshotStore(str(tmp_path / "snap"))
    tip = store.validate_chain()
    assert tip["step"] >= 2
    assert not runner.running(sample["match_id"])


def test_completed_run_releases_slot(tmp_path):
    sample = generate_sample(seed=7, players=2, frames=8, disorder=1)
    runner = MatchRunner()
    handle = runner.start(sample, str(tmp_path / "snap"))
    handle.join(timeout=30)
    assert handle.status == "COMPLETED"
    assert len(handle.trace) == 8
    handle2 = runner.start(sample, str(tmp_path / "snap2"))
    handle2.join(timeout=30)
    assert handle2.status == "COMPLETED"
    assert handle.trace == handle2.trace
