import hashlib
import re
import threading

import numpy as np
import pytest

from sciworkbench.engine import Workbench
from sciworkbench.errors import E_BUSY, E_CANCELLED, E_RECOVERY_INVALID, WorkbenchError
from sciworkbench.factors import load_factors, save_factors
from sciworkbench.model import load_sequence, write_sequence
from sciworkbench.sample import SampleProfile, generate_frames_and_factors


def _large_sample(tmp_path):
    profile = SampleProfile(
        name="incremental_96x64x2",
        width=96,
        height=64,
        frames=2,
        bad_density=0.003,
        noise_sigma=1.4,
        seed=5151,
        block_size=8,
        frac_bits=10,
    )
    frames, factors = generate_frames_and_factors(profile)
    sequence_path = tmp_path / "large.simg"
    calibration_path = tmp_path / "large.cal"
    write_sequence(sequence_path, frames)
    sequence = load_sequence(sequence_path, block_size=8)
    save_factors(calibration_path, factors, sequence.structure)
    return sequence, factors, sequence_path, calibration_path


def test_incremental_gain_change_reuses_unaffected_blocks(tmp_path):
    sequence, factors, _, _ = _large_sample(tmp_path)
    workbench = Workbench(block_size=8, snapshot_root=tmp_path / "snapshots")
    workbench.initialize(sequence, factors, "fixed")
    first = workbench.process()

    changed_gain = np.array(factors.block_gain, copy=True)
    changed_gain[0, 0] *= 1.15
    changed_factors = factors.replace("block_gain", changed_gain)
    workbench.set_factors(changed_factors)
    incremental = workbench.process()

    assert 0 < incremental.recomputed_blocks < sequence.structure.total_blocks
    assert incremental.reused_blocks > 0

    untouched_ordinal = sequence.structure.total_blocks - 1
    assert (
        incremental.blocks[untouched_ordinal].content_hash
        == first.blocks[untouched_ordinal].content_hash
    )
    recomputed_lines = [
        item
        for item in incremental.trace_lines
        if item["action"] == "recompute"
    ]
    assert recomputed_lines
    assert any("block_gain" in item["factors_changed"] for item in recomputed_lines)
    assert any(
        item["action"] == "reuse" and item["reused_blocks"] > 0
        for item in incremental.trace_lines
    )

    fresh_workbench = Workbench(
        block_size=8, snapshot_root=tmp_path / "fresh_snapshots"
    )
    fresh_workbench.initialize(sequence, changed_factors, "fixed")
    fresh = fresh_workbench.process()
    assert fresh.overall_digest == incremental.overall_digest
    assert fresh.frame_bytes(0) == incremental.frame_bytes(0)


def test_repeat_processing_is_byte_reproducible(tmp_path):
    sequence, factors, _, _ = _large_sample(tmp_path)
    workbench = Workbench(block_size=8, snapshot_root=tmp_path / "snapshots")
    workbench.initialize(sequence, factors, "fixed")
    first = workbench.process()
    second = workbench.process()
    assert second.recomputed_blocks == 0
    assert second.reused_blocks == sequence.structure.total_blocks
    assert first.overall_digest == second.overall_digest
    for frame in range(sequence.header.frame_count):
        assert first.frame_bytes(frame) == second.frame_bytes(frame)
        assert first.error_bytes(frame) == second.error_bytes(frame)
    assert first.canonical_summary()["step_summaries"] == second.canonical_summary()[
        "step_summaries"
    ]


def test_frame_specific_factor_only_recomputes_changed_frame(tmp_path):
    sequence, factors, _, _ = _large_sample(tmp_path)
    stacked_dark = np.stack(
        [factors.dark, factors.dark], axis=0
    ).astype(np.float64)
    frame_factors = factors.replace("dark", stacked_dark)
    workbench = Workbench(block_size=8, snapshot_root=tmp_path / "frame_snaps")
    workbench.initialize(sequence, frame_factors, "fixed")
    first = workbench.process()

    changed_dark = np.array(stacked_dark, copy=True)
    changed_dark[1, 0, 0] += 5.0
    workbench.update_factor("dark", changed_dark)
    second = workbench.process()

    assert second.recomputed_blocks < sequence.structure.total_blocks
    assert second.reused_blocks > 0
    assert all(
        block.frame_index == 1
        for block in second.blocks
        if block.content_hash != first.block_map[block.ordinal].content_hash
    )


def test_two_fresh_runs_have_identical_logs_and_outputs(tmp_path):
    sequence, factors, _, _ = _large_sample(tmp_path)
    first_workbench = Workbench(
        block_size=8, snapshot_root=tmp_path / "logs_a"
    )
    first_workbench.initialize(sequence, factors, "fixed")
    first = first_workbench.process()
    second_workbench = Workbench(
        block_size=8, snapshot_root=tmp_path / "logs_b"
    )
    second_workbench.initialize(sequence, factors, "fixed")
    second = second_workbench.process()
    assert first.trace_jsonl() == second.trace_jsonl()
    assert first.overall_digest == second.overall_digest
    assert all(
        first.frame_bytes(frame) == second.frame_bytes(frame)
        for frame in range(sequence.header.frame_count)
    )


