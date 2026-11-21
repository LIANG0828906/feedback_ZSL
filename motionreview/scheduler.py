"""Cross-clip parallel / within-clip serial scheduler with committed logs."""

from __future__ import annotations

import threading
from concurrent.futures import Future, ThreadPoolExecutor, wait
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Sequence

from .budget import BudgetPlan, plan_budget
from .bvh import BVHDocument
from .config import RunConfig
from .constraints import ConstraintSet
from .errors import (
    BudgetExceededError,
    BusyError,
    CommitFailedError,
    MotionReviewError,
    RecoveryInvalidError,
)
from .hashing import canonical_json_bytes
from .solver import FrameSolution, PoseSolver
from .storage import RecoveryState, SnapshotStore


@dataclass(frozen=True)
class ClipTask:
    clip_id: str
    document: BVHDocument
    constraints: ConstraintSet
    resume: bool = False

    def __post_init__(self) -> None:
        if self.document.clip_id != self.clip_id:
            raise ValueError(
                "ClipTask clip_id must match BVHDocument.clip_id"
            )
        if self.constraints.document_hash != self.document.content_hash:
            raise ValueError("constraints do not match task document")


@dataclass
class TaskReport:
    clip_id: str
    status: str
    start_frame: int = 0
    next_frame: int = 0
    committed_frames: int = 0
    committed_intervals: int = 0
    frames_solved: int = 0
    frames_reused: int = 0
    stage_name: str = "full"
    stage_index: int = 0
    usage_bytes: int = 0
    degraded: bool = False
    conflict_frame: int | None = None
    error: dict[str, Any] | None = None
    recovery: dict[str, Any] | None = None
    plan: dict[str, Any] | None = None
    trace_path: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "clip_id": self.clip_id,
            "status": self.status,
            "start_frame": self.start_frame,
            "next_frame": self.next_frame,
            "committed_frames": self.committed_frames,
            "committed_intervals": self.committed_intervals,
            "frames_solved": self.frames_solved,
            "frames_reused": self.frames_reused,
            "stage_name": self.stage_name,
            "stage_index": self.stage_index,
            "usage_bytes": self.usage_bytes,
            "degraded": self.degraded,
            "conflict_frame": self.conflict_frame,
            "error": self.error,
            "recovery": self.recovery,
            "plan": self.plan,
            "trace_path": self.trace_path,
        }


@dataclass
class RunReport:
    config_fingerprint: str
    parallelism: int
    reports: list[TaskReport] = field(default_factory=list)
    trace_path: str | None = None

    @property
    def all_completed(self) -> bool:
        return all(
            report.status in {"COMPLETED", "RECOVERED_COMPLETED"}
            for report in self.reports
        )

    @property
    def any_conflict(self) -> bool:
        return any(
            report.status == "CONFLICT_STOPPED"
            for report in self.reports
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "config_fingerprint": self.config_fingerprint,
            "parallelism": self.parallelism,
            "all_completed": self.all_completed,
            "any_conflict": self.any_conflict,
            "reports": [report.to_dict() for report in self.reports],
            "trace_path": self.trace_path,
        }


