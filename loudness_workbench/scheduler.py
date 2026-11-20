"""Stable, bounded parallel scheduling for per-asset observation blocks."""

from __future__ import annotations

import concurrent.futures
import threading
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable

from .budget import BudgetPlan, fit_plan, initial_plan
from .config import MeterConfig
from .errors import ErrorCode, MeterError, error
from .metering import AssetMeter
from .models import BlockResult
from .trace import TraceWriter


@dataclass(frozen=True)
class TaskSpec:
    asset_id: str
    block_index: int
    submit_seq: int
    block_samples: int
    channels: int
    retained_samples: int
    oversample_factor: int


@dataclass(frozen=True)
class TaskOutcome:
    task: TaskSpec
    block: BlockResult
    plan: BudgetPlan
    memory_bytes: int
    degrade_events: tuple[dict[str, Any], ...]


@dataclass(frozen=True)
class SchedulerResult:
    ok: bool
    completions: tuple[BlockResult, ...]
    errors: tuple[dict[str, Any], ...]
    degraded_blocks: int
    waiting_events: int
    trace_events: tuple[dict[str, Any], ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": self.ok,
            "completed_blocks": len(self.completions),
            "error_count": len(self.errors),
            "degraded_blocks": self.degraded_blocks,
            "waiting_events": self.waiting_events,
            "errors": list(self.errors),
        }


@dataclass
class _AssetQueue:
    asset_id: str
    meter: AssetMeter
    tasks: deque[TaskSpec]
    active: bool = True
    failed: bool = False


