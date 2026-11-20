from __future__ import annotations

import hashlib
import threading
import time
from pathlib import Path

import pytest

from streamforge import Pipeline
from streamforge.errors import BusinessError, ErrorCode
from streamforge.pipeline import load_artifacts_from_output
from streamforge.scheduler import RunControl
from tests.conftest import read_json


def _tree_hashes(root: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for path in sorted(item for item in root.rglob("*") if item.is_file()):
        result[str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result


def test_tracks_align_to_shared_timebase_and_chain_hash(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "output"
    result = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    assert result.status == "complete"
    assert not result.errors
    artifacts = load_artifacts_from_output(output, result.project)
    boundaries = result.timeline.boundaries
    for tier_id, slices in artifacts.items():
        assert len(slices) == len(boundaries)
        previous_hash = "0" * 64
        for expected_index, (artifact, boundary) in enumerate(zip(slices, boundaries)):
            assert artifact.index == expected_index
            assert artifact.start_ns == boundary.start_ns
            assert artifact.end_ns == boundary.end_ns
            assert artifact.audio_start_ns == boundary.start_ns
            assert artifact.video_start_ns == boundary.start_ns
            assert artifact.prev_hash == previous_hash
            assert artifact.audio_sample_start < artifact.audio_sample_end
            assert artifact.frames
            assert artifact.frames[0]["pts_ns"] == boundary.start_ns
            for frame, next_frame in zip(artifact.frames, artifact.frames[1:]):
                assert frame["pts_ns"] + frame["duration_ns"] == next_frame["pts_ns"]
            previous_hash = artifact.content_hash
        for previous, current in zip(slices, slices[1:]):
            assert previous.end_ns == current.start_ns
            assert previous.audio_sample_end == current.audio_sample_start
        assert result.scheduler.peak_bytes <= result.project.budget.total_memory_bytes
        for artifact in slices:
            assert (
                artifact.memory.estimated_bytes
                <= result.project.budget.single_slice_memory_bytes
            )


def test_memory_admission_and_degradation(config_factory, tmp_path: Path) -> None:
    config, budget = config_factory(
        budget_updates={
            "single_slice_memory_bytes": 300000,
            "total_memory_bytes": 900000,
        }
    )
    result = Pipeline(
        config_path=config, budget_path=budget, output_root=tmp_path / "out"
    ).run()
    assert result.status == "complete"
    assert any(event["event"] == "memory_degraded" for event in result.scheduler.events)
    assert all(
        sample["allocated_bytes"] <= 900000
        for sample in result.scheduler.memory_samples
    )
    for step in {event["step"] for event in result.scheduler.events}:
        active = [
            event
            for event in result.scheduler.events
            if event["event"] == "task_started" and event["step"] == step
        ]
        assert len(active) <= result.project.budget.global_parallelism
        started_tiers = [event["tier"] for event in active]
        assert len(started_tiers) == len(set(started_tiers))


def test_high_tier_starvation_warning_is_recorded(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory(
        budget_updates={
            "global_parallelism": 3,
            "initial_block_bytes": 4096,
            "min_block_bytes": 4096,
            "single_slice_memory_bytes": 320000,
            "starvation_ms": 0,
            "starvation_rounds": 1,
            "total_memory_bytes": 320000,
        }
    )
    result = Pipeline(
        config_path=config, budget_path=budget, output_root=tmp_path / "out"
    ).run()
    warnings = [
        event
        for event in result.scheduler.events
        if event["event"] == "starvation_warning"
    ]
    assert warnings
    assert warnings[0]["tier"] == "high"
    assert warnings[0]["wait_ms"] >= 0
    assert warnings[0]["gave_way_count"] >= 1
    assert result.scheduler.peak_bytes <= 320000


def test_budget_rejection_stops_tier_with_business_code(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory(
        budget_updates={"single_slice_memory_bytes": 50000}
    )
    output = tmp_path / "out"
    result = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    assert result.status == "failed"
    assert result.errors
    assert all(
        error["code"] == ErrorCode.BUDGET_EXCEEDED for error in result.errors
    )
    for error in result.errors:
        assert error["details"]["tier"] in {"low", "mid", "high"}
        assert error["details"]["slice_index"] == 0
    assert not list(output.glob("*/slice-*.fslice"))


def test_injected_failure_keeps_contiguous_prefix_only(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    result = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run(fault_at=("low", 1))
    assert result.status == "failed"
    assert any(
        error["code"] == ErrorCode.INJECTED_FAILURE for error in result.errors
    )
    low_files = sorted((output / "low").glob("slice-*.fslice"))
    assert [path.name for path in low_files] == ["slice-00000.fslice"]
    assert len(list((output / "mid").glob("slice-*.fslice"))) == 3
    assert len(list((output / "high").glob("slice-*.fslice"))) == 3
    assert not list(output.glob("*/.slice-*.tmp"))
    artifacts = load_artifacts_from_output(output, result.project)
    assert len(artifacts["low"]) == 1
    assert artifacts["low"][0].index == 0


def test_reuses_unaffected_tiers_after_parameter_change(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    first = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    first_hashes = {
        tier_id: [artifact.content_hash for artifact in artifacts]
        for tier_id, artifacts in first.artifacts.items()
    }
    second = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    assert set(second.reused_tiers) == {"low", "mid", "high"}
    assert second.recomputed_tiers == []
    assert {
        tier_id: [artifact.content_hash for artifact in artifacts]
        for tier_id, artifacts in second.artifacts.items()
    } == first_hashes

    changed_config, changed_budget = config_factory(
        tier_updates={"low": {"target_bitrate_kbps": 300}},
        config_version="demo-v1-changed-low",
    )
    third = Pipeline(
        config_path=changed_config,
        budget_path=changed_budget,
        output_root=output,
    ).run()
    third_hashes = {
        tier_id: [artifact.content_hash for artifact in artifacts]
        for tier_id, artifacts in third.artifacts.items()
    }
    assert "low" in third.recomputed_tiers
    assert set(third.reused_tiers) == {"mid", "high"}
    assert third_hashes["mid"] == first_hashes["mid"]
    assert third_hashes["high"] == first_hashes["high"]
    assert third_hashes["low"] != first_hashes["low"]


def test_byte_determinism_for_same_seed_and_operations(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    out_a = tmp_path / "a"
    out_b = tmp_path / "b"
    Pipeline(
        config_path=config, budget_path=budget, output_root=out_a
    ).run()
    Pipeline(
        config_path=config, budget_path=budget, output_root=out_b
    ).run()
    hashes_a = _tree_hashes(out_a)
    hashes_b = _tree_hashes(out_b)
    assert hashes_a == hashes_b


def test_busy_pause_cancel_and_temporary_file_release(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    control = RunControl()
    control.pause()
    pipeline = Pipeline(
        config_path=config,
        budget_path=budget,
        output_root=output,
        control=control,
    )
    box: dict[str, object] = {}

    def execute() -> None:
        box["result"] = pipeline.run()

    worker = threading.Thread(target=execute)
    worker.start()
    time.sleep(0.05)
    with pytest.raises(BusinessError) as caught:
        pipeline.run()
    assert caught.value.code == ErrorCode.BUSY
    control.cancel()
    control.resume()
    worker.join(timeout=5)
    assert not worker.is_alive()
    result = box["result"]
    assert result.status == "cancelled"
    assert not list(output.glob("*/slice-*.fslice"))
    assert not list(output.glob("*/.slice-*.tmp"))


def test_adding_tier_during_restore_keeps_original_budget(
    config_factory, tmp_path: Path
) -> None:
    config, budget = config_factory()
    output = tmp_path / "out"
    first = Pipeline(
        config_path=config, budget_path=budget, output_root=output
    ).run()
    extended_config, extended_budget = config_factory(
        config_version="demo-v1-added-tier",
        new_tier={
            "audio_sample_rate": 48000,
            "gain_db": 0.0,
            "id": "ultra",
            "target_bitrate_kbps": 2400,
            "video_qp": 20,
        },
    )
    second = Pipeline(
        config_path=extended_config,
        budget_path=extended_budget,
        output_root=output,
    ).run(restore_snapshot=first.snapshot_path)
    assert second.status == "complete"
    assert "ultra" in second.recomputed_tiers
    assert set(second.reused_tiers) == {"low", "mid", "high"}
    assert second.scheduler.peak_bytes <= first.project.budget.total_memory_bytes
    assert len(second.artifacts["ultra"]) == len(second.timeline.boundaries)
