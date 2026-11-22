"""Block pipeline with frozen commits, resumable checkpoints and diagnostics."""

from __future__ import annotations

import json
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .budget import choose_mode, force_mode_decision
from .checkpoint import (
    CheckpointIdentity,
    CheckpointStore,
    RecoveryState,
)
from .config import ALGORITHM_VERSION, Budget, QualityTolerance, SegmentationParams
from .errors import BusinessError
from .geometry import compare_masks, extract_regions
from .models import ComparisonReport, ModeDecision, RegionStat, Sample
from .segmentation import SegmentationResult, segment_image
from .state import BlockGrid, BlockStateMachine, STAGE_ORDER, STAGES, Stage
from .validation import array_hash


@dataclass
class PipelineResult:
    sample_id: str
    mode: str
    factor: int
    decision: ModeDecision
    initial_labels: np.ndarray
    final_labels: np.ndarray
    regions: list[RegionStat]
    comparison: ComparisonReport
    state_snapshot: dict[str, Any]
    checkpoint_dir: str
    complete: bool
    cancelled: bool
    reused_snapshot: bool
    checkpoint_invalidated: bool
    recovery_reason: str
    recovered_stages: int
    elapsed_seconds: float
    seeds: list[tuple[int, int]]
    seed_source: str
    timings: dict[str, float] = field(default_factory=dict)
    scale_metrics: dict[str, Any] | None = None

    def to_dict(self, include_contours: bool = False) -> dict[str, Any]:
        return {
            "sample_id": self.sample_id,
            "mode": self.mode,
            "factor": self.factor,
            "decision": self.decision.to_dict(),
            "regions": [
                region.to_dict(include_contour=include_contours)
                for region in self.regions
            ],
            "comparison": self.comparison.to_dict(),
            "state": self.state_snapshot,
            "checkpoint_dir": self.checkpoint_dir,
            "complete": self.complete,
            "cancelled": self.cancelled,
            "reused_snapshot": self.reused_snapshot,
            "checkpoint_invalidated": self.checkpoint_invalidated,
            "recovery_reason": self.recovery_reason,
            "recovered_stages": self.recovered_stages,
            "elapsed_seconds": self.elapsed_seconds,
            "seeds": [list(seed) for seed in self.seeds],
            "seed_source": self.seed_source,
            "timings": self.timings,
            "scale_metrics": self.scale_metrics,
            "algorithm_version": ALGORITHM_VERSION,
        }


def _block_payload(labels: np.ndarray, rect: tuple[int, int, int, int]) -> bytes:
    x0, y0, x1, y1 = rect
    return np.ascontiguousarray(labels[y0:y1, x0:x1]).tobytes()