class MeterScheduler:
    """Run one sequential stream per asset and parallel streams globally.

    A round admits one head task per asset, ordered by globally stable submit
    sequence.  Each round executes admitted tasks concurrently and records
    completions in submit-sequence order, keeping trace bytes deterministic.
    """

    def __init__(
        self,
        config: MeterConfig,
        *,
        trace_path: str | Path | None = None,
        trace: TraceWriter | None = None,
        run_metadata: dict[str, Any] | None = None,
        on_block_complete: Callable[[AssetMeter, BlockResult], None] | None = None,
    ):
        self.config = config
        self._queues: dict[str, _AssetQueue] = {}
        self._submission_order: list[str] = []
        self._sequence = 0
        self._lock = threading.RLock()
        self._executed = False
        metadata = dict(run_metadata or {})
        metadata.update(
            {
                "parallelism": config.scheduler.parallelism,
                "memory_budget_bytes": config.scheduler.memory_budget_bytes,
                "per_task_memory_budget_bytes": (
                    config.scheduler.per_task_memory_budget_bytes
                ),
                "config_hash": config.config_hash,
            }
        )
        self.trace = trace or TraceWriter(trace_path, run_metadata=metadata)
        self._on_block_complete = on_block_complete
        self._degraded_blocks = 0
        self._waiting_events = 0

    @property
    def active_asset_ids(self) -> tuple[str, ...]:
        with self._lock:
            return tuple(
                asset_id for asset_id, queue in self._queues.items() if queue.active
            )

    def start_asset(
        self,
        asset_id: str,
        meter: AssetMeter,
        *,
        block_indices: Iterable[int] | None = None,
    ) -> tuple[TaskSpec, ...]:
        """Register an asset; duplicate active registration returns E_BUSY."""
        with self._lock:
            existing = self._queues.get(asset_id)
            if existing is not None and existing.active:
                raise error(
                    ErrorCode.E_BUSY,
                    "asset stream is already active",
                    {"asset_id": asset_id},
                )
            if self._executed:
                raise error(
                    ErrorCode.E_BUSY,
                    "scheduler has already been executed",
                    {"asset_id": asset_id},
                )
            if block_indices is None:
                indices = list(range(meter._next_state_index, meter.total_blocks))
            else:
                indices = [int(index) for index in block_indices]
                if indices != sorted(set(indices)):
                    raise error(
                        ErrorCode.E_CONFIG_INVALID,
                        "block indices must be unique and increasing",
                        {"asset_id": asset_id, "block_indices": indices},
                    )
                expected = list(
                    range(indices[0] if indices else 0, (indices[-1] + 1) if indices else 0)
                )
                if indices != expected:
                    raise error(
                        ErrorCode.E_CONFIG_INVALID,
                        "same-asset blocks must be submitted as a contiguous sequence",
                        {"asset_id": asset_id, "block_indices": indices},
                    )
            tasks: list[TaskSpec] = []
            for index in indices:
                start, end = meter._block_range(index)
                self._sequence += 1
                tasks.append(
                    TaskSpec(
                        asset_id=asset_id,
                        block_index=index,
                        submit_seq=self._sequence,
                        block_samples=end - start,
                        channels=meter.info.channels,
                        retained_samples=min(
                            config_short_term(self.config),
                            end,
                        ),
                        oversample_factor=self.config.oversample_factor,
                    )
                )
            self._queues[asset_id] = _AssetQueue(
                asset_id=asset_id,
                meter=meter,
                tasks=deque(tasks),
            )
            self._submission_order.append(asset_id)
            self.trace.emit(
                "asset_start",
                {
                    "asset_id": asset_id,
                    "block_count": len(tasks),
                    "first_block": indices[0] if indices else None,
                    "last_block": indices[-1] if indices else None,
                },
                submit_order=len(self._submission_order),
            )
            return tuple(tasks)

    def submit_asset(
        self,
        asset_id: str,
        meter: AssetMeter,
        *,
        block_indices: Iterable[int] | None = None,
    ) -> tuple[TaskSpec, ...]:
        return self.start_asset(asset_id, meter, block_indices=block_indices)

    def _ready_heads(self) -> list[_AssetQueue]:
        heads = [
            queue
            for queue in self._queues.values()
            if queue.active and not queue.failed and queue.tasks
        ]
        heads.sort(key=lambda queue: queue.tasks[0].submit_seq)
        return heads

    def _fail_asset(
        self,
        queue: _AssetQueue,
        exc: BaseException,
        *,
        task: TaskSpec | None = None,
    ) -> dict[str, Any]:
        if isinstance(exc, MeterError):
            payload = exc.to_dict()
        else:
            payload = error(
                ErrorCode.E_TASK_FAILED,
                str(exc),
                {"asset_id": queue.asset_id},
            ).to_dict()
        if task is not None:
            payload.setdefault("context", {})
            payload["context"].setdefault("block_index", task.block_index)
        queue.failed = True
        queue.active = False
        self.trace.emit(
            "asset_failed",
            payload,
            asset_id=queue.asset_id,
            block_index=task.block_index if task else None,
        )
        return payload

    def execute(self, *, raise_on_error: bool = False) -> SchedulerResult:
        with self._lock:
            if self._executed:
                raise error(
                    ErrorCode.E_BUSY,
                    "scheduler.execute() may only be called once",
                )
            self._executed = True
        completions: list[BlockResult] = []
        errors: list[dict[str, Any]] = []
        with concurrent.futures.ThreadPoolExecutor(
            max_workers=self.config.scheduler.parallelism,
            thread_name_prefix="loudness-block",
        ) as executor:
            while True:
                with self._lock:
                    heads = self._ready_heads()
                if not heads:
                    break
                selected: list[
                    tuple[_AssetQueue, TaskSpec, BudgetPlan, int, tuple[dict[str, Any], ...]]
                ] = []
                available_memory = self.config.scheduler.memory_budget_bytes
                for queue in heads:
                    if len(selected) >= self.config.scheduler.parallelism:
                        break
                    task = queue.tasks[0]
                    initial = initial_plan(self.config.scheduler)
                    initial_estimate = initial.estimate_memory_bytes(
                        block_samples=task.block_samples,
                        channels=task.channels,
                        retained_samples=task.retained_samples,
                        oversample_factor=task.oversample_factor,
                    )
                    if (
                        initial_estimate
                        > self.config.scheduler.per_task_memory_budget_bytes
                    ):
                        self._waiting_events += 1
                        self.trace.emit(
                            "budget_wait",
                            {
                                "asset_id": task.asset_id,
                                "block_index": task.block_index,
                                "reason": "per_task_budget",
                                "estimated_bytes": initial_estimate,
                                "budget_bytes": (
                                    self.config.scheduler.per_task_memory_budget_bytes
                                ),
                            },
                            asset_id=task.asset_id,
                            block_index=task.block_index,
                        )
                    plan, degrade_events = fit_plan(
                        self.config.scheduler,
                        block_samples=task.block_samples,
                        channels=task.channels,
                        retained_samples=task.retained_samples,
                        oversample_factor=task.oversample_factor,
                        budget_bytes=(
                            self.config.scheduler.per_task_memory_budget_bytes
                        ),
                    )
                    estimated = plan.estimate_memory_bytes(
                        block_samples=task.block_samples,
                        channels=task.channels,
                        retained_samples=task.retained_samples,
                        oversample_factor=task.oversample_factor,
                    )
                    if estimated > self.config.scheduler.per_task_memory_budget_bytes:
                        exc = error(
                            ErrorCode.E_BUDGET_EXCEEDED,
                            "block exceeds the per-task memory budget after degrade",
                            {
                                "asset_id": task.asset_id,
                                "block_index": task.block_index,
                                "estimated_bytes": estimated,
                                "budget_bytes": (
                                    self.config.scheduler.per_task_memory_budget_bytes
                                ),
                                "degrade_stage": plan.degrade_stage,
                                "attempts": list(degrade_events),
                            },
                        )
                        self.trace.emit(
                            "budget_reject",
                            exc.to_dict()["context"],
                            asset_id=task.asset_id,
                            block_index=task.block_index,
                        )
                        errors.append(self._fail_asset(queue, exc, task=task))
                        continue
                    if estimated > available_memory:
                        self._waiting_events += 1
                        self.trace.emit(
                            "budget_wait",
                            {
                                "asset_id": task.asset_id,
                                "block_index": task.block_index,
                                "estimated_bytes": estimated,
                                "available_bytes": available_memory,
                            },
                            asset_id=task.asset_id,
                            block_index=task.block_index,
                        )
                        continue
                    for event in degrade_events:
                        if event["stage"] != "none":
                            self.trace.emit(
                                "degrade",
                                event,
                                asset_id=task.asset_id,
                                block_index=task.block_index,
                            )
                    available_memory -= estimated
                    selected.append(
                        (queue, task, plan, estimated, tuple(degrade_events))
                    )
                    if any(event["stage"] != "none" for event in degrade_events):
                        self._degraded_blocks += 1

                if not selected:
                    if any(
                        queue.active and not queue.failed and queue.tasks
                        for queue in self._queues.values()
                    ):
                        # A per-task fit always fits the global budget when the
                        # per-task cap is <= global cap, so this is defensive.
                        continue
                    break

                futures: dict[concurrent.futures.Future, tuple[Any, ...]] = {}
                for queue, task, plan, estimated, degrade_events in selected:
                    self.trace.emit(
                        "block_start",
                        {
                            "asset_id": task.asset_id,
                            "block_index": task.block_index,
                            "estimated_bytes": estimated,
                            "degrade_stage": plan.degrade_stage,
                            "queue_memory_bytes": available_memory,
                        },
                        submit_seq=task.submit_seq,
                    )
                    futures[
                        executor.submit(
                            self._run_task,
                            queue.meter,
                            task,
                            plan,
                            estimated,
                        )
                    ] = (queue, task, plan, estimated, degrade_events)

                concurrent.futures.wait(futures)
                outcomes: list[tuple[Any, ...]] = []
                for future, metadata in futures.items():
                    outcomes.append((future, *metadata))
                outcomes.sort(key=lambda item: item[2].submit_seq)

                for future, queue, task, plan, estimated, degrade_events in outcomes:
                    try:
                        block = future.result()
                    except BaseException as exc:
                        payload = self._fail_asset(queue, exc, task=task)
                        errors.append(payload)
                        self.trace.emit(
                            "block_failed",
                            payload,
                            asset_id=task.asset_id,
                            block_index=task.block_index,
                            submit_seq=task.submit_seq,
                        )
                        continue
                    queue.tasks.popleft()
                    completions.append(block)
                    self.trace.emit(
                        "block_complete",
                        {
                            "asset_id": task.asset_id,
                            "block_index": task.block_index,
                            "sync_marker": block.sync_marker,
                            "short_term_lufs": block.short_term_lufs,
                            "integrated_lufs": block.integrated_lufs,
                            "true_peak_dbtp": block.true_peak_dbtp,
                            "content_hash": block.content_hash,
                            "block_hash": block.block_hash,
                            "degrade_stage": block.degrade_stage,
                            "memory_bytes": block.memory_bytes,
                            "estimated_bytes": estimated,
                        },
                        submit_seq=task.submit_seq,
                    )
                    if self._on_block_complete is not None:
                        try:
                            self._on_block_complete(queue.meter, block)
                        except BaseException as exc:
                            payload = self._fail_asset(queue, exc, task=task)
                            errors.append(payload)
                            self.trace.emit(
                                "snapshot_failed",
                                payload,
                                asset_id=task.asset_id,
                                block_index=task.block_index,
                            )
                            continue
                    if not queue.tasks:
                        queue.active = False
                        self.trace.emit(
                            "asset_complete",
                            {
                                "asset_id": queue.asset_id,
                                "published_blocks": queue.meter.get_published().block_count,
                                "head_hash": queue.meter.get_published().head_hash,
                            },
                        )

        result = SchedulerResult(
            ok=not errors,
            completions=tuple(completions),
            errors=tuple(errors),
            degraded_blocks=self._degraded_blocks,
            waiting_events=self._waiting_events,
            trace_events=tuple(self.trace.events),
        )
        self.trace.close(
            {
                "ok": result.ok,
                "completed_blocks": len(completions),
                "error_count": len(errors),
                "degraded_blocks": self._degraded_blocks,
                "event_count": len(self.trace.events) + 1,
            }
        )
        if raise_on_error and errors:
            first = errors[0]
            raise MeterError(
                str(first["code"]),
                str(first["message"]),
                dict(first.get("context", {})),
            )
        return result

    def run(self, *, raise_on_error: bool = False) -> SchedulerResult:
        return self.execute(raise_on_error=raise_on_error)

    @staticmethod
    def _run_task(
        meter: AssetMeter,
        task: TaskSpec,
        plan: BudgetPlan,
        estimated: int,
    ) -> BlockResult:
        return meter.process_block(
            task.block_index,
            budget_plan=plan,
            memory_bytes=estimated,
        )


def config_short_term(config: MeterConfig) -> int:
    return config.short_term_samples
