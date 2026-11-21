from __future__ import annotations

import threading
import time
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .bvh import BVHData, load_bvh
from .constraints import (
    ConstraintSet,
    frames_to_ranges,
    load_constraints,
)
from .dependency import ImpactPlan, derive_impact
from .errors import E_CANCELLED
from .solver import (
    FrameSolution,
    FrameSolver,
    residual_digest,
)
from .store import AtomicStore, CommittedState


@dataclass
class SolveReport:
    status: str
    run_id: str
    plan: ImpactPlan
    state: CommittedState
    computed_frames: tuple[int, ...] = ()
    cached_frames: tuple[int, ...] = ()
    intervals: tuple[dict[str, Any], ...] = ()
    conflict: dict[str, Any] | None = None
    error_code: str | None = None
    trace_path: str | None = None

    @property
    def frames(self) -> dict[int, FrameSolution]:
        result: dict[int, FrameSolution] = {}
        for record in self.state.frame_map.values():
            for solution in record.frames:
                result[solution.frame] = solution
        return result

    def to_dict(self) -> dict[str, Any]:
        frames = self.frames
        return {
            "ok": self.error_code is None,
            "status": self.status,
            "error_code": self.error_code,
            "run_id": self.run_id,
            "plan": self.plan.to_dict(),
            "computed_frames": list(self.computed_frames),
            "cached_frames": list(self.cached_frames),
            "intervals": list(self.intervals),
            "conflict": self.conflict,
            "recovery": self.state.to_summary(),
            "frame_count": len(frames),
            "residual_digest": residual_digest(frames.values()),
            "trace_path": self.trace_path,
        }