def test_forced_termination_resume_matches_uninterrupted(tmp_path):
    sequence, factors, _, _ = _large_sample(tmp_path)
    uninterrupted_workbench = Workbench(
        block_size=8, snapshot_root=tmp_path / "uninterrupted"
    )
    uninterrupted_workbench.initialize(sequence, factors, "fixed")
    uninterrupted = uninterrupted_workbench.process()

    interrupted_root = tmp_path / "interrupted"
    interrupted_workbench = Workbench(
        block_size=8, snapshot_root=interrupted_root
    )
    interrupted_workbench.initialize(sequence, factors, "fixed")
    from sciworkbench.errors import ForcedTermination

    with pytest.raises(ForcedTermination) as caught:
        interrupted_workbench.process(interrupt_after_commits=7)
    assert caught.value.committed_blocks == 7

    recovery_workbench = Workbench(
        block_size=8, snapshot_root=interrupted_root
    )
    recovery_workbench.initialize(sequence, factors, "fixed")
    recovered = recovery_workbench.process(resume=True)
    assert recovered.recovered_blocks == 7
    assert recovered.overall_digest == uninterrupted.overall_digest
    assert recovered.frame_bytes(0) == uninterrupted.frame_bytes(0)
    assert recovered.error_bytes(0) == uninterrupted.error_bytes(0)

    block_dir = recovered.blocks[0].factor_versions
    assert block_dir
    snapshot_dirs = sorted((interrupted_root / sequence.sequence_hash[:16]).glob("v*"))
    assert snapshot_dirs
    block_hashes = {
        path.name: hashlib.sha256(path.read_bytes()).hexdigest()
        for path in (snapshot_dirs[-1] / "blocks").glob("*.blk")
    }
    again = recovery_workbench.process(resume=True)
    assert again.recovered_blocks == sequence.structure.total_blocks
    assert again.recomputed_blocks == 0
    assert again.overall_digest == uninterrupted.overall_digest
    after_hashes = {
        path.name: hashlib.sha256(path.read_bytes()).hexdigest()
        for path in (snapshot_dirs[-1] / "blocks").glob("*.blk")
    }
    assert block_hashes == after_hashes


def test_broken_hash_chain_reports_first_bad_block(tmp_path):
    sequence, factors, _, _ = _large_sample(tmp_path)
    interrupted_root = tmp_path / "broken"
    workbench = Workbench(block_size=8, snapshot_root=interrupted_root)
    workbench.initialize(sequence, factors, "fixed")
    from sciworkbench.errors import ForcedTermination

    with pytest.raises(ForcedTermination):
        workbench.process(interrupt_after_commits=6)
    version_dir = sorted(
        (interrupted_root / sequence.sequence_hash[:16]).glob("v*")
    )[-1]
    block_path = version_dir / "blocks" / "000003.blk"
    raw = bytearray(block_path.read_bytes())
    raw[-80] ^= 0x01
    block_path.write_bytes(raw)

    recovery = Workbench(block_size=8, snapshot_root=interrupted_root)
    recovery.initialize(sequence, factors, "fixed")
    with pytest.raises(WorkbenchError) as caught:
        recovery.process(resume=True)
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.frame == 0
    assert caught.value.block == 3


def test_duplicate_submission_returns_busy(tmp_path):
    sequence, factors, _, _ = _large_sample(tmp_path)
    first = Workbench(block_size=8, snapshot_root=tmp_path / "busy_a")
    first.initialize(sequence, factors, "fixed")
    second = Workbench(block_size=8, snapshot_root=tmp_path / "busy_b")
    second.initialize(sequence, factors, "fixed")
    entered = threading.Event()
    release = threading.Event()

    def progress(payload):
        if payload["completed"] == 1:
            entered.set()
            release.wait(5.0)

    errors = []

    def run_first():
        try:
            first.process(progress_callback=progress)
        except BaseException as exc:
            errors.append(exc)

    thread = threading.Thread(target=run_first)
    thread.start()
    assert entered.wait(5.0)
    with pytest.raises(WorkbenchError) as caught:
        second.process()
    assert caught.value.code == E_BUSY
    release.set()
    thread.join(10.0)
    assert not errors
    assert second.process() is not None


def test_cancel_keeps_previous_result_and_releases_busy(tmp_path):
    sequence, factors, _, _ = _large_sample(tmp_path)
    workbench = Workbench(block_size=8, snapshot_root=tmp_path / "cancel")
    workbench.initialize(sequence, factors, "fixed")
    previous = workbench.process()
    modified = factors.replace("frac_bits", 4)
    workbench.set_factors(modified)
    cancel_event = threading.Event()

    def progress(payload):
        if payload["completed"] == 2:
            cancel_event.set()

    with pytest.raises(WorkbenchError) as caught:
        workbench.process(
            cancel_event=cancel_event, progress_callback=progress
        )
    assert caught.value.code == E_CANCELLED
    assert workbench.result is previous
    assert workbench.process() is not None