def _stats_payload(
    labels: np.ndarray,
    rect: tuple[int, int, int, int],
    block_index: int,
) -> bytes:
    x0, y0, x1, y1 = rect
    block = labels[y0:y1, x0:x1]
    records = []
    for label in sorted(int(value) for value in np.unique(block) if value != 0):
        ys, xs = np.nonzero(block == label)
        records.append(
            {
                "label": label,
                "pixel_count": int(len(xs)),
                "sum_x": int(xs.sum() + len(xs) * x0),
                "sum_y": int(ys.sum() + len(ys) * y0),
                "bbox": [
                    int(xs.min() + x0),
                    int(ys.min() + y0),
                    int(xs.max() + x0),
                    int(ys.max() + y0),
                ],
            }
        )
    payload = {
        "block_index": block_index,
        "rect": [x0, y0, x1, y1],
        "regions": records,
    }
    return json.dumps(
        payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")


def _compute_block_payloads(
    initial: np.ndarray,
    final: np.ndarray,
    grid: BlockGrid,
    block_index: int,
    fail_injections: set[tuple[int, Stage]],
) -> dict[Stage, bytes | Exception]:
    rect = grid.rect(block_index)
    outputs: dict[Stage, bytes | Exception] = {}
    for stage in STAGES:
        if (block_index, stage) in fail_injections:
            outputs[stage] = RuntimeError(
                f"injected failure at block {block_index} stage {stage.value}"
            )
            continue
        if stage == Stage.GROWTH:
            outputs[stage] = _block_payload(initial, rect)
        elif stage == Stage.CLEANUP:
            outputs[stage] = _block_payload(final, rect)
        else:
            outputs[stage] = _stats_payload(final, rect, block_index)
    return outputs


def _assemble_labels(
    grid: BlockGrid,
    records: dict[tuple[int, Stage], bytes],
    stage: Stage,
    dtype: np.dtype,
) -> np.ndarray:
    assembled = np.zeros((grid.height, grid.width), dtype=dtype)
    for block_index in range(grid.count):
        x0, y0, x1, y1 = grid.rect(block_index)
        payload = records[(block_index, stage)]
        expected = (y1 - y0) * (x1 - x0) * np.dtype(dtype).itemsize
        if len(payload) != expected:
            raise BusinessError(
                "E_STATE",
                "检查点块尺寸与网格不一致",
                details={
                    "block_index": block_index,
                    "stage": stage.value,
                    "expected_bytes": expected,
                    "actual_bytes": len(payload),
                },
            )
        assembled[y0:y1, x0:x1] = np.frombuffer(payload, dtype=dtype).reshape(
            y1 - y0, x1 - x0
        )
    return assembled


def _normalize_failures(
    fail_injections: set[int] | set[tuple[int, Stage]] | None,
) -> set[tuple[int, Stage]]:
    if not fail_injections:
        return set()
    normalized: set[tuple[int, Stage]] = set()
    for item in fail_injections:
        if isinstance(item, tuple):
            normalized.add((int(item[0]), Stage(item[1])))
        else:
            normalized.add((int(item), Stage.GROWTH))
    return normalized


class SegmentationPipeline:
    def __init__(
        self,
        checkpoint_root: str | Path,
        *,
        quality_tolerance: QualityTolerance | None = None,
    ) -> None:
        self.checkpoint_root = Path(checkpoint_root).resolve()
        self.checkpoint_root.mkdir(parents=True, exist_ok=True)
        self.quality_tolerance = quality_tolerance

    def run(
        self,
        sample: Sample,
        params: SegmentationParams,
        budget: Budget,
        *,
        force_mode: str | None = None,
        force_factor: int | None = None,
        fail_injections: set[int] | set[tuple[int, Stage]] | None = None,
        cancel_event: threading.Event | None = None,
        pause_event: threading.Event | None = None,
        status_callback: Callable[[dict[str, Any]], None] | None = None,
        state_callback: Callable[[BlockStateMachine], None] | None = None,
    ) -> PipelineResult:
        started = time.perf_counter()
        params.validate(image_shape=sample.image.shape[:2])
        budget.validate()
        decision = choose_mode(
            sample.width, sample.height, sample.region_count, budget
        )
        if force_mode is not None:
            decision = force_mode_decision(
                decision,
                mode=force_mode,
                factor=force_factor or 2,
                label="reproducible experiment",
            )
        grid = BlockGrid(sample.width, sample.height, decision.block_size)
        identity = CheckpointIdentity(
            sample_id=sample.sample_id,
            image_hash=array_hash(sample.image),
            parameter_version=params.version(),
            algorithm_version=ALGORITHM_VERSION,
            budget_version=budget.version(),
            mode=decision.mode,
            factor=decision.downsample_factor,
            block_size=decision.block_size,
            block_count=grid.count,
        )
        store = CheckpointStore(
            self.checkpoint_root,
            identity,
            block_buffer_mb=budget.block_buffer_mb,
        )
        recovery = store.open()
        state = BlockStateMachine(grid)
        if state_callback is not None:
            state_callback(state)
        failures = _normalize_failures(fail_injections)
        cancelled = False
        status_callback = status_callback or (lambda _event: None)
        reused_snapshot = recovery.reused
        recovered_stages = 0

        def emit(event: str, **payload: Any) -> None:
            status_callback(
                {
                    "event": event,
                    **payload,
                    "state": state.snapshot(),
                    "decisions": decision.to_dict(),
                }
            )

        emit(
            "snapshot_opened",
            reused=recovery.reused,
            invalidated=recovery.invalidated,
            reason=recovery.reason,
            record_count=recovery.record_count,
        )

        complete_recovery = recovery.record_count == grid.stage_count and all(
            (block_index, stage) in recovery.records
            for block_index in range(grid.count)
            for stage in STAGES
        )
        if complete_recovery and recovery.record_count:
            payloads = {
                key: record.payload for key, record in recovery.records.items()
            }
            try:
                initial = _assemble_labels(
                    grid, payloads, Stage.GROWTH, np.dtype(np.uint16)
                )
                final = _assemble_labels(
                    grid, payloads, Stage.CLEANUP, np.dtype(np.uint16)
                )
            except BusinessError:
                recovery = store.invalidate_all("完整检查点块尺寸校验失败")
                reused_snapshot = False
            else:
                for block_index in range(grid.count):
                    for stage in STAGES:
                        record = recovery.records[(block_index, stage)]
                        state.restore_committed(
                            block_index,
                            stage,
                            record.payload,
                            record.payload_hash,
                            sequence=record.sequence,
                        )
                        recovered_stages += 1
                comparison = compare_masks(
                    final,
                    sample.truth_mask,
                    tolerance=self.quality_tolerance,
                )
                elapsed = time.perf_counter() - started
                emit("complete", reused=True, recovered_stages=recovered_stages)
                return PipelineResult(
                    sample_id=sample.sample_id,
                    mode=decision.mode,
                    factor=decision.downsample_factor,
                    decision=decision,
                    initial_labels=initial,
                    final_labels=final,
                    regions=extract_regions(final, include_contours=True),
                    comparison=comparison,
                    state_snapshot=state.snapshot(),
                    checkpoint_dir=str(store.snapshot_dir),
                    complete=True,
                    cancelled=False,
                    reused_snapshot=True,
                    checkpoint_invalidated=False,
                    recovery_reason=recovery.reason,
                    recovered_stages=recovered_stages,
                    elapsed_seconds=elapsed,
                    seeds=[],
                    seed_source="checkpoint",
                    timings={
                        "recovery_seconds": elapsed,
                        "segmentation_seconds": 0.0,
                        "block_commit_seconds": 0.0,
                    },
                )

        segmentation = segment_image(
            sample.image,
            params,
            mode=decision.mode,
            factor=decision.downsample_factor,
        )
        emit(
            "segmentation_complete",
            mode=decision.mode,
            factor=decision.downsample_factor,
            seconds=segmentation.elapsed_seconds,
            seeds=[list(seed) for seed in segmentation.seeds],
        )

        expected_payloads: dict[int, dict[Stage, bytes | Exception]] = {}
        commit_started = time.perf_counter()
        workers = max(1, min(budget.max_workers, grid.count))
        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="artmask") as pool:
            futures = {
                pool.submit(
                    _compute_block_payloads,
                    segmentation.initial_labels,
                    segmentation.final_labels,
                    grid,
                    block_index,
                    failures,
                ): block_index
                for block_index in range(grid.count)
            }
            for future in as_completed(futures):
                block_index = futures[future]
                try:
                    expected_payloads[block_index] = future.result()
                except Exception as exc:  # pragma: no cover - defensive isolation
                    expected_payloads[block_index] = {
                        stage: RuntimeError(str(exc)) for stage in STAGES
                    }

        restored_ok = True
        if recovery.records:
            for block_index in range(grid.count):
                for stage in STAGES:
                    key = (block_index, stage)
                    record = recovery.records.get(key)
                    if record is None:
                        continue
                    expected = expected_payloads[block_index].get(stage)
                    if isinstance(expected, Exception) or expected != record.payload:
                        restored_ok = False
                        break
                if not restored_ok:
                    break
        if not restored_ok:
            recovery = store.invalidate_all(
                "重算结果与已持久化阶段不一致，整体作废并全量重算"
            )
            reused_snapshot = False
            recovered_stages = 0
            emit("snapshot_invalidated", reason=recovery.reason)
        else:
            for block_index in range(grid.count):
                for stage in STAGES:
                    record = recovery.records.get((block_index, stage))
                    if record is None:
                        continue
                    state.restore_committed(
                        block_index,
                        stage,
                        record.payload,
                        record.payload_hash,
                        sequence=record.sequence,
                    )
                    recovered_stages += 1

        for block_index in range(grid.count):
            if cancel_event is not None and cancel_event.is_set():
                cancelled = True
                break
            for stage in STAGES:
                key = (block_index, stage)
                if state.state(block_index, stage).value == "committed":
                    continue
                if cancel_event is not None and cancel_event.is_set():
                    cancelled = True
                    break
                while pause_event is not None and pause_event.is_set():
                    if cancel_event is not None and cancel_event.is_set():
                        break
                    time.sleep(0.05)
                if cancel_event is not None and cancel_event.is_set():
                    cancelled = True
                    break
                worker_id = f"worker-{block_index % workers}"
                state.begin(block_index, stage, worker_id)
                emit(
                    "stage_processing",
                    block_index=block_index,
                    stage=stage.value,
                    worker_id=worker_id,
                )
                output = expected_payloads[block_index][stage]
                if isinstance(output, Exception):
                    state.fail(block_index, stage, str(output))
                    emit(
                        "stage_failed",
                        block_index=block_index,
                        stage=stage.value,
                        error=str(output),
                    )
                    break
                try:
                    payload_hash, sequence = store.commit(
                        block_index,
                        stage,
                        output,
                        worker_id=worker_id,
                    )
                    state.commit(
                        block_index,
                        stage,
                        output,
                        payload_hash,
                        worker_id=worker_id,
                        sequence=sequence,
                    )
                except Exception as exc:
                    state.fail(block_index, stage, str(exc))
                    emit(
                        "stage_failed",
                        block_index=block_index,
                        stage=stage.value,
                        error=str(exc),
                    )
                    break
                emit(
                    "stage_committed",
                    block_index=block_index,
                    stage=stage.value,
                    sequence=sequence,
                )
            if cancelled:
                break

        complete = state.is_complete()
        if complete:
            payloads = {
                key: record.payload
                for key, record in (
                    (
                        (block_index, stage),
                        state.record(block_index, stage),
                    )
                    for block_index in range(grid.count)
                    for stage in STAGES
                )
                if record is not None
            }
            initial = _assemble_labels(
                grid, payloads, Stage.GROWTH, np.dtype(np.uint16)
            )
            final = _assemble_labels(
                grid, payloads, Stage.CLEANUP, np.dtype(np.uint16)
            )
        else:
            initial = segmentation.initial_labels
            final = segmentation.final_labels
            if cancel_event is not None and cancel_event.is_set():
                state.mark_cancelled_processing()
        regions = extract_regions(final, include_contours=True)
        comparison = compare_masks(
            final,
            sample.truth_mask,
            tolerance=self.quality_tolerance,
        )
        elapsed = time.perf_counter() - started
        commit_seconds = time.perf_counter() - commit_started
        emit("pipeline_finished", complete=complete, cancelled=cancelled)
        return PipelineResult(
            sample_id=sample.sample_id,
            mode=decision.mode,
            factor=decision.downsample_factor,
            decision=decision,
            initial_labels=initial,
            final_labels=final,
            regions=regions,
            comparison=comparison,
            state_snapshot=state.snapshot(),
            checkpoint_dir=str(store.snapshot_dir),
            complete=complete,
            cancelled=cancelled,
            reused_snapshot=reused_snapshot,
            checkpoint_invalidated=recovery.invalidated,
            recovery_reason=recovery.reason,
            recovered_stages=recovered_stages,
            elapsed_seconds=elapsed,
            seeds=segmentation.seeds,
            seed_source=segmentation.seed_source,
            timings={
                "segmentation_seconds": segmentation.elapsed_seconds,
                "block_commit_seconds": commit_seconds,
                "total_seconds": elapsed,
            },
        )


