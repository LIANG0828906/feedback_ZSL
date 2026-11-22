from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest

from sciseq.checkpoint import CheckpointStore
from sciseq.compare import compare_results
from sciseq.engine import LiveIncrementalSession, SequenceProcessor
from sciseq.errors import SciSeqError
from sciseq.state import CommittedPayload, FrameStateMachine, FrameStatus


def test_illegal_transition_preserves_state() -> None:
    machine = FrameStateMachine(4)
    with pytest.raises(SciSeqError) as caught:
        machine.transition(FrameStatus.COMMITTED)
    assert caught.value.code == "E_STATE"
    assert caught.value.frame == 4
    assert machine.status == FrameStatus.PENDING


def test_committed_payload_is_frozen() -> None:
    machine = FrameStateMachine(1)
    machine.transition(FrameStatus.REGISTERED)
    machine.transition(FrameStatus.DENOISING)
    payload = CommittedPayload(
        offset=(1.25, -0.5),
        denoised_sha256="a" * 64,
        content_sha256="b" * 64,
        parameter_version="params",
        algorithm_version="algo",
    )
    machine.commit(payload)
    with pytest.raises(SciSeqError) as caught:
        machine.transition(FrameStatus.PENDING)
    assert caught.value.code == "E_STATE"
    assert machine.status == FrameStatus.COMMITTED
    assert machine.payload == payload


def test_full_incremental_and_resume_are_pixel_identical(
    tiny_sequence,
    tiny_config,
    tmp_path: Path,
) -> None:
    full = SequenceProcessor(tiny_sequence, tiny_config).run_full(parallel=True)
    checkpoint = tmp_path / "checkpoint"
    first = SequenceProcessor(
        tiny_sequence,
        tiny_config,
        checkpoint_dir=checkpoint,
    ).run_incremental(parallel=True, pause_after=3, reset=True)
    assert not first.complete
    assert first.committed_count == 3
    assert len(list(checkpoint.glob("frame_*.npz"))) == 3
    resumed = SequenceProcessor(
        tiny_sequence,
        tiny_config,
        checkpoint_dir=checkpoint,
    ).run_resume(parallel=True)
    assert resumed.complete
    assert resumed.snapshot_reason == "snapshot validated"
    assert not resumed.invalidated
    report = compare_results(full, resumed.result, label="resume")
    assert report["mismatch_count"] == 0


def test_parameter_version_mismatch_invalidates_whole_snapshot(
    tiny_sequence,
    tiny_config,
    tmp_path: Path,
) -> None:
    checkpoint = tmp_path / "checkpoint"
    first = SequenceProcessor(
        tiny_sequence,
        tiny_config,
        checkpoint_dir=checkpoint,
    ).run_incremental(parallel=True, pause_after=3, reset=True)
    assert first.committed_count == 3
    changed = tiny_config.__class__(
        reference_frame=tiny_config.reference_frame,
        registration=tiny_config.registration,
        denoise=tiny_config.denoise.__class__(
            filter="guided",
            block_size=tiny_config.denoise.block_size,
            strength=tiny_config.denoise.strength,
        ),
        budget=tiny_config.budget,
    )
    resumed = SequenceProcessor(
        tiny_sequence,
        changed,
        checkpoint_dir=checkpoint,
    ).run_resume(parallel=True)
    assert resumed.complete
    assert resumed.invalidated
    assert resumed.quarantine_path is not None
    baseline = SequenceProcessor(tiny_sequence, changed).run_full(parallel=True)
    compare_results(baseline, resumed.result, label="invalidated-resume")


def test_corrupt_denoised_digest_invalidates_snapshot(
    tiny_sequence,
    tiny_config,
    tmp_path: Path,
) -> None:
    checkpoint = tmp_path / "checkpoint"
    SequenceProcessor(
        tiny_sequence,
        tiny_config,
        checkpoint_dir=checkpoint,
    ).run_incremental(parallel=True, pause_after=2, reset=True)
    record_path = checkpoint / "frame_000001.npz"
    with np.load(record_path, allow_pickle=False) as payload:
        arrays = {name: payload[name] for name in payload.files}
    arrays["denoised_sha256"] = np.asarray("0" * 64)
    with record_path.open("wb") as handle:
        np.savez(handle, **arrays)
    resumed = SequenceProcessor(
        tiny_sequence,
        tiny_config,
        checkpoint_dir=checkpoint,
    ).run_resume(parallel=True)
    assert resumed.complete
    assert resumed.invalidated
    assert "digest mismatch" in resumed.snapshot_reason


def test_snapshot_commits_are_contiguous(
    tiny_sequence,
    tiny_config,
    tmp_path: Path,
) -> None:
    store = CheckpointStore(
        tmp_path / "store",
        sequence_fingerprint=tiny_sequence.fingerprint(),
        parameter_version=tiny_config.parameter_version(),
        algorithm_version="algo",
        sequence_id=tiny_sequence.sequence_id,
        frame_count=tiny_sequence.frame_count,
        shape=tiny_sequence.shape,
    )
    with pytest.raises(SciSeqError) as caught:
        store.commit(
            1,
            offset=(0.0, 0.0),
            denoised=np.zeros(tiny_sequence.shape, dtype=np.float32),
            noise_sigma=1.0,
            background=0.0,
            stats={},
            content_sha256=tiny_sequence.frame_hashes[1],
        )
    assert caught.value.code == "E_CHECKPOINT"


def test_live_parameter_change_freezes_committed_frames(
    tiny_sequence,
    tiny_config,
    tmp_path: Path,
) -> None:
    checkpoint = tmp_path / "live"
    session = LiveIncrementalSession(
        tiny_sequence,
        tiny_config,
        checkpoint,
    )
    first = session.advance(frame_count=3, parallel=False)
    frozen_pixels = first.records[0].denoised.copy()
    frozen_offset = first.records[0].offset.copy()
    changed = tiny_config.__class__(
        reference_frame=tiny_config.reference_frame,
        registration=tiny_config.registration,
        denoise=tiny_config.denoise.__class__(
            filter="guided",
            block_size=tiny_config.denoise.block_size,
            strength=tiny_config.denoise.strength,
        ),
        budget=tiny_config.budget,
    )
    update = session.update_config(changed)
    assert update["applies_from_frame"] == 3
    assert np.array_equal(session.records[0].denoised, frozen_pixels)
    assert np.array_equal(session.records[0].offset, frozen_offset)
    final = session.advance(parallel=False)
    assert final.complete
    assert np.array_equal(final.records[0].denoised, frozen_pixels)
    assert final.records[3].stats["denoise"]["filter"] == "guided"
    assert session.mixed_parameter_versions
    restarted = SequenceProcessor(
        tiny_sequence,
        changed,
        checkpoint_dir=checkpoint,
    ).run_resume(parallel=True)
    assert restarted.complete
    assert restarted.invalidated
    baseline = SequenceProcessor(tiny_sequence, changed).run_full(parallel=True)
    compare_results(baseline, restarted.result, label="mixed-snapshot-restart")

