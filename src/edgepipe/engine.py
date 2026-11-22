"""Chunked stage engine with resume, deterministic commits and crash boundary."""

from __future__ import annotations

import hashlib
import math
import threading
import time
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Dict, List, Mapping, Optional, Sequence, Tuple

import numpy as np

from .checkpoint import CheckpointStore, SnapshotValidation
from .config import ALGORITHM_VERSION, STAGES, PipelineConfig
from .errors import EdgePipeError, ErrorCode
from .geometry import geometric_stats
from .ops import (
    BlockGrid,
    assemble_stage_mask,
    process_block_stage,
    run_whole_pipeline,
)
from .states import (
    COMMITTED,
    FAILED,
    PENDING,
    PROCESSING,
    BlockStageStateMachine,
    state_matrix_with_headers,
)

RUN_MODES = ("full", "resume", "incremental")


class SimulatedCrash(Exception):
    """Raised in tests to emulate abrupt termination after a commit."""


class CancellationToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()


@dataclass
class PipelineResult:
    mode: str
    config: PipelineConfig
    final_mask: Optional[np.ndarray]
    stage_masks: Dict[str, np.ndarray]
    complete: bool
    cancelled: bool
    progress: Dict[str, Any]
    states: Dict[str, Any]
    commit_sequence: List[Dict[str, Any]]
    restored_commits: int
    newly_committed: int
    validation: SnapshotValidation
    failed_tasks: List[Dict[str, Any]]
    geometry: Optional[Dict[str, Any]]
    elapsed_seconds: float
    snapshot_path: str
    invalidated_reason: Optional[str] = None

    def summary(self) -> Dict[str, Any]:
        return {
            "mode": self.mode,
            "complete": self.complete,
            "cancelled": self.cancelled,
            "progress": self.progress,
            "committed": self.progress["committed"],
            "total": self.progress["total"],
            "restored_commits": self.restored_commits,
            "newly_committed": self.newly_committed,
            "snapshot_validation": self.validation.status,
            "invalidated_reason": self.invalidated_reason,
            "failed_tasks": self.failed_tasks,
            "geometry": self.geometry,
            "elapsed_seconds": round(self.elapsed_seconds, 6),
            "snapshot_path": self.snapshot_path,
            "config": self.config.to_dict(),
            "algorithm_version": ALGORITHM_VERSION,
            "commit_sequence": self.commit_sequence,
            "states": self.states,
        }


def canonical_input_hash(image: np.ndarray) -> str:
    array = np.ascontiguousarray(image)
    digest = hashlib.sha256()
    digest.update(f"{array.shape}:{array.dtype}".encode("ascii"))
    digest.update(array.tobytes(order="C"))
    return digest.hexdigest()


