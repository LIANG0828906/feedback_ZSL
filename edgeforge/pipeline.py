from __future__ import annotations

from collections import deque
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from dataclasses import dataclass
import os
from pathlib import Path
import threading
import time
import traceback
from typing import Any, Callable, Iterable

import numpy as np

from .algorithms import apply_stage
from .blocks import Block, expanded_region, make_blocks
from .checkpoint import CheckpointStore, build_run_meta
from .config import PipelineConfig
from .errors import (
    BandError,
    BudgetError,
    CancelledError,
    CheckpointError,
    EdgeForgeError,
    PipelineFailure,
)
from .loader import LoadedSample
from .metrics import compute_all_metrics
from .resource import MemoryBudget, RunControl
from .stages import STAGE_ORDER, BlockState, BlockStateMachine, Stage


EventCallback = Callable[[dict[str, Any]], None]


@dataclass
class BlockTaskOutcome:
    block: Block
    array: np.ndarray | None
    error: dict[str, Any] | None
    elapsed_seconds: float


class Pipeline:
    def __init__(
        self,
        sample: LoadedSample,
        config: PipelineConfig,
        *,
        run_dir: str | Path | None = None,
        execution_mode: str = "parallel",
        control: RunControl | None = None,
        on_event: EventCallback | None = None,
        fault_once: Iterable[tuple[Stage, int]] | None = None,
        reset_checkpoint: bool = False,
    ) -> None:
        self.sample = sample
        self.config = config.validate()
        if execution_mode not in {"serial", "parallel"}:
            self.config._para(
                "execution_mode",
                "执行模式必须是 serial 或 parallel",
            )
        self.execution_mode = execution_mode
        self.control = control or RunControl()
        self.on_event = on_event
        self.blocks = make_blocks(sample.height, sample.width, self.config.tile_size)
        self.block_by_id = {block.block_id: block for block in self.blocks}
        self.state_machine = BlockStateMachine(
            tuple(block.block_id for block in self.blocks)
        )
        self.stage_arrays: dict[Stage, np.ndarray] = {}
        self.stage_metrics: dict[str, dict[str, Any]] = {}
        self.block_outputs: dict[tuple[Stage, int], np.ndarray] = {}
        self.failures: list[dict[str, Any]] = []
        self.queue_events: list[dict[str, Any]] = []
        self._queue_started: dict[tuple[Stage, int], float] = {}
        self._fault_lock = threading.Lock()
        self._fault_once = set(fault_once or ())
        self.checkpoint: CheckpointStore | None = None
        self.snapshot_invalidated = False
        self.snapshot_invalidation_reason: str | None = None
        self.resumed_commits = 0
        self.budget_peak_bytes = 0
        self.run_dir = Path(run_dir) if run_dir is not None else None
        self._last_stage_limit: int | None = None
        if self.run_dir is not None:
            self._open_checkpoint(reset_checkpoint)

    def _open_checkpoint(self, reset: bool) -> None:
        if self.run_dir is None:
            return
        if reset and self.run_dir.exists():
            stamp = time.strftime("%Y%m%dT%H%M%S", time.gmtime())
            retired = self.run_dir.with_name(
                f"{self.run_dir.name}.reset-{stamp}-{time.time_ns()}"
            )
            try:
                os.replace(self.run_dir, retired)
            except OSError as exc:
                raise CheckpointError(
                    "无法作废旧检查点目录",
                    details={"path": str(self.run_dir), "reason": str(exc)},
                ) from exc
        meta = build_run_meta(
            sample_id=self.sample.sample_id,
            image_sha256=self.sample.image_sha256,
            truth_sha256=self.sample.truth_sha256,
            width=self.sample.width,
            height=self.sample.height,
            channels=self.sample.channels,
            tile_size=self.config.tile_size,
            params_version=self.config.params_version(),
        )
        self.checkpoint = CheckpointStore(self.run_dir, meta)
        if self.checkpoint.invalidated:
            self.snapshot_invalidated = True
            self.snapshot_invalidation_reason = self.checkpoint.invalidation_reason
            self._emit(
                {
                    "type": "snapshot_invalidated",
                    "reason": self.checkpoint.invalidation_reason,
                    "discarded_path": (
                        None
                        if self.checkpoint.invalidated_path is None
                        else str(self.checkpoint.invalidated_path)
                    ),
                }
            )
            return
        self._restore_checkpoint_records()

    def _restore_checkpoint_records(self) -> None:
        assert self.checkpoint is not None
        records = self.checkpoint.records
        if not records:
            return
        expected_shape = self.sample.image.shape
        expected_dtype = {
            Stage.GRADIENT: np.dtype(np.float32),
            Stage.THRESHOLD: np.dtype(np.uint8),
            Stage.OPEN: np.dtype(np.uint8),
            Stage.CLOSE: np.dtype(np.uint8),
            Stage.THIN: np.dtype(np.uint8),
            Stage.PRUNE: np.dtype(np.uint8),
        }
        for (stage, block_id), array in records.items():
            if block_id not in self.block_by_id:
                self._invalidate_restored_snapshot("record_block_unknown")
                return
            block = self.block_by_id[block_id]
            expected_block_shape = (
                block.height,
                block.width,
            ) + (() if self.sample.channels == 1 else (self.sample.channels,))
            if array.shape != expected_block_shape:
                self._invalidate_restored_snapshot("record_shape_mismatch")
                return
            if array.dtype != expected_dtype[stage]:
                self._invalidate_restored_snapshot("record_dtype_mismatch")
                return
            if array.shape[:2] != expected_block_shape[:2]:
                self._invalidate_restored_snapshot("record_shape_mismatch")
                return

        committed_by_stage: dict[Stage, set[int]] = {
            stage: set() for stage in STAGE_ORDER
        }
        for stage, block_id in records:
            committed_by_stage[stage].add(block_id)
        all_blocks = set(self.block_by_id)
        blocked_later_stage = False
        previous_complete = True
        for stage in STAGE_ORDER:
            if committed_by_stage[stage] and not previous_complete:
                blocked_later_stage = True
                break
            previous_complete = committed_by_stage[stage] == all_blocks
        if blocked_later_stage:
            self._invalidate_restored_snapshot("record_stage_order_violation")
            return

        for (stage, block_id), array in records.items():
            self.block_outputs[(stage, block_id)] = np.array(array, copy=True)
            self.state_machine.restore_committed(stage, block_id)
            self.resumed_commits += 1
        for stage in STAGE_ORDER:
            if committed_by_stage[stage] == all_blocks:
                assembled = self._assemble_stage(stage)
                if assembled is None:
                    self._invalidate_restored_snapshot("record_assembly_failed")
                    return
                self.stage_arrays[stage] = assembled
        self.stage_metrics = compute_all_metrics(
            self.stage_arrays,
            self.sample.truth_mask,
            self.sample.lines,
            self.sample.circles,
            self.config,
        )

    def _invalidate_restored_snapshot(self, reason: str) -> None:
        assert self.checkpoint is not None
        self.checkpoint.invalidate_snapshot(reason)
        self.snapshot_invalidated = True
        self.snapshot_invalidation_reason = reason
        self.block_outputs.clear()
        self.stage_arrays.clear()
        self.stage_metrics.clear()
        self.resumed_commits = 0
        ids = tuple(block.block_id for block in self.blocks)
        self.state_machine = BlockStateMachine(ids)
        self._emit(
            {
                "type": "snapshot_invalidated",
                "reason": reason,
                "discarded_path": (
                    None
                    if self.checkpoint.invalidated_path is None
                    else str(self.checkpoint.invalidated_path)
                ),
            }
        )

    def _emit(self, event: dict[str, Any]) -> None:
        event.setdefault("progress", self.progress)
        if self.on_event is not None:
            self.on_event(event)

    @property
    def progress(self) -> dict[str, Any]:
        snapshot = self.state_machine.snapshot()
        return {
            "committed": snapshot.committed,
            "total": snapshot.total,
            "ratio": snapshot.progress,
        }

    def run(
        self,
        *,
        retry_failed: bool = False,
        stages_limit: int | None = None,
    ) -> "Pipeline":
        if stages_limit is not None and (stages_limit < 1 or stages_limit > len(STAGE_ORDER)):
            self.config._para("stages_limit", "阶段数必须位于 1..6")
        self.control.check()
        self._last_stage_limit = stages_limit
        target_stages = STAGE_ORDER[:stages_limit]
        self._normalize_processing_states()
        for stage_index, stage in enumerate(target_stages):
            if any(previous not in self.stage_arrays for previous in STAGE_ORDER[:stage_index]):
                raise PipelineFailure(
                    "前置阶段尚未完整提交，不能推进当前阶段",
                    stage=stage.value,
                    failures=self.failures,
                )
            committed = {
                block.block_id
                for block in self.blocks
                if self.state_machine.cell(stage, block.block_id).state
                is BlockState.COMMITTED
            }
            if len(committed) == len(self.blocks):
                if stage not in self.stage_arrays:
                    assembled = self._assemble_stage(stage)
                    if assembled is None:
                        raise PipelineFailure(
                            "已提交分块无法组装完整阶段",
                            stage=stage.value,
                        )
                    self.stage_arrays[stage] = assembled
                self._ensure_stage_metrics(stage)
                continue
            source = (
                self.sample.image
                if stage is Stage.GRADIENT
                else self.stage_arrays[STAGE_ORDER[stage_index - 1]]
            )
            failed_in_stage = [
                block.block_id
                for block in self.blocks
                if self.state_machine.cell(stage, block.block_id).state
                is BlockState.FAILED
            ]
            if failed_in_stage and not retry_failed:
                failures = [
                    {
                        "stage": stage.value,
                        "block_id": block_id,
                        "message": self.state_machine.cell(stage, block_id).error,
                    }
                    for block_id in failed_in_stage
                ]
                raise PipelineFailure(
                    "存在失败的分块阶段；请使用重试或复位",
                    stage=stage.value,
                    failures=failures,
                )
            self._run_stage(stage, source, retry_failed=retry_failed)
            if len(
                {
                    block.block_id
                    for block in self.blocks
                    if self.state_machine.cell(stage, block.block_id).state
                    is BlockState.COMMITTED
                }
            ) != len(self.blocks):
                raise PipelineFailure(
                    "阶段未能完整提交",
                    stage=stage.value,
                    failures=self.failures,
                )
            assembled = self._assemble_stage(stage)
            if assembled is None:
                raise PipelineFailure("阶段提交结果不完整", stage=stage.value)
            self.stage_arrays[stage] = assembled
            self._ensure_stage_metrics(stage)
            self._emit(
                {
                    "type": "stage_complete",
                    "stage": stage.value,
                    "metrics": self.stage_metrics.get(stage.value),
                }
            )
        self._emit(
            {
                "type": "complete",
                "stages_complete": len(self.stage_arrays),
                "snapshot_invalidated": self.snapshot_invalidated,
                "resumed_commits": self.resumed_commits,
                "queue_events": len(self.queue_events),
                "budget_peak_bytes": self.budget_peak_bytes,
            }
        )
        return self

    def _normalize_processing_states(self) -> None:
        snapshot = self.state_machine.snapshot()
        for cell in snapshot.cells.values():
            if cell.state is BlockState.PROCESSING:
                self.state_machine.transition(
                    cell.stage,
                    cell.block_id,
                    BlockState.FAILED,
                    error="上次运行中断时的处理中状态",
                )

    def _ensure_stage_metrics(self, stage: Stage) -> None:
        if stage.value in self.stage_metrics:
            return
        metrics = compute_all_metrics(
            {stage: self.stage_arrays[stage]},
            self.sample.truth_mask,
            self.sample.lines,
            self.sample.circles,
            self.config,
        )
        self.stage_metrics.update(metrics)

    def _run_stage(
        self,
        stage: Stage,
        source: np.ndarray,
        *,
        retry_failed: bool,
    ) -> None:
        required = self.config.required_band(stage)
        effective = self.config.effective_band(stage)
        pending_blocks = [
            block
            for block in self.blocks
            if self.state_machine.cell(stage, block.block_id).state
            in ({BlockState.PENDING, BlockState.FAILED} if retry_failed else {BlockState.PENDING})
        ]
        if effective < required and pending_blocks:
            block = pending_blocks[0]
            raise BandError(
                "边带宽度不足，拒绝以截断边界继续",
                block_id=block.block_id,
                stage=stage.value,
                details={
                    "required_width": required,
                    "configured_width": effective,
                    "tile_size": self.config.tile_size,
                },
            )
        self._emit(
            {
                "type": "stage_start",
                "stage": stage.value,
                "blocks": len(pending_blocks),
                "band_width": effective,
            }
        )
        if not pending_blocks:
            return
        if self.execution_mode == "serial":
            outcomes = self._execute_serial(stage, source, pending_blocks)
        else:
            outcomes = self._execute_parallel(stage, source, pending_blocks)
        failures = [outcome for outcome in outcomes if outcome.error is not None]
        if failures:
            details = [
                {
                    "stage": stage.value,
                    "block_id": failure.block.block_id,
                    "message": (failure.error or {}).get("message", "unknown"),
                    "exception": (failure.error or {}).get("exception"),
                }
                for failure in failures
            ]
            self.failures.extend(details)
            raise PipelineFailure(
                "阶段内存在失败分块；其他分块与已提交阶段保持不变",
                stage=stage.value,
                failures=details,
            )

    def _execute_serial(
        self,
        stage: Stage,
        source: np.ndarray,
        blocks: list[Block],
    ) -> list[BlockTaskOutcome]:
        outcomes: list[BlockTaskOutcome] = []
        budget = MemoryBudget(self.config.memory_budget_bytes)
        for block in blocks:
            self.control.wait_if_paused()
            estimate, expanded_pixels = self._estimate_task(stage, block)
            if expanded_pixels > self.config.thread_buffer_pixels:
                raise BudgetError(
                    "单块边带缓冲超过每线程像素上限",
                    field="thread_buffer_pixels",
                    block_id=block.block_id,
                    stage=stage.value,
                    details={
                        "required_pixels": expanded_pixels,
                        "limit": self.config.thread_buffer_pixels,
                    },
                )
            if estimate > budget.capacity_bytes:
                raise BudgetError(
                    "单块边带内存超过全局预算，无法排队完成",
                    field="memory_budget_bytes",
                    block_id=block.block_id,
                    stage=stage.value,
                    details={
                        "required_bytes": estimate,
                        "budget_bytes": budget.capacity_bytes,
                    },
                )
            if not budget.try_reserve(estimate):
                raise BudgetError(
                    "串行路径无法取得边带预算",
                    field="memory_budget_bytes",
                    block_id=block.block_id,
                    stage=stage.value,
                )
            self.state_machine.transition(stage, block.block_id, BlockState.PROCESSING)
            self._emit(
                {
                    "type": "block_processing",
                    "stage": stage.value,
                    "block_id": block.block_id,
                    "execution_mode": "serial",
                }
            )
            started = time.monotonic()
            try:
                array = self._execute_block(stage, source, block)
                outcome = BlockTaskOutcome(
                    block=block,
                    array=array,
                    error=None,
                    elapsed_seconds=time.monotonic() - started,
                )
            except Exception as exc:
                outcome = BlockTaskOutcome(
                    block=block,
                    array=None,
                    error=self._exception_payload(exc),
                    elapsed_seconds=time.monotonic() - started,
                )
            finally:
                budget.release(estimate)
                self.budget_peak_bytes = max(self.budget_peak_bytes, budget.peak_bytes)
            outcomes.append(outcome)
            self._commit_outcome(stage, outcome)
        return outcomes

    def _execute_parallel(
        self,
        stage: Stage,
        source: np.ndarray,
        blocks: list[Block],
    ) -> list[BlockTaskOutcome]:
        budget = MemoryBudget(self.config.memory_budget_bytes)
        pending = deque(blocks)
        active: dict[Future[BlockTaskOutcome], Block] = {}
        reservations: dict[Future[BlockTaskOutcome], int] = {}
        outcomes: list[BlockTaskOutcome] = []
        result_by_block: dict[int, BlockTaskOutcome] = {}
        next_to_commit = 0
        pending_ids = [block.block_id for block in blocks]
        with ThreadPoolExecutor(
            max_workers=self.config.max_parallel,
            thread_name_prefix=f"edgeforge-{stage.value}",
        ) as executor:
            while pending or active:
                self.control.wait_if_paused()
                while pending and len(active) < self.config.max_parallel:
                    block = pending[0]
                    estimate, expanded_pixels = self._estimate_task(stage, block)
                    if expanded_pixels > self.config.thread_buffer_pixels:
                        raise BudgetError(
                            "单块边带缓冲超过每线程像素上限",
                            field="thread_buffer_pixels",
                            block_id=block.block_id,
                            stage=stage.value,
                            details={
                                "required_pixels": expanded_pixels,
                                "limit": self.config.thread_buffer_pixels,
                            },
                        )
                    if estimate > budget.capacity_bytes:
                        raise BudgetError(
                            "单块边带内存超过全局预算，无法排队完成",
                            field="memory_budget_bytes",
                            block_id=block.block_id,
                            stage=stage.value,
                            details={
                                "required_bytes": estimate,
                                "budget_bytes": budget.capacity_bytes,
                            },
                        )
                    if not budget.try_reserve(estimate):
                        key = (stage, block.block_id)
                        if key not in self._queue_started:
                            self._queue_started[key] = time.monotonic()
                            self._emit(
                                {
                                    "type": "budget_queue",
                                    "stage": stage.value,
                                    "block_id": block.block_id,
                                    "reason": "memory_budget",
                                    "required_bytes": estimate,
                                    "budget_bytes": budget.capacity_bytes,
                                    "reserved_bytes": budget.reserved_bytes,
                                }
                            )
                        break
                    key = (stage, block.block_id)
                    if key in self._queue_started:
                        waited_ms = (time.monotonic() - self._queue_started.pop(key)) * 1000.0
                        self.queue_events.append(
                            {
                                "stage": stage.value,
                                "block_id": block.block_id,
                                "reason": "memory_budget",
                                "waited_ms": waited_ms,
                            }
                        )
                    pending.popleft()
                    self.state_machine.transition(
                        stage,
                        block.block_id,
                        BlockState.PROCESSING,
                    )
                    self._emit(
                        {
                            "type": "block_processing",
                            "stage": stage.value,
                            "block_id": block.block_id,
                            "execution_mode": "parallel",
                        }
                    )
                    future = executor.submit(
                        self._parallel_task,
                        stage,
                        source,
                        block,
                    )
                    active[future] = block
                    reservations[future] = estimate
                if not active:
                    if pending:
                        raise BudgetError(
                            "预算调度无活动任务但仍无法提交，已阻塞",
                            stage=stage.value,
                            block_id=pending[0].block_id,
                        )
                    break
                done, _ = wait(
                    tuple(active),
                    timeout=0.1,
                    return_when=FIRST_COMPLETED,
                )
                for future in done:
                    block = active.pop(future)
                    estimate = reservations.pop(future)
                    budget.release(estimate)
                    self.budget_peak_bytes = max(
                        self.budget_peak_bytes,
                        budget.peak_bytes,
                    )
                    try:
                        outcome = future.result()
                    except Exception as exc:
                        outcome = BlockTaskOutcome(
                            block=block,
                            array=None,
                            error=self._exception_payload(exc),
                            elapsed_seconds=0.0,
                        )
                    outcomes.append(outcome)
                    result_by_block[block.block_id] = outcome
                    while (
                        next_to_commit < len(pending_ids)
                        and pending_ids[next_to_commit] in result_by_block
                    ):
                        block_id = pending_ids[next_to_commit]
                        self._commit_outcome(stage, result_by_block.pop(block_id))
                        next_to_commit += 1
                    self._emit(
                        {
                            "type": "block_work_done",
                            "stage": stage.value,
                            "block_id": block.block_id,
                            "ok": outcome.error is None,
                        }
                    )
        outcomes.sort(key=lambda outcome: outcome.block.block_id)
        return outcomes

    def _parallel_task(
        self,
        stage: Stage,
        source: np.ndarray,
        block: Block,
    ) -> BlockTaskOutcome:
        started = time.monotonic()
        try:
            self.control.wait_if_paused()
            array = self._execute_block(stage, source, block)
            return BlockTaskOutcome(
                block=block,
                array=array,
                error=None,
                elapsed_seconds=time.monotonic() - started,
            )
        except CancelledError:
            raise
        except Exception as exc:
            return BlockTaskOutcome(
                block=block,
                array=None,
                error=self._exception_payload(exc),
                elapsed_seconds=time.monotonic() - started,
            )

    def _execute_block(
        self,
        stage: Stage,
        source: np.ndarray,
        block: Block,
    ) -> np.ndarray:
        if self._consume_fault(stage, block.block_id):
            raise RuntimeError(
                f"injected single-block failure at {stage.value}/{block.block_id}"
            )
        band = self.config.effective_band(stage)
        ey0, ey1, ex0, ex1 = expanded_region(
            block,
            band,
            self.sample.height,
            self.sample.width,
        )
        expanded = np.asarray(source[ey0:ey1, ex0:ex1])
        protected_borders = (
            ey0 > 0,
            ey1 < self.sample.height,
            ex0 > 0,
            ex1 < self.sample.width,
        )
        processed = apply_stage(
            stage,
            expanded,
            self.config,
            protected_borders=protected_borders,
        )
        local_y0 = block.y0 - ey0
        local_y1 = block.y1 - ey0
        local_x0 = block.x0 - ex0
        local_x1 = block.x1 - ex0
        core = processed[local_y0:local_y1, local_x0:local_x1]
        expected_dtype = np.float32 if stage is Stage.GRADIENT else np.uint8
        if core.dtype != expected_dtype:
            core = core.astype(expected_dtype, copy=False)
        expected_shape = (
            block.height,
            block.width,
        ) + (() if self.sample.channels == 1 else (self.sample.channels,))
        if core.shape != expected_shape:
            raise RuntimeError(
                f"stage {stage.value} block {block.block_id} shape "
                f"{core.shape} != {expected_shape}"
            )
        return np.ascontiguousarray(core)

    def _consume_fault(self, stage: Stage, block_id: int) -> bool:
        with self._fault_lock:
            key = (stage, block_id)
            if key in self._fault_once:
                self._fault_once.remove(key)
                return True
            return False

    def _commit_outcome(
        self,
        stage: Stage,
        outcome: BlockTaskOutcome,
    ) -> None:
        block = outcome.block
        if outcome.error is not None:
            self.state_machine.transition(
                stage,
                block.block_id,
                BlockState.FAILED,
                error=outcome.error["message"],
            )
            self._emit(
                {
                    "type": "block_failed",
                    "stage": stage.value,
                    "block_id": block.block_id,
                    "message": outcome.error["message"],
                    "exception": outcome.error.get("exception"),
                }
            )
            return
        assert outcome.array is not None
        if self.checkpoint is not None:
            try:
                metadata = self.checkpoint.commit(stage, block.block_id, outcome.array)
            except EdgeForgeError:
                raise
            except Exception as exc:
                raise CheckpointError(
                    "检查点写入失败",
                    stage=stage.value,
                    block_id=block.block_id,
                    details={"reason": str(exc)},
                ) from exc
        else:
            metadata = {}
        self.block_outputs[(stage, block.block_id)] = outcome.array
        self.state_machine.transition(
            stage,
            block.block_id,
            BlockState.COMMITTED,
        )
        self._emit(
            {
                "type": "block_committed",
                "stage": stage.value,
                "block_id": block.block_id,
                "content_hash": metadata.get("content_hash"),
                "elapsed_ms": outcome.elapsed_seconds * 1000.0,
            }
        )

    def _assemble_stage(self, stage: Stage) -> np.ndarray | None:
        if stage is Stage.GRADIENT:
            dtype = np.float32
        else:
            dtype = np.uint8
        shape = self.sample.image.shape
        output = np.zeros(shape, dtype=dtype)
        for block in self.blocks:
            array = self.block_outputs.get((stage, block.block_id))
            if array is None and self.checkpoint is not None:
                array = self.checkpoint.record_for(stage, block.block_id)
                if array is not None:
                    self.block_outputs[(stage, block.block_id)] = array
            if array is None:
                return None
            output[block.y0 : block.y1, block.x0 : block.x1] = array
        return np.ascontiguousarray(output)

    def _estimate_task(
        self,
        stage: Stage,
        block: Block,
    ) -> tuple[int, int]:
        band = self.config.effective_band(stage)
        ey0, ey1, ex0, ex1 = expanded_region(
            block,
            band,
            self.sample.height,
            self.sample.width,
        )
        expanded_pixels = (ey1 - ey0) * (ex1 - ex0) * self.sample.channels
        estimate = expanded_pixels * 8
        return estimate, expanded_pixels

    @staticmethod
    def _exception_payload(exc: Exception) -> dict[str, Any]:
        if isinstance(exc, EdgeForgeError):
            return {
                "code": exc.code,
                "message": str(exc),
                "field": exc.field,
                "details": exc.details,
                "exception": type(exc).__name__,
            }
        return {
            "code": "E_INTERNAL",
            "message": str(exc) or type(exc).__name__,
            "exception": type(exc).__name__,
            "traceback": traceback.format_exc(limit=8),
        }


def process_whole_image(
    sample: LoadedSample,
    config: PipelineConfig,
) -> dict[Stage, np.ndarray]:
    config.validate()
    current = sample.image
    outputs: dict[Stage, np.ndarray] = {}
    for stage in STAGE_ORDER:
        current = apply_stage(stage, current, config)
        expected_dtype = np.float32 if stage is Stage.GRADIENT else np.uint8
        if current.dtype != expected_dtype:
            current = current.astype(expected_dtype, copy=False)
        outputs[stage] = np.ascontiguousarray(current)
    return outputs
