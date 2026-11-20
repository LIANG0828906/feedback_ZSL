"""原子快照提交与强制终止后的恢复。"""

import json
import os

import pytest

from replay.engine import ReplayEngine
from replay.errors import RecoveryInvalidError, SnapshotWriteError
from replay.generator import generate_sample
from replay.snapshot import SnapshotStore


def _run_with_snapshots(sample, directory, steps=None):
    store = SnapshotStore(directory)
    engine = ReplayEngine(sample)
    limit = steps if steps is not None else engine.frames
    while engine.step_no < limit:
        engine.step()
        store.commit(engine.snapshot_payload())
    return engine, store


def test_atomic_commit_leaves_no_tmp(tmp_path):
    sample = generate_sample(seed=7, players=2, frames=6, disorder=1)
    _, store = _run_with_snapshots(sample, str(tmp_path / "snap"))
    assert store.steps() == list(range(6))
    assert not [f for f in os.listdir(store.directory)
                if f.endswith(".tmp")]


def test_snapshot_write_failure_preserves_previous(tmp_path, monkeypatch):
    sample = generate_sample(seed=7, players=2, frames=4, disorder=0)
    store = SnapshotStore(str(tmp_path / "snap"))
    engine = ReplayEngine(sample)
    engine.step()
    store.commit(engine.snapshot_payload())
    good = open(store._path(0), "rb").read()
    engine.step()
    monkeypatch.setattr(
        os, "replace",
        lambda *a, **k: (_ for _ in ()).throw(OSError("boom")))
    with pytest.raises(SnapshotWriteError) as excinfo:
        store.commit(engine.snapshot_payload())
    assert excinfo.value.details["step"] == 1
    assert "boom" in excinfo.value.details["reason"]
    assert open(store._path(0), "rb").read() == good
    assert not os.path.exists(store._path(1))


def test_recovery_continues_without_reapplying(tmp_path):
    sample = generate_sample(seed=42, players=5, frames=20, disorder=3)
    full = ReplayEngine(sample)
    full.run()
    partial, store = _run_with_snapshots(sample, str(tmp_path / "snap"),
                                         steps=8)
    assert partial.step_no == 8
    recovered = ReplayEngine.recover(sample, store)
    recovered.run(on_frame=lambda r: store.commit(
        recovered.snapshot_payload()))
    assert recovered.chain_tip == full.chain_tip
    assert [h["state_hash"] for h in recovered.hash_chain] == \
        [h["state_hash"] for h in full.hash_chain]


def test_recovery_detects_broken_chain(tmp_path):
    sample = generate_sample(seed=11, players=3, frames=8, disorder=1)
    _, store = _run_with_snapshots(sample, str(tmp_path / "snap"))
    snap = store.load(3)
    snap["state"]["units"][0]["x"] += 1
    with open(store._path(3), "wb") as fh:
        fh.write(json.dumps(snap).encode("utf-8"))
    with pytest.raises(RecoveryInvalidError) as excinfo:
        ReplayEngine.recover(sample, store)
    assert excinfo.value.details["step"] == 3


def test_recovery_detects_version_mismatch(tmp_path):
    sample = generate_sample(seed=11, players=3, frames=4, disorder=0)
    _, store = _run_with_snapshots(sample, str(tmp_path / "snap"))
    snap = store.load(0)
    snap["version"] = 999
    with open(store._path(0), "wb") as fh:
        fh.write(json.dumps(snap).encode("utf-8"))
    with pytest.raises(RecoveryInvalidError) as excinfo:
        ReplayEngine.recover(sample, store)
    assert excinfo.value.details["step"] == 0


def test_recovery_without_snapshot_fails(tmp_path):
    sample = generate_sample(seed=11, players=3, frames=4, disorder=0)
    store = SnapshotStore(str(tmp_path / "empty"))
    with pytest.raises(RecoveryInvalidError):
        ReplayEngine.recover(sample, store)
