from __future__ import annotations

import threading
from collections.abc import Callable
from concurrent.futures import Future, ThreadPoolExecutor, wait
from dataclasses import dataclass, field
from typing import Any

from .budget import BudgetDecision
from .errors import BusinessError, ErrorCode
from .models import AppConfig, SliceBoundary


@dataclass(frozen=True)
class SliceTask:
    submission_seq: int
    track_id: str
    track_index: int
    slice_index: int
    boundary: SliceBoundary


@dataclass(frozen=True)
class PreparedTask:
    task: SliceTask
    budget: BudgetDecision
    artifacts: Any


@dataclass
class SchedulerResult:
    completed: dict[str, int]
    failed_tracks: dict[str, dict[str, Any]]
    committed_tasks: int
    memory_high_water_bytes: int
    cancelled: bool
    events: list[dict[str, Any]]

    def to_dict(self) -> dict[str, Any]:
        return {
            "completed_slice_counts": dict(sorted(self.completed.items())),
            "failed_tracks": dict(sorted(self.failed_tracks.items())),
            "committed_tasks": self.committed_tasks,
            "memory_high_water_bytes": self.memory_high_water_bytes,
            "cancelled": self.cancelled,
            "event_count": len(self.events),
        }


@dataclass
class _TaskState:
    task: SliceTask
    status: str = "queued"
    budget: BudgetDecision | None = None
    prepared: PreparedTask | None = None
    error: dict[str, Any] | None = None
    events: list[dict[str, Any]] = field(default_factory=list)
    memory_wait_recorded: bool = False
    next_event_serial: int = 0
    event_sink: Callable[[dict[str, Any]], None] | None = None

    def event(self, name: str, **details: Any) -> None:
        value = {
            "event": name,
            "task_seq": self.task.submission_seq,
            "track_id": self.task.track_id,
            "slice_index": self.task.slice_index,
            "serial": self.next_event_serial,
            "details": details,
        }
        self.events.append(value)
        self.next_event_serial += 1
        if self.event_sink is not None:
            self.event_sink(value)