def export_artifacts(
    result: PipelineResult,
    export_dir: str | Path,
    *,
    image: np.ndarray | None = None,
    truth_mask: np.ndarray | None = None,
) -> dict[str, str]:
    from .imageops import deviation_heatmap, overlay_labels

    export_dir = Path(export_dir).resolve()
    export_dir.mkdir(parents=True, exist_ok=True)
    stem = f"{result.sample_id}_{result.mode}_f{result.factor}"
    mask_path = export_dir / f"{stem}_mask.npy"
    initial_path = export_dir / f"{stem}_initial.npy"
    report_path = export_dir / f"{stem}_report.json"
    np.save(mask_path, result.final_labels, allow_pickle=False)
    np.save(initial_path, result.initial_labels, allow_pickle=False)
    paths = {
        "mask": str(mask_path),
        "initial_mask": str(initial_path),
        "report": str(report_path),
    }
    if image is not None:
        overlay = overlay_labels(image, result.final_labels)
        overlay_path = export_dir / f"{stem}_overlay.npy"
        np.save(overlay_path, overlay, allow_pickle=False)
        paths["overlay"] = str(overlay_path)
        errors = {
            item.predicted_region_id: item.centroid_error_px
            for item in result.comparison.regions
        }
        heatmap = deviation_heatmap(
            result.final_labels,
            truth_mask if truth_mask is not None else result.final_labels,
            errors,
        )
        heatmap_path = export_dir / f"{stem}_heatmap.npy"
        np.save(heatmap_path, heatmap, allow_pickle=False)
        paths["heatmap"] = str(heatmap_path)
    report = result.to_dict()
    report["complete"] = result.complete
    with report_path.open("w", encoding="utf-8") as handle:
        json.dump(report, handle, ensure_ascii=False, indent=2, sort_keys=True)
    return paths
