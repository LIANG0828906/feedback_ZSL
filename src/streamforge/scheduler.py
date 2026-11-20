from __future__ import annotations

import threading
import time
from concurrent.futures import Future, ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from .cache import TierCache, TierCacheState
from .errors import BusinessError, ErrorCode, fail
from .generator import SliceWriter, estimate_memory
from .models import (
    BudgetConfig,
    MemoryPlan,
    SliceArtifact,
    SourceTimeline,
    TierSpec,
)


class RunControl:
    def __init__(self) -> None:
        self.pause_event = threading.Event()
        self.cancel_event = threading.Event()

    def pause(self) -> None:
        self.pause_event.set()

    def resume(self) -> None:
        self.pause_event.clear()

    def cancel(self) -> None:
        self.cancel_event.set()

    @property
    def paused(self) -> bool:
        return self.pause_event.is_set()

    @property
    def cancelled(self) -> bool:
        return self.cancel_event.is_set()

    def wait_if_paused(self) -> None:
        while self.paused and not self.cancelled:
            time.sleep(0.02)


@dataclass
class ScheduledTask:
    tier: TierSpec
    index: int
    submit_seq: int
    prev_hash: str
    plan: MemoryPlan
    status: str = "queued"
    wait_rounds: int = 0
    wait_ms: int = 0
    gave_way: int = 0
    admitted_round: int | None = None

    @property
    def key(self) -> tuple[str, int]:
        return (self.tier.tier_id, self.index)

    def waiting_dict(self) -> dict[str, Any]:
        return {
            "gave_way_count": self.gave_way,
            "index": self.index,
            "submit_seq": self.submit_seq,
            "tier": self.tier.tier_id,
            "wait_ms": self.wait_ms,
            "wait_rounds": self.wait_rounds,
        }


@dataclass
class TierWork:
    tier: TierSpec
    cache: TierCache
    state: TierCacheState
    current_task: ScheduledTask | None = None
    halted: bool = False
    error: dict[str, Any] | None = None

    @property
    def next_index(self) -> int:
        return self.state.next_index

    @property
    def prev_hash(self) -> str:
        return self.state.prev_hash


@dataclass
class SchedulerResult:
    tiers: dict[str, TierWork]
    events: list[dict[str, Any]]
    memory_samples: list[dict[str, int]]
    peak_bytes: int
    waiting_queue: list[dict[str, Any]]
    cancelled: bool = False
    errors: list[dict[str, Any]] = field(default_factory=list)

    def artifacts(self) -> dict[str, list[SliceArtifact]]:
        return {
            tier_id: list(work.state.artifacts)
            for tier_id, work in self.tiers.items()
        }