class PipelineEngine:
    def __init__(
        self,
        config: PipelineConfig,
        checkpoint_dir: str | Path,
        *,
        store: Optional[CheckpointStore] = None,
    ) -> None:
        self.config = config
        self.store = store or CheckpointStore(checkpoint_dir)

    def run(
        self,
        image: np.ndarray,
        *,
        truth: Optional[Mapping[str, Any]] = None,
        mode: str = "full",
        cancel_token: Optional[CancellationToken] = None,
        fault_injector: Optional[
            Callable[[str, str, int], None]
        ] = None,
        stop_after_commits: Optional[int] = None,
        crash_after_commits: Optional[int] = None,
        crash_hook: Optional[Callable[[int], None]] = None,
        progress_callback: Optional[Callable[[Dict[str, Any]], None]] = None,
    ) -> PipelineResult:
        started = time.monotonic()
        if mode not in RUN_MODES:
            raise EdgePipeError(
                ErrorCode.PARA,
                f"mode 必须为 {RUN_MODES} 之一",
                field="mode",
                details={"mode": mode},
            )
        self.config.validate_image(image)
        grid = BlockGrid.create(
            image.shape[0], image.shape[1], self.config.block_size
        )
        total_commits = len(grid.blocks) * len(STAGES)
        image_hash = canonical_input_hash(image)
        validation = self._prepare_store(
            mode=mode,
            image_hash=image_hash,
            grid=grid,
            total_commits=total_commits,
        )
        invalidated_reason = (
            validation.reason
            if validation.status == "invalid"
            else None
        )
        if not validation.valid:
            self.store.reset()
            self.store.initialize(
                input_hash=image_hash,
                params_version=self.config.params_version,
                algorithm_version=ALGORITHM_VERSION,
                config=self.config.to_dict(),
                grid=grid,
                total_commits=total_commits,
            )
            entries: List[Dict[str, Any]] = []
        else:
            entries = validation.entries

        machine = BlockStageStateMachine(len(grid.blocks), STAGES)
        machine.restore_committed(
            ((entry["block_id"], entry["stage"]) for entry in entries)
        )

        stage_cores: Dict[str, Dict[str, np.ndarray]] = {
            stage: {} for stage in STAGES
        }
        for entry in entries:
            stage_cores[entry["stage"]][entry["block_id"]] = (
                self.store.load_entry_mask(entry)
            )

        restored_commits = len(entries)
        newly_committed = 0
        stage_masks: Dict[str, np.ndarray] = {}
        failed_tasks: List[Dict[str, Any]] = []
        cancelled = False
        complete = restored_commits == total_commits

        for stage_index, stage in enumerate(STAGES):
            if stage_index == 0:
                source = image
            else:
                previous_stage = STAGES[stage_index - 1]
                if previous_stage not in stage_masks:
                    break
                source = stage_masks[previous_stage]

            cores = stage_cores[stage]
            pending_blocks = [
                block for block in grid.blocks if block.block_id not in cores
            ]
            stop_stage = False
            batch_size = max(1, self.config.max_workers)
            for start in range(0, len(pending_blocks), batch_size):
                if cancel_token is not None and cancel_token.cancelled:
                    cancelled = True
                    stop_stage = True
                    break
                batch = pending_blocks[start : start + batch_size]
                futures: Dict[str, Future] = {}
                with ThreadPoolExecutor(
                    max_workers=min(batch_size, len(batch)),
                    thread_name_prefix=f"edgepipe-{stage}",
                ) as executor:
                    for block in batch:
                        task = machine.transition(block.block_id, stage, PROCESSING)
                        futures[block.block_id] = executor.submit(
                            process_block_stage,
                            stage,
                            source,
                            block,
                            self.config,
                        )

                    for block in batch:
                        future = futures[block.block_id]
                        try:
                            core = future.result()
                            if fault_injector is not None:
                                attempt = machine.get(block.block_id, stage).attempts
                                fault_injector(block.block_id, stage, attempt)
                        except Exception as exc:
                            error = _error_dict(exc)
                            task = machine.transition(
                                block.block_id, stage, FAILED, error=error
                            )
                            failed_tasks.append(task.to_dict())
                            cancelled = cancelled or error["code"] == ErrorCode.CANCELLED
                            stop_stage = True
                            break

                        if cancel_token is not None and cancel_token.cancelled:
                            error = {
                                "code": ErrorCode.CANCELLED,
                                "message": "用户取消，未提交当前任务",
                            }
                            task = machine.transition(
                                block.block_id, stage, FAILED, error=error
                            )
                            failed_tasks.append(task.to_dict())
                            cancelled = True
                            stop_stage = True
                            break

                        entry = self.store.commit(
                            block_id=block.block_id,
                            block_index=block.index,
                            stage=stage,
                            stage_index=stage_index,
                            mask=core,
                            params_version=self.config.params_version,
                            algorithm_version=ALGORITHM_VERSION,
                        )
                        machine.transition(block.block_id, stage, COMMITTED)
                        cores[block.block_id] = core
                        newly_committed += 1
                        if progress_callback is not None:
                            progress_callback(
                                {
                                    **machine.progress,
                                    "block_id": block.block_id,
                                    "stage": stage,
                                    "commit_seq": entry["commit_seq"],
                                }
                            )
                        if (
                            crash_after_commits is not None
                            and newly_committed >= crash_after_commits
                        ):
                            if crash_hook is not None:
                                crash_hook(newly_committed)
                            raise SimulatedCrash(
                                f"模拟强制终止于第 {newly_committed} 个新提交"
                            )
                        if (
                            stop_after_commits is not None
                            and newly_committed >= stop_after_commits
                        ):
                            stop_stage = True
                            break

                if stop_stage:
                    # In-flight futures in this batch are complete. Tasks that
                    # were not committed roll back to PENDING; only the task
                    # that actually raised remains FAILED.
                    for block in batch:
                        task = machine.get(block.block_id, stage)
                        if task.state == PROCESSING:
                            machine.transition(block.block_id, stage, PENDING)
                    break

            if stop_stage:
                cancelled = cancelled or (
                    cancel_token is not None and cancel_token.cancelled
                )
                break

            if len(cores) == len(grid.blocks):
                channel_shape = image.shape[2:] if image.ndim == 3 else ()
                stage_masks[stage] = assemble_stage_mask(
                    grid, channel_shape, cores
                )
            else:
                break

        complete = machine.committed_count == machine.total
        final_mask = stage_masks.get("prune")
        geometry = None
        if complete and final_mask is not None and truth is not None:
            geometry = geometric_stats(final_mask, truth)
        elapsed = time.monotonic() - started
        return PipelineResult(
            mode=mode,
            config=self.config,
            final_mask=final_mask,
            stage_masks=stage_masks,
            complete=complete,
            cancelled=cancelled,
            progress=machine.progress,
            states=state_matrix_with_headers(machine, len(grid.blocks)),
            commit_sequence=self.store.canonical_commit_sequence(),
            restored_commits=restored_commits,
            newly_committed=newly_committed,
            validation=validation,
            failed_tasks=failed_tasks,
            geometry=geometry,
            elapsed_seconds=elapsed,
            snapshot_path=str(self.store.root),
            invalidated_reason=invalidated_reason,
        )

    def _prepare_store(
        self,
        *,
        mode: str,
        image_hash: str,
        grid: BlockGrid,
        total_commits: int,
    ) -> SnapshotValidation:
        if mode == "full":
            return SnapshotValidation("missing", "全量模式清空旧检查点")
        validation = self.store.validate_snapshot(
            expected_input_hash=image_hash,
            expected_params_version=self.config.params_version,
            expected_algorithm_version=ALGORITHM_VERSION,
            grid=grid,
        )
        if validation.valid:
            return validation
        return validation

    @staticmethod
    def whole_reference(
        image: np.ndarray,
        config: PipelineConfig,
        truth: Optional[Mapping[str, Any]] = None,
    ) -> Tuple[np.ndarray, Dict[str, np.ndarray], Optional[Dict[str, Any]]]:
        final_mask, stage_masks = run_whole_pipeline(image, config)
        geometry = geometric_stats(final_mask, truth) if truth is not None else None
        return final_mask, stage_masks, geometry


def _error_dict(exc: BaseException) -> Dict[str, Any]:
    if isinstance(exc, EdgePipeError):
        return exc.to_dict()
    return {
        "code": ErrorCode.INTERNAL,
        "message": f"{type(exc).__name__}: {exc}",
    }