class WorkbenchSession:
    def __init__(
        self,
        bvh_path: str | Path,
        constraints_path: str | Path | None,
        session_dir: str | Path,
        *,
        constraints: ConstraintSet | None = None,
        fault_hook=None,
        post_commit_hook=None,
    ):
        self.bvh_path = Path(bvh_path)
        self.constraints_path = (
            Path(constraints_path) if constraints_path is not None else None
        )
        self.session_dir = Path(session_dir)
        self.bvh_bytes = self.bvh_path.read_bytes()
        self.bvh: BVHData = load_bvh(self.bvh_path)
        if constraints is None:
            if self.constraints_path is None:
                raise ValueError("Either constraints_path or constraints is required")
            constraints = load_constraints(self.constraints_path, self.bvh)
        self.constraints: ConstraintSet = constraints
        self.store = AtomicStore(
            self.session_dir,
            fault_hook=fault_hook,
            post_commit_hook=post_commit_hook,
        )

    def set_constraints(self, constraints: ConstraintSet) -> None:
        constraints.validate(self.bvh)
        self.constraints = constraints

    def recover(
        self,
        *,
        expected_parameters_version: str | None = None,
        expected_table_version: int | None = None,
    ) -> CommittedState | None:
        if not self.store.manifest_path.exists():
            return None
        with self.store.solve_lock():
            return self.store.load_committed(
                self.bvh,
                constraints=self.constraints,
                expected_parameters_version=expected_parameters_version,
                expected_table_version=expected_table_version,
            )

    def _emit_progress(
        self,
        progress_callback,
        event: dict[str, Any],
    ) -> None:
        if progress_callback is not None:
            progress_callback(event)

    def solve(
        self,
        *,
        run_id: str = "run",
        cancel_event: threading.Event | None = None,
        pause_event: threading.Event | None = None,
        chunk_size: int = 8,
        progress_callback=None,
    ) -> SolveReport:
        if chunk_size < 1:
            raise ValueError("chunk_size must be positive")
        cancel_event = cancel_event or threading.Event()
        global_lock_root = (
            Path(tempfile.gettempdir())
            / "motion-workbench-locks"
            / self.bvh.content_digest[:24]
        )
        from .store import SolveLock

        global_lock = SolveLock(global_lock_root)
        with global_lock, self.store.solve_lock():
            manifest_existed = self.store.manifest_path.exists()
            previous_state: CommittedState | None = None
            if manifest_existed:
                previous_state = self.store.load_committed(
                    self.bvh, constraints=self.constraints
                )
            manifest, _ = self.store.initialize(
                self.bvh_bytes, self.bvh, self.constraints
            )
            if previous_state is None:
                plan = derive_impact(self.bvh, None, self.constraints)
            else:
                plan = derive_impact(
                    self.bvh, previous_state.parameters, self.constraints
                )
            self.store.set_parameters(self.bvh, self.constraints)
            state = self.store.load_committed(
                self.bvh, constraints=self.constraints
            )
            parameters_version = state.parameters_version
            solver = FrameSolver(self.bvh, self.constraints)

            all_frames = set(range(self.bvh.frame_count))
            active_frames = set(state.frame_map)
            stale_frames = set(state.stale_frames)
            missing_frames = all_frames - active_frames
            recompute = (
                plan.frame_set() | stale_frames | missing_frames
            )
            recompute &= all_frames
            recompute_ranges = frames_to_ranges(recompute)
            recompute_sorted = sorted(recompute)

            plan_event = {
                "event": "impact_plan",
                "run_id": run_id,
                "recomputed_ranges": [
                    list(pair) for pair in recompute_ranges
                ],
                "dependency_plan": plan.to_dict(),
                "stale_frames": sorted(stale_frames)[:200],
                "missing_ranges": [list(pair) for pair in state.missing_ranges],
                "parameters_version": parameters_version,
                "chain_head": state.chain_head,
            }
            self._emit_progress(progress_callback, plan_event)

            computed: list[int] = []
            committed_intervals: list[dict[str, Any]] = []
            pending: list[FrameSolution] = []
            conflict_diagnostic = None
            conflict_payload: dict[str, Any] | None = None
            cancelled = False
            trace_buffer: list[dict[str, Any]] = [plan_event]

            def flush_pending() -> None:
                nonlocal pending
                if not pending:
                    return
                record = self.store.commit_interval(
                    pending, self.bvh, parameters_version
                )
                committed_intervals.append(
                    {
                        "start": record.start,
                        "end": record.end,
                        "sequence": record.sequence,
                        "table_version": record.table_version,
                        "content_hash": record.content_hash,
                        "file": record.file,
                    }
                )
                interval_event = {
                    "event": "interval_committed",
                    "run_id": run_id,
                    "start": record.start,
                    "end": record.end,
                    "sequence": record.sequence,
                    "table_version": record.table_version,
                    "content_hash": record.content_hash,
                    "file": record.file,
                    "parameters_version": parameters_version,
                }
                trace_buffer.append(interval_event)
                for solution in pending:
                    trace_buffer.append(
                        self._frame_trace(
                            solution,
                            run_id,
                            recompute_ranges,
                            cache_hit=False,
                            conflict_status="none",
                        )
                    )
                self.store.append_trace(trace_buffer)
                trace_buffer.clear()
                self._emit_progress(
                    progress_callback,
                    {
                        "event": "progress",
                        "run_id": run_id,
                        "last_committed_frame": record.end,
                        "interval": interval_event,
                    },
                )
                pending = []

            for frame in recompute_sorted:
                if cancel_event.is_set():
                    flush_pending()
                    cancelled = True
                    break
                if pause_event is not None and pause_event.is_set():
                    flush_pending()
                    while (
                        pause_event is not None
                        and pause_event.is_set()
                        and not cancel_event.is_set()
                    ):
                        time.sleep(0.03)
                    if cancel_event.is_set():
                        cancelled = True
                        break
                if pending and pending[-1].frame + 1 != frame:
                    flush_pending()
                solution = solver.solve_frame(frame, run_id=run_id)
                if solution.conflict is not None or not solution.converged:
                    flush_pending()
                    conflict_diagnostic = solution.conflict
                    if conflict_diagnostic is None:
                        raise AssertionError("Unconverged frame lacks diagnostic")
                    conflict_payload = self.store.record_conflict(
                        conflict_diagnostic
                    )
                    trace_buffer.append(
                        {
                            "event": "conflict",
                            "run_id": run_id,
                            "frame": frame,
                            "recomputed_ranges": [
                                list(pair) for pair in recompute_ranges
                            ],
                            "conflict_status": "conflict",
                            "cache_hit": False,
                            "content_hash": None,
                            "diagnostic": conflict_diagnostic.to_dict(),
                            "report": conflict_payload,
                            "parameters_version": parameters_version,
                        }
                    )
                    self.store.append_trace(trace_buffer)
                    trace_buffer.clear()
                    self._emit_progress(
                        progress_callback,
                        {
                            "event": "conflict",
                            "run_id": run_id,
                            "frame": frame,
                            "diagnostic": conflict_diagnostic.to_dict(),
                            "report": conflict_payload,
                        },
                    )
                    break
                pending.append(solution)
                computed.append(frame)
                if len(pending) >= chunk_size:
                    flush_pending()

            if (
                not cancelled
                and conflict_diagnostic is None
                and pending
            ):
                flush_pending()

            final_state = self.store.load_committed(
                self.bvh, constraints=self.constraints
            )
            final_active = set(final_state.frame_map)
            cached_final = sorted(final_active - set(computed))
            for frame in cached_final:
                solution = final_state.frame_solution(frame)
                if solution is None:
                    continue
                trace_buffer.append(
                    self._frame_trace(
                        solution,
                        run_id,
                        recompute_ranges,
                        cache_hit=True,
                        conflict_status="none",
                    )
                )
            if trace_buffer:
                self.store.append_trace(trace_buffer)

            if conflict_diagnostic is not None:
                status = "conflict"
                error_code = "E_CONFLICT"
            elif cancelled:
                status = "cancelled"
                error_code = E_CANCELLED
            elif final_active >= all_frames and not final_state.stale_frames:
                status = "complete"
                error_code = None
            else:
                status = "partial"
                error_code = None

            report = SolveReport(
                status=status,
                run_id=run_id,
                plan=plan,
                state=final_state,
                computed_frames=tuple(sorted(set(computed))),
                cached_frames=tuple(cached_final),
                intervals=tuple(committed_intervals),
                conflict=(
                    None
                    if conflict_diagnostic is None
                    else {
                        "diagnostic": conflict_diagnostic.to_dict(),
                        "report": conflict_payload,
                    }
                ),
                error_code=error_code,
                trace_path=str(self.store.trace_path),
            )
            self._emit_progress(
                progress_callback,
                {
                    "event": "finished",
                    "run_id": run_id,
                    "status": status,
                    "report": report.to_dict(),
                },
            )
            return report

    def _frame_trace(
        self,
        solution: FrameSolution,
        run_id: str,
        recompute_ranges: tuple[tuple[int, int], ...],
        *,
        cache_hit: bool,
        conflict_status: str,
    ) -> dict[str, Any]:
        return {
            "event": "frame",
            "run_id": run_id,
            "frame": solution.frame,
            "recomputed_ranges": [list(pair) for pair in recompute_ranges],
            "residual": solution.total_residual,
            "raw_weighted_total": solution.raw_weighted_total,
            "residuals": [
                {
                    "constraint_id": residual.constraint_id,
                    "type": residual.type,
                    "joint": residual.joint,
                    "raw": residual.raw,
                    "tolerance": residual.tolerance,
                    "satisfied": residual.satisfied,
                }
                for residual in solution.residuals
            ],
            "conflict_status": conflict_status,
            "cache_hit": cache_hit,
            "content_hash": solution.frame_hash,
            "dependency_hash": solution.dependency_hash,
        }


def resume_session(
    session_dir: str | Path,
    *,
    fault_hook=None,
    post_commit_hook=None,
) -> WorkbenchSession:
    directory = Path(session_dir)
    store = AtomicStore(
        directory,
        fault_hook=fault_hook,
        post_commit_hook=post_commit_hook,
    )
    if not store.manifest_path.exists():
        from .errors import E_RECOVERY_INVALID, WorkbenchError

        raise WorkbenchError(
            E_RECOVERY_INVALID,
            "Session has no committed manifest",
            {"first_break_frame": 0, "session_dir": str(directory)},
        )
    source_path = store.source_path
    if not source_path.exists():
        from .errors import E_RECOVERY_INVALID, WorkbenchError

        raise WorkbenchError(
            E_RECOVERY_INVALID,
            "Session source BVH is missing",
            {"first_break_frame": 0, "session_dir": str(directory)},
        )
    bvh = load_bvh(source_path)
    constraints, _ = store.load_parameters(bvh)
    return WorkbenchSession(
        source_path,
        None,
        directory,
        constraints=constraints,
        fault_hook=fault_hook,
        post_commit_hook=post_commit_hook,
    )
