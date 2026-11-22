from __future__ import annotations

import json

import numpy as np
import pytest

from spatial_workbench.checkpoint import CheckpointStore
from spatial_workbench.errors import ErrorCode, WorkbenchError
from spatial_workbench.models import FrameRenderStats


def test_commit_records_all_required_fields(analyzed, tmp_path):
    _, frames, _ = analyzed
    store = CheckpointStore(
        tmp_path / "checkpoint.json",
        input_hash="input-hash",
        algorithm_version="algorithm-v1",
    )
    store.reset()
    stats = FrameRenderStats.from_signal(np.ones(16))
    store.commit(frames[0], stats)
    loaded = store.load()
    assert loaded.valid and loaded.frame_count == 1
    raw = json.loads((tmp_path / "checkpoint.json").read_text())
    record = raw["payload"]["frames"][0]
    assert record["input_hash"] == "input-hash"
    assert record["algorithm_version"] == "algorithm-v1"
    assert record["direct_delays_samples"] == frames[0].direct_delays_samples
    assert "energy_statistics" in record
    assert "params" in record
    assert raw["integrity"]


def test_commit_requires_next_frame_index(analyzed, tmp_path):
    _, frames, _ = analyzed
    store = CheckpointStore(
        tmp_path / "checkpoint.json",
        input_hash="input-hash",
        algorithm_version="algorithm-v1",
    )
    store.reset()
    with pytest.raises(WorkbenchError) as caught:
        store.commit(frames[1])
    assert caught.value.code == ErrorCode.E_STATE
    assert store.generation == 1


def test_checkpoint_keeps_independent_parameter_copy(analyzed, tmp_path):
    _, frames, _ = analyzed
    store = CheckpointStore(
        tmp_path / "checkpoint.json",
        input_hash="input-hash",
        algorithm_version="algorithm-v1",
    )
    store.reset()
    mutable = frames[0].clone()
    original = mutable.direct_delays_samples[0]
    store.commit(mutable)
    mutable.direct_delays_samples[0] = original + 123
    assert store.records[0].params.direct_delays_samples[0] == original


def test_input_hash_mismatch_invalidates_whole_snapshot(analyzed, tmp_path):
    _, frames, _ = analyzed
    path = tmp_path / "checkpoint.json"
    original = CheckpointStore(
        path,
        input_hash="input-a",
        algorithm_version="algorithm-v1",
    )
    original.reset()
    original.commit(frames[0])
    different = CheckpointStore(
        path,
        input_hash="input-b",
        algorithm_version="algorithm-v1",
    )
    result = different.load()
    assert not result.valid
    assert result.invalid_reason == "INPUT_HASH_MISMATCH"
    assert result.records == ()
    assert different.generation == 0


def test_algorithm_mismatch_invalidates_whole_snapshot(analyzed, tmp_path):
    _, frames, _ = analyzed
    path = tmp_path / "checkpoint.json"
    original = CheckpointStore(
        path,
        input_hash="input-a",
        algorithm_version="algorithm-v1",
    )
    original.reset()
    original.commit(frames[0])
    different = CheckpointStore(
        path,
        input_hash="input-a",
        algorithm_version="algorithm-v2",
    )
    result = different.load()
    assert not result.valid
    assert result.invalid_reason == "ALGORITHM_VERSION_MISMATCH"
    assert result.records == ()


def test_corrupt_checkpoint_is_rejected_without_partial_frames(tmp_path):
    path = tmp_path / "checkpoint.json"
    path.write_text("{broken", encoding="utf-8")
    store = CheckpointStore(
        path,
        input_hash="input-a",
        algorithm_version="algorithm-v1",
    )
    result = store.load()
    assert not result.valid
    assert result.records == ()
    assert result.invalid_reason.startswith("CHECKPOINT_DECODE")


def test_serial_commits_produce_valid_snapshot(analyzed, tmp_path):
    _, frames, _ = analyzed
    store = CheckpointStore(
        tmp_path / "checkpoint.json",
        input_hash="input-a",
        algorithm_version="algorithm-v1",
    )
    store.reset()
    for frame in frames[:8]:
        store.commit(frame)
    loaded = store.load()
    assert loaded.valid
    assert loaded.frame_count == 8
    assert [record.frame_index for record in loaded.records] == list(range(8))

