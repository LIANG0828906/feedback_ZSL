from __future__ import annotations

import shutil
from pathlib import Path

import numpy as np
import pytest

from probe_baker.bake import BakeEngine
from probe_baker.errors import ErrorCode, ProbeError
from probe_baker.snapshot import SnapshotStore


def test_complete_snapshot_round_trip(room_scene, small_config, tmp_path) -> None:
    outcome = BakeEngine().bake(room_scene, small_config)
    store = SnapshotStore(tmp_path)
    store.commit_state(outcome.state, version=1)
    report = store.recover(tmp_path / "v00000001")
    assert report.status == "complete"
    assert report.clean_exit is True
    assert report.published is True
    assert report.pending_count == 0
    assert report.committed_count == outcome.state.grid.count
    assert report.snapshot is not None
    assert report.snapshot.content_hash == outcome.state.content_hash
    query = report.snapshot.query_probe(0)
    assert query.version == 1


class _ForcedStop(RuntimeError):
    pass


def test_forced_termination_recovers_only_uncommitted(room_scene, small_config, tmp_path) -> None:
    outcome = BakeEngine().bake(room_scene, small_config)
    store = SnapshotStore(tmp_path)
    with pytest.raises(_ForcedStop):
        store.commit_state(
            outcome.state,
            version=1,
            forced_terminate_after_chunks=2,
            terminate_fn=lambda: (_ for _ in ()).throw(_ForcedStop()),
        )
    report = store.recover(tmp_path / "v00000001")
    assert report.status == "partial"
    assert report.clean_exit is False
    assert report.published is False
    assert report.committed_count == 32
    assert report.pending_count == outcome.state.grid.count - 32
    assert report.snapshot is None
    resumed = BakeEngine().resume_from_recovery(report, room_scene, small_config)
    assert resumed.plan is not None
    assert np.array_equal(resumed.plan.resampled, report.pending_probe_ids)
    assert resumed.plan.recomputed.size <= (
        report.pending_count + report.total_probes
    )
    snapshot = SnapshotStore(tmp_path / "resumed").commit_state(
        resumed.state, version=1
    )
    recovered = SnapshotStore(tmp_path / "resumed").recover(snapshot)
    assert recovered.status == "complete"
    assert recovered.pending_count == 0


def test_missing_tail_is_partial_but_chain_gap_is_invalid(room_scene, small_config, tmp_path) -> None:
    outcome = BakeEngine().bake(room_scene, small_config)
    root = tmp_path / "snapshots"
    store = SnapshotStore(root)
    version_dir = store.commit_state(outcome.state, version=1)
    chunk_paths = sorted(version_dir.glob("chunk_*.npz"))
    assert len(chunk_paths) > 2
    chunk_paths[-1].unlink()
    (version_dir / "manifest.json").unlink()
    report = store.recover(version_dir)
    assert report.status == "partial"
    assert report.pending_count > 0

    # Create a gap while retaining a later chunk: the hash chain cannot prove continuity.
    chunk_paths[1].unlink()
    with pytest.raises(ProbeError) as exc:
        store.recover(version_dir)
    assert exc.value.code == ErrorCode.RECOVERY_INVALID


def test_missing_begin_and_missing_fields_are_recovery_invalid(
    room_scene, small_config, tmp_path
) -> None:
    outcome = BakeEngine().bake(room_scene, small_config)
    store = SnapshotStore(tmp_path)
    version_dir = store.commit_state(outcome.state, version=1)
    begin = version_dir / "begin.json"
    payload = begin.read_text(encoding="utf-8")
    begin.write_text(payload.replace('"chunk_size"', '"missing_chunk_size"'), encoding="utf-8")
    with pytest.raises(ProbeError) as exc:
        store.recover(version_dir)
    assert exc.value.code == ErrorCode.RECOVERY_INVALID

    begin.unlink()
    with pytest.raises(ProbeError) as missing:
        store.recover(version_dir)
    assert missing.value.code == ErrorCode.RECOVERY_INVALID