class SliceScheduler:
    """Deterministic batch-barrier scheduler with explicit memory admission."""

    def __init__(
        self,
        *,
        timeline: SourceTimeline,
        tiers: list[TierSpec],
        budget: BudgetConfig,
        output_root: Path,
        states: dict[str, TierCacheState],
        control: RunControl | None = None,
        on_event: Callable[[dict[str, Any]], None] | None = None,
        on_progress: Callable[[dict[str, Any]], None] | None = None,
        fault_at: tuple[str, int] | None = None,
    ) -> None:
        self.timeline = timeline
        self.tiers = tiers
        self.budget = budget
        self.control = control or RunControl()
        self.on_event = on_event
        self.on_progress = on_progress
        self.fault_at = fault_at
        self._running_lock = threading.Lock()
        self._running = False
        self._active_tiers: set[str] = set()
        self.events: list[dict[str, Any]] = []
        self.memory_samples: list[dict[str, int]] = []
        self.peak_bytes = 0
        self.waiting_queue: list[dict[str, Any]] = []
        self._event_seq = 0
        self._step = 0
        ordered_tiers = sorted(tiers, key=lambda item: item.target_bitrate_kbps)
        self._high_tier_ids = {
            tier.tier_id for tier in ordered_tiers[len(ordered_tiers) // 2 :]
        }
        self.works: dict[str, TierWork] = {}
        for tier in tiers:
            cache = TierCache(output_root, tier, timeline)
            state = states.get(tier.tier_id) or TierCacheState()
            self.works[tier.tier_id] = TierWork(
                tier=tier,
                cache=cache,
                state=state,
                error=state.error,
            )
        self._submit_seq = {
            (tier.tier_id, index): index * len(tiers) + tier_position
            for index in range(len(timeline.boundaries))
            for tier_position, tier in enumerate(tiers)
        }

    def _emit(self, kind: str, **payload: Any) -> None:
        event = {
            "event": kind,
            "event_seq": self._event_seq,
            "step": self._step,
            **payload,
        }
        self._event_seq += 1
        self.events.append(event)
        if self.on_event is not None:
            self.on_event(event)

    def _record_memory(self, allocated: int) -> None:
        self.peak_bytes = max(self.peak_bytes, allocated)
        sample = {
            "allocated_bytes": allocated,
            "peak_bytes": self.peak_bytes,
            "step": self._step,
        }
        self.memory_samples.append(sample)
        if self.on_progress is not None:
            self.on_progress(
                {
                    "allocated_bytes": allocated,
                    "peak_bytes": self.peak_bytes,
                    "step": self._step,
                    "tiers": {
                        tier_id: {
                            "completed": len(work.state.artifacts),
                            "halted": work.halted,
                            "total": len(self.timeline.boundaries),
                        }
                        for tier_id, work in self.works.items()
                    },
                }
            )

    def _halt(
        self,
        work: TierWork,
        error: BusinessError | dict[str, Any],
        *,
        task: ScheduledTask | None = None,
    ) -> None:
        if work.halted:
            return
        work.halted = True
        work.current_task = None
        payload = error.to_dict() if isinstance(error, BusinessError) else error
        work.error = payload
        work.state.error = payload
        work.state.status = "cancelled" if payload.get("code") == ErrorCode.CANCELLED else "failed"
        work.cache.save(work.state, self._config_version)
        event_payload = {
            "error": payload,
            "tier": work.tier.tier_id,
        }
        if task is not None:
            event_payload["index"] = task.index
            event_payload["submit_seq"] = task.submit_seq
        self._emit("tier_halted", **event_payload)

    _config_version = "unknown"

    def set_config_version(self, value: str) -> None:
        self._config_version = value

    def _make_task(self, work: TierWork) -> ScheduledTask | None:
        if work.halted or work.next_index >= len(self.timeline.boundaries):
            if work.next_index >= len(self.timeline.boundaries) and not work.halted:
                work.state.status = "complete"
                work.cache.save(work.state, self._config_version)
            return None
        task = work.current_task
        if task is not None:
            return task
        boundary = self.timeline.boundaries[work.next_index]
        try:
            plan = estimate_memory(
                tier=work.tier,
                timeline=self.timeline,
                boundary=boundary,
                budget=self.budget,
            )
        except BusinessError as exc:
            self._emit(
                "budget_rejected",
                error=exc.to_dict(),
                index=boundary.index,
                tier=work.tier.tier_id,
            )
            self._halt(work, exc)
            return None
        if plan.estimated_bytes > self.budget.total_memory_bytes:
            error = fail(
                ErrorCode.BUDGET_EXCEEDED,
                "slice estimate exceeds total memory budget",
                tier=work.tier.tier_id,
                slice_index=boundary.index,
                estimated_bytes=plan.estimated_bytes,
                total_budget_bytes=self.budget.total_memory_bytes,
            )
            self._emit(
                "budget_rejected",
                error=error.to_dict(),
                index=boundary.index,
                tier=work.tier.tier_id,
            )
            self._halt(work, error)
            return None
        task = ScheduledTask(
            tier=work.tier,
            index=boundary.index,
            submit_seq=self._submit_seq[(work.tier.tier_id, boundary.index)],
            prev_hash=work.prev_hash,
            plan=plan,
        )
        work.current_task = task
        if plan.degraded:
            self._emit(
                "memory_degraded",
                block_bytes=plan.block_bytes,
                estimated_bytes=plan.estimated_bytes,
                index=task.index,
                tier=task.tier.tier_id,
            )
        self._emit(
            "task_queued",
            estimated_bytes=plan.estimated_bytes,
            index=task.index,
            submit_seq=task.submit_seq,
            tier=task.tier.tier_id,
        )
        return task

    def _pre_commit(self, tier_id: str, index: int) -> None:
        if self.fault_at == (tier_id, index):
            raise BusinessError(
                ErrorCode.INJECTED_FAILURE,
                "injected slice failure before atomic commit",
                {"tier": tier_id, "slice_index": index},
            )

    def _execute(
        self, work: TierWork, task: ScheduledTask
    ) -> SliceArtifact:
        boundary = self.timeline.boundaries[task.index]
        writer = SliceWriter(work.cache.directory)
        try:
            return writer.write(
                timeline=self.timeline,
                tier=work.tier,
                boundary=boundary,
                prev_hash=task.prev_hash,
                plan=task.plan,
                cancel_event=self.control.cancel_event,
                pre_commit=self._pre_commit,
            )
        except BaseException:
            writer.cleanup_failed(task.index)
            raise

    def run(self) -> SchedulerResult:
        with self._running_lock:
            if self._running:
                raise fail(ErrorCode.BUSY, "scheduler is already running")
            for tier_id in self._active_tiers:
                raise fail(
                    ErrorCode.BUSY,
                    "tier already has an active slicing task",
                    tier=tier_id,
                )
            self._running = True
            self._active_tiers = set(self.works)
        try:
            return self._run_inner()
        finally:
            with self._running_lock:
                self._running = False
                self._active_tiers.clear()

    def _mark_cancelled(self) -> None:
        for work in self.works.values():
            if work.next_index < len(self.timeline.boundaries) and not work.halted:
                self._halt(
                    work,
                    BusinessError(
                        ErrorCode.CANCELLED,
                        "run cancelled",
                        {"tier": work.tier.tier_id, "next_index": work.next_index},
                    ),
                    task=work.current_task,
                )

    def _run_inner(self) -> SchedulerResult:
        executor = ThreadPoolExecutor(
            max_workers=self.budget.global_parallelism,
            thread_name_prefix="streamforge-slice",
        )
        try:
            while True:
                if self.control.cancelled:
                    self._mark_cancelled()
                    break
                self.control.wait_if_paused()
                if self.control.cancelled:
                    self._mark_cancelled()
                    break

                candidates: list[tuple[TierWork, ScheduledTask]] = []
                for work in self.works.values():
                    task = self._make_task(work)
                    if task is not None:
                        candidates.append((work, task))
                candidates.sort(key=lambda pair: pair[1].submit_seq)
                if not candidates:
                    break

                admitted: list[tuple[TierWork, ScheduledTask]] = []
                waiting: list[tuple[TierWork, ScheduledTask, str]] = []
                allocated = 0
                slots_left = self.budget.global_parallelism
                for work, task in candidates:
                    if slots_left <= 0:
                        waiting.append((work, task, "parallelism"))
                        task.wait_rounds += 1
                        task.wait_ms += max(
                            1, self.timeline.slice_duration_ns // 1_000_000
                        )
                        continue
                    if (
                        allocated + task.plan.estimated_bytes
                        > self.budget.total_memory_bytes
                    ):
                        waiting.append((work, task, "memory"))
                        task.wait_rounds += 1
                        task.wait_ms += max(
                            1, self.timeline.slice_duration_ns // 1_000_000
                        )
                        continue
                    admitted.append((work, task))
                    allocated += task.plan.estimated_bytes
                    slots_left -= 1

                if not admitted:
                    if any(
                        task.plan.estimated_bytes <= self.budget.total_memory_bytes
                        for _, task in waiting
                    ):
                        smallest = min(
                            waiting,
                            key=lambda pair: (
                                pair[1].plan.estimated_bytes,
                                pair[1].submit_seq,
                            ),
                        )
                        admitted.append(smallest)
                        waiting.remove(smallest)
                        allocated += smallest[1].plan.estimated_bytes
                    else:
                        break

                for _, task, _ in waiting:
                    if task.tier.tier_id in self._high_tier_ids:
                        task.gave_way += len(admitted)
                        if (
                            task.wait_rounds >= self.budget.starvation_rounds
                            and task.wait_ms >= self.budget.starvation_ms
                        ):
                            self._emit(
                                "starvation_warning",
                                gave_way_count=task.gave_way,
                                index=task.index,
                                tier=task.tier.tier_id,
                                wait_ms=task.wait_ms,
                                wait_rounds=task.wait_rounds,
                            )
                self.waiting_queue = [
                    task.waiting_dict() for _, task, _ in waiting
                ]
                for _, task, reason in waiting:
                    self._emit(
                        "task_waiting",
                        estimated_bytes=task.plan.estimated_bytes,
                        index=task.index,
                        reason=reason,
                        submit_seq=task.submit_seq,
                        tier=task.tier.tier_id,
                        wait_rounds=task.wait_rounds,
                    )
                self._record_memory(allocated)
                if not admitted:
                    break

                futures: dict[Future[SliceArtifact], tuple[TierWork, ScheduledTask]] = {}
                for work, task in admitted:
                    task.status = "running"
                    task.admitted_round = self._step
                    self._emit(
                        "task_started",
                        estimated_bytes=task.plan.estimated_bytes,
                        index=task.index,
                        submit_seq=task.submit_seq,
                        tier=task.tier.tier_id,
                    )
                    futures[executor.submit(self._execute, work, task)] = (work, task)
                for future in futures:
                    try:
                        future.result()
                    except BaseException:
                        pass

                completions: list[
                    tuple[ScheduledTask, SliceArtifact | None, BaseException | None]
                ] = []
                for future, (work, task) in futures.items():
                    try:
                        artifact = future.result()
                        completions.append((task, artifact, None))
                    except BaseException as exc:
                        completions.append((task, None, exc))
                completions.sort(key=lambda item: item[0].submit_seq)
                for task, artifact, error in completions:
                    work = self.works[task.tier.tier_id]
                    if error is not None:
                        if isinstance(error, BusinessError):
                            business_error = error
                        else:
                            business_error = BusinessError(
                                ErrorCode.IO,
                                "slice task failed",
                                {
                                    "error": repr(error),
                                    "tier": task.tier.tier_id,
                                    "slice_index": task.index,
                                },
                            )
                        self._emit(
                            "task_failed",
                            error=business_error.to_dict(),
                            index=task.index,
                            submit_seq=task.submit_seq,
                            tier=task.tier.tier_id,
                        )
                        self._halt(work, business_error, task=task)
                        continue
                    assert artifact is not None
                    work.state.artifacts.append(artifact)
                    work.state.status = "running"
                    work.current_task = None
                    self._emit(
                        "task_completed",
                        content_hash=artifact.content_hash,
                        index=task.index,
                        submit_seq=task.submit_seq,
                        tier=task.tier.tier_id,
                    )
                    if work.next_index >= len(self.timeline.boundaries):
                        work.state.status = "complete"
                    work.cache.save(work.state, self._config_version)
                for work, _ in admitted:
                    if not work.halted and work.next_index >= len(self.timeline.boundaries):
                        work.state.status = "complete"
                        work.cache.save(work.state, self._config_version)
                self._record_memory(0)
                self._step += 1
        finally:
            executor.shutdown(wait=True)

        errors: list[dict[str, Any]] = []
        for work in self.works.values():
            if work.error is not None and work.error.get("code") != ErrorCode.CANCELLED:
                errors.append(work.error)
        for work in self.works.values():
            if work.current_task is not None and not work.halted:
                self.waiting_queue.append(work.current_task.waiting_dict())
        return SchedulerResult(
            tiers=self.works,
            events=self.events,
            memory_samples=self.memory_samples,
            peak_bytes=self.peak_bytes,
            waiting_queue=self.waiting_queue,
            cancelled=self.control.cancelled,
            errors=errors,
        )
