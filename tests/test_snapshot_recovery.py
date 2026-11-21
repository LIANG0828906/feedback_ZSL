from __future__ import annotations

import base64
import json

import numpy as np
import pytest

from volrender.errors import AppError, ErrorCode
from volrender.render import SimulatedTermination, render_frame, summary_bytes
from volrender.snapshot import SnapshotCheckpoint, SnapshotStore, load_snapshot


def test_atomic_snapshot_stop_and_resume(sample_bundle, tmp_path):
    loaded = sample_bundle["loaded"]
    store = SnapshotStore(tmp_path / "snapshots")
    checkpoint = SnapshotCheckpoint(
        store, loaded.scene, loaded.config, "float64"
    )
    with pytest.raises(SimulatedTermination):
        render_frame(
            loaded.scene,
            loaded.config,
            "float64",
            checkpoint=checkpoint,
            stop_after=2,
        )
    latest = store.latest_path()
    assert latest is not None
    loaded_snapshot = store.load_latest(
        loaded.scene, loaded.config, "float64"
    )
    assert loaded_snapshot is not None
    assert len(loaded_snapshot.completed) >= 1
    assert loaded_snapshot.document["completed_chunk_indices"] == sorted(
        loaded_snapshot.completed
    )

    resumed = render_frame(
        loaded.scene,
        loaded.config,
        "float64",
        completed=loaded_snapshot.completed,
    )
    fresh = render_frame(loaded.scene, loaded.config, "float64")
    assert resumed.color_bytes == fresh.color_bytes
    assert resumed.transmittance_bytes == fresh.transmittance_bytes
    assert summary_bytes(resumed.summary) == summary_bytes(fresh.summary)
    skipped = {
        event["tile_index"]
        for event in resumed.trace
        if event["event"] == "resume_skip"
    }
    completed_this_run = {
        event["tile_index"]
        for event in resumed.trace
        if event["event"] == "complete"
    }
    assert skipped == set(loaded_snapshot.completed)
    assert not (skipped & completed_this_run)


def test_snapshot_hash_chain_tamper_is_rejected(sample_bundle, tmp_path):
    loaded = sample_bundle["loaded"]
    store = SnapshotStore(tmp_path / "snapshots")
    checkpoint = SnapshotCheckpoint(
        store, loaded.scene, loaded.config, "float64"
    )
    with pytest.raises(SimulatedTermination):
        render_frame(
            loaded.scene,
            loaded.config,
            "float64",
            checkpoint=checkpoint,
            stop_after=1,
        )
    path = store.latest_path()
    assert path is not None
    document = json.loads(path.read_text(encoding="utf-8"))
    raw = base64.b64decode(document["entries"][0]["payload_b64"])
    document["entries"][0]["payload_b64"] = base64.b64encode(
        bytes([raw[0] ^ 0xFF]) + raw[1:]
    ).decode("ascii")
    path.write_text(json.dumps(document), encoding="utf-8")
    with pytest.raises(AppError) as caught:
        load_snapshot(path, loaded.scene, loaded.config, "float64")
    assert caught.value.code == ErrorCode.E_RECOVERY_INVALID
    assert "hash" in caught.value.message


def test_snapshot_missing_field_is_rejected(sample_bundle, tmp_path):
    loaded = sample_bundle["loaded"]
    store = SnapshotStore(tmp_path / "snapshots")
    checkpoint = SnapshotCheckpoint(
        store, loaded.scene, loaded.config, "float64"
    )
    with pytest.raises(SimulatedTermination):
        render_frame(
            loaded.scene,
            loaded.config,
            "float64",
            checkpoint=checkpoint,
            stop_after=1,
        )
    path = store.latest_path()
    assert path is not None
    document = json.loads(path.read_text(encoding="utf-8"))
    document.pop("chain_head")
    path.write_text(json.dumps(document), encoding="utf-8")
    with pytest.raises(AppError) as caught:
        load_snapshot(path, loaded.scene, loaded.config, "float64")
    assert caught.value.code == ErrorCode.E_RECOVERY_INVALID
    assert caught.value.details.get("missing_field") == "chain_head"