class Scheduler:
    """Stable two-phase scheduler.

    Phase one plans each clip without launching it. Phase two runs at most
    ``max_parallelism`` clips, one serial frame stream inside every clip.
    Global memory reservations are estimates from the selected budget plan.
    """

    def __init__(
        self,
        config: RunConfig,
        output_root: str | Path,
        *,
        checkpoint_hook: (
            Any | None
        ) = None,
    ) -> None:
        self.config = config
        self.output_root = Path(output_root)
        self.output_root.mkdir(parents=True, exist_ok=True)
        self._executor = ThreadPoolExecutor(
            max_workers=config.max_parallelism,
            thread_name_prefix="motionreview",
        )
        self._state_lock = threading.RLock()
        self._memory_condition = threading.Condition(self._state_lock)
        self._active_clips: set[str] = set()
        self._reserved_bytes = 0
        self._events: list[dict[str, Any]] = []
        self._frame_records: list[dict[str, Any]] = []
        self._pause_events: dict[str, threading.Event] = {}
        self._cancel_events: dict[str, threading.Event] = {}
        self._futures: dict[str, Future[TaskReport]] = {}
        self._reports: dict[str, TaskReport] = {}
        self._closed = False
        self.checkpoint_hook = checkpoint_hook

    def _new_clip_events(self, clip_id: str) -> None:
        with self._state_lock:
            self._pause_events[clip_id] = threading.Event()
            self._pause_events[clip_id].set()
            self._cancel_events[clip_id] = threading.Event()

    def _event(self, clip_id: str, event_type: str, **fields: Any) -> None:
        record = {
            "schema": "scheduler-event-v1",
            "type": event_type,
            "clip_id": clip_id,
            "parallelism": self.config.max_parallelism,
            **fields,
        }
        with self._state_lock:
            self._events.append(record)

    def _claim(self, clip_id: str) -> None:
        with self._state_lock:
            if clip_id in self._active_clips:
                raise BusyError(
                    "clip is already running", clip_id=clip_id
                )
            self._active_clips.add(clip_id)

    def _release_claim(self, clip_id: str) -> None:
        with self._state_lock:
            self._active_clips.discard(clip_id)
            self._pause_events.pop(clip_id, None)
            self._cancel_events.pop(clip_id, None)

    def start_clip(
        self,
        task: ClipTask,
        *,
        plan: BudgetPlan | None = None,
    ) -> Future[TaskReport]:
        """Start one clip; repeated active starts return ``E_BUSY``."""

        if self._closed:
            raise RuntimeError("scheduler is closed")
        self._claim(task.clip_id)
        self._new_clip_events(task.clip_id)
        try:
            selected_plan = plan or plan_budget(
                config=self.config,
                joints=len(task.document.joints),
                frames=task.document.frame_count,
                channels=task.document.channel_count,
                clip_id=task.clip_id,
            )
        except Exception:
            self._release_claim(task.clip_id)
            raise
        self._event(
            task.clip_id,
            "budget_planned",
            stage_name=selected_plan.stage_name,
            stage_index=selected_plan.stage_index,
            usage_bytes=selected_plan.estimate.total_bytes,
            attempted=list(selected_plan.attempted),
        )
        future = self._executor.submit(
            self._execute_task, task, selected_plan
        )
        self._futures[task.clip_id] = future
        future.add_done_callback(
            lambda completed, clip_id=task.clip_id: self._handle_done(
                clip_id, completed
            )
        )
        return future

    def _handle_done(
        self, clip_id: str, future: Future[TaskReport]
    ) -> None:
        try:
            report = future.result()
        except Exception as exc:
            report = TaskReport(
                clip_id=clip_id,
                status="FAILED",
                error=(
                    exc.to_dict()
                    if isinstance(exc, MotionReviewError)
                    else {"code": "E_INTERNAL", "message": str(exc)}
                ),
            )
        with self._state_lock:
            self._reports[clip_id] = report
        self._release_claim(clip_id)

    def run(
        self,
        tasks: Sequence[ClipTask],
        *,
        strict_busy: bool = False,
    ) -> RunReport:
        """Run a batch and return deterministic per-clip reports."""

        futures: dict[str, Future[TaskReport]] = {}
        reports: list[TaskReport] = []
        duplicate = set()
        prepared: list[tuple[ClipTask, BudgetPlan]] = []
        # Phase one: plan every clip before launching any worker.
        for task in tasks:
            if task.clip_id in duplicate:
                busy = BusyError(
                    "clip was submitted more than once", clip_id=task.clip_id
                )
                if strict_busy:
                    raise busy
                reports.append(
                    TaskReport(
                        clip_id=task.clip_id,
                        status="BUSY",
                        error=busy.to_dict(),
                    )
                )
                continue
            duplicate.add(task.clip_id)
            try:
                selected_plan = plan_budget(
                    config=self.config,
                    joints=len(task.document.joints),
                    frames=task.document.frame_count,
                    channels=task.document.channel_count,
                    clip_id=task.clip_id,
                )
            except BudgetExceededError as exc:
                reports.append(
                    TaskReport(
                        clip_id=task.clip_id,
                        status="REJECTED_BUDGET",
                        error=exc.to_dict(),
                    )
                )
                self._event(
                    task.clip_id,
                    "budget_rejected",
                    error=exc.to_dict(),
                )
            else:
                prepared.append((task, selected_plan))
        # Phase two: submit only fully planned clips.
        for task, selected_plan in prepared:
            try:
                futures[task.clip_id] = self.start_clip(
                    task, plan=selected_plan
                )
            except BusyError as exc:
                if strict_busy:
                    raise
                reports.append(
                    TaskReport(
                        clip_id=task.clip_id,
                        status="BUSY",
                        error=exc.to_dict(),
                    )
                )
        if futures:
            wait(list(futures.values()))
        for clip_id, future in futures.items():
            try:
                reports.append(future.result())
            except Exception as exc:
                reports.append(
                    TaskReport(
                        clip_id=clip_id,
                        status="FAILED",
                        error=(
                            exc.to_dict()
                            if isinstance(exc, MotionReviewError)
                            else {"code": "E_INTERNAL", "message": str(exc)}
                        ),
                    )
                )
        reports.sort(key=lambda report: report.clip_id)
        trace_path = self._write_aggregate_trace()
        return RunReport(
            config_fingerprint=self.config.fingerprint,
            parallelism=self.config.max_parallelism,
            reports=reports,
            trace_path=str(trace_path),
        )

    def pause(self, clip_id: str) -> None:
        with self._state_lock:
            event = self._pause_events.get(clip_id)
            if event is None:
                return
            event.clear()
        self._event(clip_id, "pause_requested")

    def resume_from_pause(self, clip_id: str) -> None:
        with self._state_lock:
            event = self._pause_events.get(clip_id)
            if event is None:
                return
            event.set()
            self._memory_condition.notify_all()
        self._event(clip_id, "pause_released")

    def cancel(self, clip_id: str) -> None:
        with self._state_lock:
            event = self._cancel_events.get(clip_id)
            if event is None:
                return
            event.set()
            self._memory_condition.notify_all()
        self._event(clip_id, "cancel_requested")

    def wait_clip(self, clip_id: str) -> TaskReport:
        with self._state_lock:
            future = self._futures.get(clip_id)
        if future is None:
            with self._state_lock:
                if clip_id in self._reports:
                    return self._reports[clip_id]
            raise KeyError(clip_id)
        return future.result()

    def _reserve_memory(
        self, clip_id: str, plan: BudgetPlan
    ) -> bool:
        needed = plan.estimate.total_bytes
        if needed > self.config.total_memory_bytes:
            raise BudgetExceededError(
                "task cannot fit even an empty global memory quota",
                clip_id=clip_id,
                details={
                    "usage_bytes": needed,
                    "total_memory_bytes": self.config.total_memory_bytes,
                },
            )
        waiting_logged = False
        with self._memory_condition:
            while self._reserved_bytes + needed > self.config.total_memory_bytes:
                cancel = self._cancel_events[clip_id]
                if cancel.is_set():
                    return False
                if not waiting_logged:
                    waiting_logged = True
                    self._event(
                        clip_id,
                        "wait_queue",
                        usage_bytes=needed,
                        reserved_bytes=self._reserved_bytes,
                        total_memory_bytes=self.config.total_memory_bytes,
                    )
                self._memory_condition.wait(timeout=0.05)
            self._reserved_bytes += needed
            self._event(
                clip_id,
                "memory_reserved",
                usage_bytes=needed,
                reserved_bytes=self._reserved_bytes,
                total_memory_bytes=self.config.total_memory_bytes,
            )
            return True

    def _release_memory(
        self, clip_id: str, plan: BudgetPlan
    ) -> None:
        with self._memory_condition:
            self._reserved_bytes -= plan.estimate.total_bytes
            self._event(
                clip_id,
                "memory_released",
                usage_bytes=plan.estimate.total_bytes,
                reserved_bytes=self._reserved_bytes,
            )
            self._memory_condition.notify_all()

    def _checkpoint(
        self, clip_id: str, frame: int
    ) -> str:
        pause = self._pause_events[clip_id]
        cancel = self._cancel_events[clip_id]
        logged_pause = False
        while not pause.is_set():
            if cancel.is_set():
                return "CANCEL"
            if not logged_pause:
                self._event(clip_id, "paused")
                logged_pause = True
            pause.wait(timeout=0.05)
        if cancel.is_set():
            return "CANCEL"
        if self.checkpoint_hook is not None:
            self.checkpoint_hook(clip_id, frame)
            if cancel.is_set():
                return "CANCEL"
        return "RUN"

    def _execute_task(
        self, task: ClipTask, plan: BudgetPlan
    ) -> TaskReport:
        report = TaskReport(
            clip_id=task.clip_id,
            status="RUNNING",
            stage_name=plan.stage_name,
            stage_index=plan.stage_index,
            usage_bytes=plan.estimate.total_bytes,
            degraded=plan.stage_index > 0,
            plan=plan.to_dict(),
        )
        reserved = False
        solver: PoseSolver | None = None
        store = SnapshotStore(self.output_root, task.clip_id)
        trace_records: list[dict[str, Any]] = []
        try:
            solver = PoseSolver(
                task.document,
                task.constraints,
                max_iterations=self.config.max_iterations,
                cache_depth=plan.cache_depth,
            )
            if task.resume:
                recovery = store.recover(
                    expected_parameter_version=solver.parameter_version,
                    expected_document_hash=task.document.content_hash,
                )
            elif store.recover().has_commits:
                raise RecoveryInvalidError(
                    "existing committed intervals require resume=True",
                    clip_id=task.clip_id,
                    frame=0,
                )
            else:
                recovery = store.recover()
            report.start_frame = recovery.next_frame
            report.next_frame = recovery.next_frame
            report.recovery = {
                "next_frame": recovery.next_frame,
                "head_hash": recovery.head_hash,
                "seq": recovery.seq,
                "committed_frames": len(recovery.solutions),
            }
            if recovery.next_frame > task.document.frame_count:
                raise RecoveryInvalidError(
                    "snapshot commits exceed document frame count",
                    clip_id=task.clip_id,
                    frame=recovery.next_frame,
                )
            seeded = solver.seed_solutions(recovery.solutions)
            report.frames_reused = seeded
            self._event(
                task.clip_id,
                "recovery_loaded",
                next_frame=recovery.next_frame,
                seeded_frames=seeded,
                head_hash=recovery.head_hash,
            )
            if recovery.next_frame >= task.document.frame_count:
                report.status = "RECOVERED_COMPLETED"
                report.next_frame = task.document.frame_count
                report.committed_frames = len(recovery.solutions)
                report.committed_intervals = len(recovery.intervals)
                trace_records.extend(
                    self._frame_trace_for_recovery(task, plan, recovery)
                )
                return report

            reserved = self._reserve_memory(task.clip_id, plan)
            if not reserved:
                report.status = "CANCELLED"
                return report

            cursor = recovery.next_frame
            interval_size = min(
                plan.batch_frames, self.config.checkpoint_frames
            )
            while cursor < task.document.frame_count:
                checkpoint = self._checkpoint(task.clip_id, cursor)
                if checkpoint == "CANCEL":
                    report.status = "CANCELLED"
                    report.next_frame = cursor
                    break
                start = cursor
                end = min(
                    task.document.frame_count,
                    start + max(1, interval_size),
                )
                batch = solver.solve_interval(start, end)
                report.frames_solved += len(batch.frames_solved)
                report.frames_reused += len(batch.frames_reused)
                conflict_index = next(
                    (
                        index
                        for index, solution in enumerate(batch.solutions)
                        if not solution.converged
                    ),
                    None,
                )
                safe_solutions = (
                    batch.solutions
                    if conflict_index is None
                    else batch.solutions[:conflict_index]
                )
                if safe_solutions:
                    interval_payload = store.commit_range(
                        safe_solutions,
                        parameter_version=solver.parameter_version,
                        document_hash=task.document.content_hash,
                        extra_dependencies={
                            "constraint_version": (
                                solver.constraints.parameter_version
                            ),
                            "algorithm": solver.algorithm_version,
                            "degradation_stage": plan.stage_name,
                        },
                    )
                    commit_hash = str(interval_payload["content_hash"])
                    chain_hash = str(interval_payload["chain_hash"])
                    for solution in safe_solutions:
                        trace_records.append(
                            self._frame_trace_record(
                                task,
                                plan,
                                solution,
                                commit_hash=commit_hash,
                                chain_hash=chain_hash,
                            )
                        )
                    report.committed_intervals += 1
                    report.committed_frames += len(safe_solutions)
                    cursor = safe_solutions[-1].frame + 1
                if conflict_index is not None:
                    conflict_solution = batch.solutions[conflict_index]
                    diagnostic = dict(conflict_solution.conflict or {})
                    diagnostic["diagnostic_path"] = str(
                        store.write_diagnostic(diagnostic)
                    )
                    self._event(
                        task.clip_id,
                        "constraint_conflict",
                        frame=conflict_solution.frame,
                        constraint_ids=list(
                            (conflict_solution.conflict or {}).get(
                                "constraint_ids", []
                            )
                        ),
                    )
                    report.status = "CONFLICT_STOPPED"
                    report.conflict_frame = conflict_solution.frame
                    report.next_frame = conflict_solution.frame
                    break
                if cursor == start:
                    raise CommitFailedError(
                        "solver made no progress across a frame interval",
                        clip_id=task.clip_id,
                        frame=start,
                    )
                report.next_frame = cursor
            else:
                report.status = "COMPLETED"
                report.next_frame = task.document.frame_count
            return report
        except Exception as exc:
            if isinstance(exc, MotionReviewError):
                report.error = exc.to_dict()
            else:
                report.error = {
                    "code": "E_INTERNAL",
                    "message": str(exc),
                }
            if isinstance(exc, BudgetExceededError):
                report.status = "REJECTED_BUDGET"
            elif isinstance(exc, RecoveryInvalidError):
                report.status = "RECOVERY_INVALID"
            elif isinstance(exc, CommitFailedError):
                report.status = "COMMIT_FAILED"
            elif isinstance(exc, BusyError):
                report.status = "BUSY"
            else:
                report.status = "FAILED"
            return report
        finally:
            if reserved:
                self._release_memory(task.clip_id, plan)
            if solver is not None:
                try:
                    report.trace_path = str(
                        store.write_trace(
                        sorted(
                            trace_records,
                            key=lambda item: (
                                int(item["frame"]),
                                str(item["constraint_version"]),
                            ),
                        )
                        )
                    )
                except Exception:
                    report.trace_path = None
            self._append_frame_records(trace_records)

    def _frame_trace_record(
        self,
        task: ClipTask,
        plan: BudgetPlan,
        solution: FrameSolution,
        *,
        commit_hash: str,
        chain_hash: str,
    ) -> dict[str, Any]:
        return {
            "schema": "frame-trace-v1",
            "type": "frame",
            "clip_id": task.clip_id,
            "frame": solution.frame,
            "parallelism": self.config.max_parallelism,
            "degradation_stage": plan.stage_name,
            "degradation_stage_index": plan.stage_index,
            "usage_bytes": plan.estimate.total_bytes,
            "residual": solution.total_residual,
            "residuals": [dict(record) for record in solution.residuals],
            "pose_hash": solution.pose_hash,
            "content_hash": solution.content_hash,
            "commit_content_hash": commit_hash,
            "commit_chain_hash": chain_hash,
            "parameter_version": solution.parameter_version,
            "constraint_version": task.constraints.parameter_version,
        }

    def _frame_trace_for_recovery(
        self,
        task: ClipTask,
        plan: BudgetPlan,
        recovery: RecoveryState,
    ) -> list[dict[str, Any]]:
        records: list[dict[str, Any]] = []
        for interval in recovery.intervals:
            for record in interval["records"]:
                records.append(
                    {
                        "schema": "frame-trace-v1",
                        "type": "frame",
                        "clip_id": task.clip_id,
                        "frame": int(record["frame"]),
                        "parallelism": self.config.max_parallelism,
                        "degradation_stage": "recovered",
                        "degradation_stage_index": plan.stage_index,
                        "usage_bytes": plan.estimate.total_bytes,
                        "residual": record["total_residual"],
                        "residuals": record["residuals"],
                        "pose_hash": record["pose_hash"],
                        "content_hash": record["content_hash"],
                        "commit_content_hash": interval["content_hash"],
                        "commit_chain_hash": interval["chain_hash"],
                        "parameter_version": record["parameter_version"],
                        "constraint_version": (
                            task.constraints.parameter_version
                        ),
                    }
                )
        return records

    def _append_frame_records(
        self, records: list[dict[str, Any]]
    ) -> None:
        with self._state_lock:
            self._frame_records.extend(records)

    def _write_aggregate_trace(self) -> Path:
        with self._state_lock:
            frames = sorted(
                self._frame_records,
                key=lambda item: (
                    str(item["clip_id"]),
                    int(item["frame"]),
                ),
            )
            events = sorted(
                self._events,
                key=lambda item: (
                    str(item["clip_id"]),
                    str(item["type"]),
                    canonical_json_bytes(item),
                ),
            )
        destination = self.output_root / "trace.jsonl"
        lines = [
            canonical_json_bytes(record).decode("utf-8")
            for record in [*frames, *events]
        ]
        data = ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8")
        temporary = destination.with_name(
            f".{destination.name}.tmp-{threading.get_ident()}"
        )
        temporary.write_bytes(data)
        temporary.replace(destination)
        return destination

    def close(self, *, wait: bool = True) -> None:
        self._closed = True
        with self._memory_condition:
            for event in self._cancel_events.values():
                event.set()
            for event in self._pause_events.values():
                event.set()
            self._memory_condition.notify_all()
        self._executor.shutdown(wait=wait, cancel_futures=False)

    def __enter__(self) -> "Scheduler":
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()