class SliceScheduler:
    """Stable submission-order scheduler with cross-track parallel preparation.

    Workers prepare at most one slice per track. Publication is serialized in global
    submission order, so the trace and the per-track index cannot disagree. Later
    runnable tasks may use spare workers only if no earlier runnable task is memory-blocked.
    """

    def __init__(
        self,
        config: AppConfig,
        tasks: list[SliceTask],
        prepare: Callable[[SliceTask, BudgetDecision, threading.Event], Any],
        commit: Callable[[PreparedTask], None],
        choose_budget: Callable[[SliceTask], BudgetDecision],
        initial_completed: dict[str, int] | None = None,
        initial_failed: dict[str, dict[str, Any]] | None = None,
    ) -> None:
        self.config = config
        self.tasks = sorted(tasks, key=lambda task: task.submission_seq)
        self.prepare = prepare
        self.commit = commit
        self.choose_budget = choose_budget
        self.initial_completed = dict(initial_completed or {})
        self.initial_failed = dict(initial_failed or {})
        self.states = {task.submission_seq: _TaskState(task=task) for task in self.tasks}
        self._event_log: list[dict[str, Any]] = []
        for state in self.states.values():
            state.event_sink = self._event_log.append
        self.track_status = {
            track.track_id: ("failed" if track.track_id in self.initial_failed else "pending")
            for track in config.tracks
        }
        self.active: dict[Future[Any], int] = {}
        self.reserved_memory = 0
        self.high_water = 0
        self.pause_event = threading.Event()
        self.cancel_event = threading.Event()
        self.committed = 0
        self.completed_counts = dict(self.initial_completed)
        self.failed_tracks = dict(self.initial_failed)
        self._run_events: list[dict[str, Any]] = []
        self._lock = threading.Lock()
        self._stop_requested = False

    def pause(self) -> None:
        self.pause_event.set()

    def resume(self) -> None:
        self.pause_event.clear()

    def cancel(self) -> None:
        self.cancel_event.set()
        self.pause_event.clear()

    def request_stop(self) -> None:
        self._stop_requested = True

    def _run_event(self, name: str, **details: Any) -> None:
        self._run_events.append({"event": name, "details": details})

    def _head_tasks(self) -> list[_TaskState]:
        heads: dict[str, _TaskState] = {}
        for state in self.states.values():
            if state.status != "queued":
                continue
            current = heads.get(state.task.track_id)
            if current is None or state.task.slice_index < current.task.slice_index:
                heads[state.task.track_id] = state
        return sorted(heads.values(), key=lambda state: state.task.submission_seq)

    def _resolved(self, state: _TaskState) -> bool:
        return state.status in {"committed", "failed", "cancelled", "skipped"}

    def _fail_track(self, track_id: str, error: dict[str, Any]) -> None:
        if track_id not in self.failed_tracks:
            self.failed_tracks[track_id] = error
        self.track_status[track_id] = "failed"
        for state in self.states.values():
            if state.task.track_id == track_id and state.status == "queued":
                state.status = "skipped"
                state.error = {
                    "code": ErrorCode.BUDGET_EXCEEDED
                    if error.get("code") == ErrorCode.BUDGET_EXCEEDED
                    else error.get("code", ErrorCode.INTERNAL),
                    "message": "track stopped after an earlier slice failure",
                    "context": {
                        "track_id": track_id,
                        "slice_index": state.task.slice_index,
                        "blocked_by_slice": error.get("context", {}).get("slice_index"),
                    },
                }
                state.event("skipped_after_failure", code=state.error["code"])

    def _budget_decision(self, state: _TaskState) -> BudgetDecision | None:
        try:
            decision = self.choose_budget(state.task)
        except BusinessError as exc:
            state.status = "failed"
            state.error = exc.to_dict()
            state.event(
                "budget_rejected",
                code=exc.code,
                context=exc.context,
            )
            self._fail_track(state.task.track_id, state.error)
            return None
        state.budget = decision
        state.event(
            "budget_selected",
            stage=decision.stage.name,
            estimated_bytes=decision.estimated_bytes,
            degrade_stage=decision.degraded_stage_index,
            attempts=[attempt.to_dict() for attempt in decision.attempts],
        )
        if decision.degraded_stage_index:
            state.event(
                "degraded",
                stage=decision.stage.name,
                stage_index=decision.degraded_stage_index,
                estimated_bytes=decision.estimated_bytes,
                components=decision.components,
            )
        return decision

    def _publish_ready(self) -> bool:
        progress = False
        while True:
            remaining = [state for state in self.states.values() if not self._resolved(state)]
            if not remaining:
                return progress
            state = min(remaining, key=lambda item: item.task.submission_seq)
            if state.status == "running":
                return progress
            if state.status == "prepared":
                if self.cancel_event.is_set() or self._stop_requested:
                    state.status = "cancelled"
                    state.event("cancelled_before_commit")
                    self.reserved_memory -= state.budget.estimated_bytes if state.budget else 0
                    state.prepared = None
                    progress = True
                    continue
                assert state.prepared is not None
                try:
                    self.commit(state.prepared)
                except BusinessError as exc:
                    state.status = "failed"
                    state.error = exc.to_dict()
                    state.event("commit_failed", code=exc.code, context=exc.context)
                    self._fail_track(state.task.track_id, state.error)
                except Exception as exc:  # pragma: no cover - defensive
                    error = {
                        "ok": False,
                        "code": ErrorCode.INTERNAL,
                        "message": str(exc),
                        "context": {"track_id": state.task.track_id, "slice_index": state.task.slice_index},
                    }
                    state.status = "failed"
                    state.error = error
                    state.event("commit_failed", code=ErrorCode.INTERNAL, detail=str(exc))
                    self._fail_track(state.task.track_id, error)
                else:
                    state.status = "committed"
                    state.event(
                        "published",
                        stage=state.budget.stage.name if state.budget else None,
                        estimated_bytes=state.budget.estimated_bytes if state.budget else 0,
                    )
                    self.committed += 1
                    self.completed_counts[state.task.track_id] = state.task.slice_index + 1
                    self.track_status[state.task.track_id] = "running"
                finally:
                    self.reserved_memory -= state.budget.estimated_bytes if state.budget else 0
                    state.prepared = None
                progress = True
                continue
            if state.status == "queued":
                return progress
            if self._resolved(state):
                progress = True
                continue
        return progress

    def _handle_future(self, future: Future[Any]) -> None:
        task_seq = self.active.pop(future)
        state = self.states[task_seq]
        try:
            artifacts = future.result()
        except BusinessError as exc:
            if exc.code == ErrorCode.CANCELLED or self.cancel_event.is_set():
                state.status = "cancelled"
                state.error = exc.to_dict()
                state.event("cancelled_during_prepare", code=exc.code)
            else:
                state.status = "failed"
                state.error = exc.to_dict()
                state.event("prepare_failed", code=exc.code, context=exc.context)
                self._fail_track(state.task.track_id, state.error)
        except Exception as exc:  # pragma: no cover - defensive
            error = {
                "ok": False,
                "code": ErrorCode.INTERNAL,
                "message": str(exc),
                "context": {"track_id": state.task.track_id, "slice_index": state.task.slice_index},
            }
            state.status = "failed"
            state.error = error
            state.event("prepare_failed", code=ErrorCode.INTERNAL, detail=str(exc))
            self._fail_track(state.task.track_id, error)
        else:
            state.status = "prepared"
            state.prepared = PreparedTask(
                task=state.task,
                budget=state.budget,  # type: ignore[arg-type]
                artifacts=artifacts,
            )
            state.event("prepared", output_bytes=getattr(artifacts, "output_bytes", None))

    def _launch_ready(self, executor: ThreadPoolExecutor) -> bool:
        if self.pause_event.is_set() or self.cancel_event.is_set() or self._stop_requested:
            return False
        progress = False
        busy_tracks = {
            state.task.track_id
            for state in self.states.values()
            if state.status in {"running", "prepared"}
        }
        for state in self._head_tasks():
            if len(self.active) >= self.config.global_parallelism:
                break
            if self.track_status.get(state.task.track_id) == "failed":
                continue
            if state.task.track_id in busy_tracks:
                continue
            if state.status != "queued":
                continue
            decision = state.budget or self._budget_decision(state)
            if decision is None or state.status != "queued":
                progress = True
                continue
            if self.reserved_memory + decision.estimated_bytes > self.config.memory_budget_bytes:
                if not state.memory_wait_recorded:
                    state.memory_wait_recorded = True
                    state.event(
                        "memory_wait",
                        estimated_bytes=decision.estimated_bytes,
                        memory_budget_bytes=self.config.memory_budget_bytes,
                    )
                break
            state.status = "running"
            state.event(
                "admitted",
                estimated_bytes=decision.estimated_bytes,
                stage=decision.stage.name,
            )
            self.reserved_memory += decision.estimated_bytes
            self.high_water = max(self.high_water, self.reserved_memory)
            future = executor.submit(self.prepare, state.task, decision, self.cancel_event)
            self.active[future] = state.task.submission_seq
            busy_tracks.add(state.task.track_id)
            progress = True
        return progress

    def run(self) -> SchedulerResult:
        self._run_event(
            "run_start",
            track_order=[track.track_id for track in self.config.tracks],
            task_count=len(self.tasks),
            global_parallelism=self.config.global_parallelism,
            memory_budget_bytes=self.config.memory_budget_bytes,
            scheduling_policy="cross_track_wave_barrier",
        )
        for state in self.states.values():
            state.event("queued")
        with ThreadPoolExecutor(max_workers=self.config.global_parallelism, thread_name_prefix="slice") as executor:
            while True:
                if self.active:
                    done, _ = wait(set(self.active), return_when="ALL_COMPLETED")
                    for future in sorted(done, key=lambda item: self.active[item]):
                        self._handle_future(future)
                    self._publish_ready()
                    continue
                unresolved = [state for state in self.states.values() if not self._resolved(state)]
                if not unresolved:
                    break
                if self.cancel_event.is_set() or self._stop_requested:
                    for state in unresolved:
                        if state.status == "queued":
                            state.status = "cancelled"
                            state.event("cancelled")
                    break
                if self.pause_event.is_set():
                    # Pause is a boundary checkpoint. Remaining tasks stay queued for resume().
                    break
                self._launch_ready(executor)
                if self.active:
                    continue
                launchable = [
                    state
                    for state in self._head_tasks()
                    if self.track_status.get(state.task.track_id) != "failed"
                ]
                if not launchable:
                    for state in unresolved:
                        if state.status == "queued":
                            state.status = "skipped"
                            state.event("skipped_without_runnable_head")
                    break
        self._publish_ready()
        cancelled = self.cancel_event.is_set()
        if cancelled:
            for state in self.states.values():
                if state.status == "queued":
                    state.status = "cancelled"
                    state.event("cancelled")
        self._run_event(
            "run_end",
            committed_tasks=self.committed,
            cancelled=cancelled,
            failed_tracks=sorted(self.failed_tracks),
        )
        events = self.canonical_events()
        return SchedulerResult(
            completed=dict(self.completed_counts),
            failed_tracks=dict(self.failed_tracks),
            committed_tasks=self.committed,
            memory_high_water_bytes=self.high_water,
            cancelled=cancelled,
            events=events,
        )

    def canonical_events(self) -> list[dict[str, Any]]:
        starts = [event for event in self._run_events if event["event"] == "run_start"]
        ends = [event for event in self._run_events if event["event"] == "run_end"]
        middle = [event for event in self._run_events if event["event"] not in {"run_start", "run_end"}]
        return starts + middle + list(self._event_log) + ends

    def queue_snapshot(self) -> list[dict[str, Any]]:
        queue = []
        for state in sorted(self.states.values(), key=lambda item: item.task.submission_seq):
            if state.status in {"queued", "running", "prepared"}:
                queue.append(
                    {
                        "task_seq": state.task.submission_seq,
                        "track_id": state.task.track_id,
                        "slice_index": state.task.slice_index,
                        "status": state.status,
                        "estimated_bytes": state.budget.estimated_bytes if state.budget else None,
                    }
                )
        return queue

    def memory_reserved(self) -> int:
        with self._lock:
            return self.reserved_memory
