from __future__ import annotations

import json
from dataclasses import replace
from pathlib import Path

import numpy as np
import pytest

from edgepipe.checkpoint import CheckpointStore
from edgepipe.config import STAGES, PipelineConfig
from edgepipe.engine import PipelineEngine, canonical_input_hash
from edgepipe.errors import EdgePipeError, ErrorCode
from edgepipe.ops import BlockGrid
from edgepipe.samples import decode_image, sample_image_path, truth_from_sample
from edgepipe.states import (
    COMMITTED,
    FAILED,
    PENDING,
    PROCESSING,
    BlockStageStateMachine,
)


def test_state_machine_legal_flow_and_atomic_illegal_rejection():
    machine = BlockStageStateMachine(1)
    machine.transition("b0000", "gradient", PROCESSING)
    machine.transition("b0000", "gradient", FAILED, error={"code": "X"})
    machine.transition("b0000", "gradient", PROCESSING)
    machine.transition("b0000", "gradient", COMMITTED)
    with pytest.raises(EdgePipeError) as caught:
        machine.transition("b0000", "gradient", PROCESSING)
    assert caught.value.code == ErrorCode.STATE
    assert machine.get("b0000", "gradient").state == COMMITTED


def test_uncommitted_rollback_is_explicit():
    machine = BlockStageStateMachine(1)
    machine.transition("b0000", "gradient", PROCESSING)
    machine.transition("b0000", "gradient", PENDING)
    assert machine.get("b0000", "gradient").state == PENDING


def test_single_block_failure_is_retryable_without_checkpoint_pollution(
    tmp_path: Path, first_sample, samples_dir: Path
):
    image = decode_image(sample_image_path(first_sample, samples_dir))
    config = PipelineConfig.from_mapping(first_sample["recommended_config"])
    checkpoint = tmp_path / "failure"
    injected = {"done": False}

    def fault(block_id: str, stage: str, attempt: int) -> None:
        if (
            block_id == "b0001"
            and stage == "gradient"
            and attempt == 1
            and not injected["done"]
        ):
            injected["done"] = True
            raise RuntimeError("injected block failure")

    first = PipelineEngine(config, checkpoint).run(
        image, truth=truth_from_sample(first_sample), mode="full", fault_injector=fault
    )
    assert not first.complete
    assert first.progress["committed"] == 1
    assert first.states["states"][0][0] == COMMITTED
    assert first.states["states"][1][0] == FAILED
    assert first.states["states"][2][0] == PENDING
    assert len(first.failed_tasks) == 1

    second = PipelineEngine(config, checkpoint).run(
        image, truth=truth_from_sample(first_sample), mode="resume"
    )
    baseline = PipelineEngine(config, tmp_path / "baseline").run(
        image, truth=truth_from_sample(first_sample), mode="full"
    )
    assert second.complete
    assert second.restored_commits == 1
    assert np.array_equal(second.final_mask, baseline.final_mask)
    assert all(
        row[0] == COMMITTED for row in second.states["states"]
    )


def _full_run(tmp_path: Path, sample, samples_dir: Path):
    image = decode_image(sample_image_path(sample, samples_dir))
    config = PipelineConfig.from_mapping(sample["recommended_config"])
    result = PipelineEngine(config, tmp_path / "cp").run(
        image, truth=truth_from_sample(sample), mode="full"
    )
    return image, config, result


def test_resume_after_graceful_stop_reuses_committed_prefix(
    tmp_path: Path, first_sample, samples_dir: Path
):
    image, config, partial = _full_run(tmp_path, first_sample, samples_dir)
    partial = PipelineEngine(config, tmp_path / "cp").run(
        image,
        truth=truth_from_sample(first_sample),
        mode="full",
        stop_after_commits=7,
    )
    assert not partial.complete
    assert partial.progress["committed"] == 7
    resumed = PipelineEngine(config, tmp_path / "cp").run(
        image, truth=truth_from_sample(first_sample), mode="resume"
    )
    assert resumed.complete
    assert resumed.restored_commits == 7
    assert resumed.newly_committed == resumed.progress["total"] - 7
    assert len(resumed.commit_sequence) == resumed.progress["total"]


def test_content_hash_mismatch_invalidates_entire_snapshot(
    tmp_path: Path, first_sample, samples_dir: Path
):
    image, config, baseline = _full_run(tmp_path, first_sample, samples_dir)
    root = tmp_path / "cp"
    manifest = json.loads((root / "snapshot.json").read_text(encoding="utf-8"))
    target = manifest["entries"][5]
    blob = root / target["path"]
    with np.load(str(blob), allow_pickle=False) as archive:
        mask = np.array(archive["mask"], copy=True)
        meta = np.array(archive["meta"], copy=True)
    flat = mask.reshape(-1)
    flat[0] = 1 if flat[0] == 0 else 0
    with open(blob, "wb") as handle:
        np.savez_compressed(handle, mask=mask, meta=meta)

    grid = BlockGrid.create(image.shape[0], image.shape[1], config.block_size)
    store = CheckpointStore(root)
    validation = store.validate_snapshot(
        expected_input_hash=canonical_input_hash(image),
        expected_params_version=config.params_version,
        expected_algorithm_version="edgepipe-1.0.0",
        grid=grid,
    )
    assert not validation.valid
    assert "内容哈希" in (validation.reason or "")
    rerun = PipelineEngine(config, root).run(
        image, truth=truth_from_sample(first_sample), mode="resume"
    )
    assert rerun.complete
    assert rerun.invalidated_reason is not None
    assert rerun.restored_commits == 0
    assert np.array_equal(rerun.final_mask, baseline.final_mask)


def test_parameter_version_mismatch_forces_full_recompute(
    tmp_path: Path, first_sample, samples_dir: Path
):
    image, config, baseline = _full_run(tmp_path, first_sample, samples_dir)
    changed = replace(config, high_threshold=config.high_threshold + 5.0)
    rerun = PipelineEngine(changed, tmp_path / "cp").run(
        image, truth=truth_from_sample(first_sample), mode="resume"
    )
    assert rerun.complete
    assert rerun.invalidated_reason is not None
    assert "参数版本" in rerun.invalidated_reason
    assert rerun.restored_commits == 0


def test_commit_store_rejects_out_of_order_writes(tmp_path: Path):
    grid = BlockGrid.create(64, 64, 32)
    store = CheckpointStore(tmp_path / "order")
    store.initialize(
        input_hash="x",
        params_version="p",
        algorithm_version="a",
        config={},
        grid=grid,
        total_commits=len(grid.blocks) * len(STAGES),
    )
    mask = np.zeros((32, 32), dtype=np.uint8)
    with pytest.raises(EdgePipeError) as caught:
        store.commit(
            block_id="b0001",
            block_index=1,
            stage="gradient",
            stage_index=0,
            mask=mask,
            params_version="p",
            algorithm_version="a",
        )
    assert caught.value.code == ErrorCode.STATE
    assert store.commit_count == 0

