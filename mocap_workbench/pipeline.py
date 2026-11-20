"""End-to-end solve pipeline: adaptive solving, cache, snapshots and control."""

from __future__ import annotations

import os
import threading
import time
from collections.abc import Callable, Iterable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .bvh import BvhDocument, Pose, parse_bvh
from .constraints import ConstraintSet, load_constraints
from .errors import (
    E_BUDGET_EXCEEDED,
    E_CANCELLED,
    E_RECOVERY_INVALID,
    BusinessError,
)
from .locking import JobLock
from .math3d import hash_pose
from .snapshot import RecoveredFrame, RecoveryState, SnapshotStore
from .solver import (
    FrameResult,
    SolveCache,
    SolverConfig,
    estimate_memory_bytes,
    evaluate_residuals,
    select_strategy,
    solve_frames,
)


@dataclass
class ProgressEvent:
    status: str
    start_frame: int
    end_frame: int
    frame_count: int
    committed_frames: int
    conflicts: int
    cache_hits: int
    cache_misses: int
    strategy: str
    committed_ranges: tuple[tuple[int, int], ...] = ()
    frame_results: tuple[FrameResult, ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "start_frame": self.start_frame,
            "end_frame": self.end_frame,
            "frame_count": self.frame_count,
            "committed_frames": self.committed_frames,
            "conflicts": self.conflicts,
            "cache_hits": self.cache_hits,
            "cache_misses": self.cache_misses,
            "strategy": self.strategy,
            "committed_ranges": [list(item) for item in self.committed_ranges],
        }


@dataclass
class RunReport:
    status: str
    frame_count: int
    strategy: str
    results: list[FrameResult] = field(default_factory=list)
    committed_ranges: tuple[tuple[int, int], ...] = ()
    conflict_reports: list[dict[str, Any]] = field(default_factory=list)
    recovered: RecoveryState | None = None
    cache_stats: dict[str, int] = field(default_factory=dict)
    error_code: str | None = None
    message: str = ""
    crashed_after_chunks: int = 0
    constraints_revision: int = 1
    constraints_hash: str = ""

    @property
    def committed_frames(self) -> tuple[int, ...]:
        out: list[int] = []
        for start, end in self.committed_ranges:
            out.extend(range(start, end + 1))
        return tuple(out)

    @property
    def has_conflicts(self) -> bool:
        return bool(self.conflict_reports)

    def summary(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "error_code": self.error_code,
            "message": self.message,
            "frame_count": int(self.frame_count),
            "strategy": self.strategy,
            "constraints_revision": int(self.constraints_revision),
            "constraints_hash": self.constraints_hash,
            "committed_ranges": [list(item) for item in self.committed_ranges],
            "committed_frames": len(self.committed_frames),
            "processed_results": len(self.results),
            "conflicts": len(self.conflict_reports),
            "cache": dict(self.cache_stats),
            "recovery": self.recovered.summary() if self.recovered is not None else None,
        }


def _ranges_from_frames(frames: Iterable[int]) -> tuple[tuple[int, int], ...]:
    ordered = sorted({int(frame) for frame in frames})
    if not ordered:
        return ()
    out: list[tuple[int, int]] = []
    start = previous = ordered[0]
    for frame in ordered[1:]:
        if frame == previous + 1:
            previous = frame
            continue
        out.append((start, previous))
        start = previous = frame
    out.append((start, previous))
    return tuple(out)


def _pose_from_world(document: BvhDocument, world: np.ndarray) -> Pose:
    local = np.empty((document.joint_count, 3, 3), dtype=np.float64)
    for index, joint in enumerate(document.joints):
        if joint.parent is None:
            parent_rotation = np.eye(3, dtype=np.float64)
        else:
            parent_rotation = world[joint.parent, :3, :3]
        local[index] = parent_rotation.T @ world[index, :3, :3]
    return Pose(
        world=np.array(world, dtype=np.float64, copy=True),
        local_rotations=local,
        joint_positions=np.array(world[:, :3, 3], dtype=np.float64, copy=True),
    )


def _result_from_recovered(
    document: BvhDocument,
    constraints: ConstraintSet,
    recovered: RecoveredFrame,
    strategy: str,
) -> FrameResult:
    pose = _pose_from_world(document, recovered.transforms)
    stored_hash = recovered.pose_hash
    actual_hash = hash_pose(document.joint_names, pose.world)
    if stored_hash != actual_hash:
        raise BusinessError(
            E_RECOVERY_INVALID,
            "recovered pose hash does not match its transforms",
            {"frame": recovered.frame},
        )
    residuals, total, satisfied, conflicts, diagnostic = evaluate_residuals(
        document, constraints, recovered.frame, pose
    )
    return FrameResult(
        frame=recovered.frame,
        pose=pose,
        residuals=residuals,
        total_residual=total,
        satisfied=satisfied,
        conflicts=conflicts,
        pose_hash=stored_hash,
        cache_hit=False,
        strategy=strategy,
        iterations=0,
        diagnostic=diagnostic,
        restored=True,
    )


class SolvePipeline:
    """One run over one BVH and one constraint version.

    A pipeline instance is reusable for a fresh run or resume, but one
    :meth:`run` call maps to exactly one OS-level job lock.
    """

    def __init__(
        self,
        bvh_path: str | Path,
        constraints_path: str | Path,
        snapshot_dir: str | Path,
        config: SolverConfig | None = None,
        *,
        document: BvhDocument | None = None,
        constraints: ConstraintSet | None = None,
    ):
        self.bvh_path = str(Path(bvh_path))
        self.constraints_path = str(Path(constraints_path))
        self.snapshot_dir = Path(snapshot_dir)
        self.config = config or SolverConfig()
        self.config.validate()
        self._document = document
        self._constraints = constraints

    @property
    def document(self) -> BvhDocument:
        if self._document is None:
            self._document = parse_bvh(self.bvh_path)
        return self._document

    @property
    def constraints(self) -> ConstraintSet:
        if self._constraints is None:
            self._constraints = load_constraints(self.constraints_path, self.document)
        return self._constraints

    def _store(self) -> SnapshotStore:
        return SnapshotStore(
            self.snapshot_dir,
            self.document,
            self.constraints,
            self.config,
        )

    def _checkpoint(
        self,
        cancel_event: threading.Event | None,
        pause_event: threading.Event | None,
    ) -> None:
        if cancel_event is not None and cancel_event.is_set():
            raise BusinessError(
                E_CANCELLED,
                "solve task cancelled at an interval boundary",
            )
        while pause_event is not None and pause_event.is_set():
            if cancel_event is not None and cancel_event.is_set():
                raise BusinessError(
                    E_CANCELLED,
                    "solve task cancelled while paused",
                )
            time.sleep(0.02)

    def run(
        self,
        *,
        resume: bool = False,
        cancel_event: threading.Event | None = None,
        pause_event: threading.Event | None = None,
        progress: Callable[[ProgressEvent], None] | None = None,
        cache: SolveCache | None = None,
        crash_after_chunks: int = 0,
    ) -> RunReport:
        self.config.validate()
        lock = JobLock(self.bvh_path)
        lock.acquire()
        try:
            return self._run_locked(
                resume=resume,
                cancel_event=cancel_event,
                pause_event=pause_event,
                progress=progress,
                cache=cache,
                crash_after_chunks=crash_after_chunks,
            )
        finally:
            lock.release()

    def _run_locked(
        self,
        *,
        resume: bool,
        cancel_event: threading.Event | None,
        pause_event: threading.Event | None,
        progress: Callable[[ProgressEvent], None] | None,
        cache: SolveCache | None,
        crash_after_chunks: int,
    ) -> RunReport:
        document = self.document
        constraints = self.constraints
        constraints.validate(document)
        chosen = select_strategy(document, constraints, self.config)
        cache = cache or SolveCache(self.config.cache_max_entries)
        store = self._store()
        recovery: RecoveryState | None = None
        if resume:
            recovery = store.recover(require_manifest=False)
            start_frame = recovery.next_frame
        else:
            store.reset()
            store.open()
            start_frame = 0

        results: list[FrameResult] = []
        if recovery is not None:
            for frame in sorted(recovery.frames):
                results.append(
                    _result_from_recovered(
                        document, constraints, recovery.frames[frame], chosen
                    )
                )
        committed_frames = {
            frame for frame in (recovery.frames.keys() if recovery is not None else ())
        }
        conflict_reports: list[dict[str, Any]] = (
            list(recovery.conflict_reports) if recovery is not None else []
        )
        status = "completed"
        error_code: str | None = None
        message = ""
        chunks_committed = 0

        try:
            self._checkpoint(cancel_event, pause_event)
            for chunk_start in range(start_frame, document.frame_count, self.config.chunk_size):
                chunk_end = min(
                    chunk_start + self.config.chunk_size - 1,
                    document.frame_count - 1,
                )
                self._checkpoint(cancel_event, pause_event)
                estimated = estimate_memory_bytes(
                    document,
                    constraints,
                    chunk_end - chunk_start + 1,
                    self.config,
                )
                if estimated > self.config.memory_budget_bytes:
                    raise BusinessError(
                        E_BUDGET_EXCEEDED,
                        "estimated local solver working set exceeds the memory budget",
                        {
                            "interval": [chunk_start, chunk_end],
                            "estimated_bytes": estimated,
                            "budget_bytes": self.config.memory_budget_bytes,
                        },
                    )
                frames = list(range(chunk_start, chunk_end + 1))
                chunk_results = solve_frames(
                    document,
                    constraints,
                    frames,
                    self.config,
                    strategy=chosen,
                    cache=cache,
                    cancel_check=(
                        (lambda: cancel_event.is_set() and self._raise_cancelled(cancel_event))
                        if cancel_event is not None
                        else None
                    ),
                )
                successful = [item for item in chunk_results if item.satisfied]
                for item in chunk_results:
                    if item.diagnostic is not None:
                        conflict_reports.append(item.diagnostic)
                    results.append(item)
                for item in successful:
                    committed_frames.add(item.frame)
                # A snapshot is committed only after the whole interval has
                # been solved.  A budget/cancel failure above therefore leaves
                # the last previously committed interval intact.
                store.commit_interval(
                    chunk_start,
                    chunk_end,
                    successful,
                    [item.diagnostic for item in chunk_results if item.diagnostic is not None],
                )
                chunks_committed += 1
                if progress is not None:
                    stats = cache.stats()
                    progress(
                        ProgressEvent(
                            status="running",
                            start_frame=chunk_start,
                            end_frame=chunk_end,
                            frame_count=document.frame_count,
                            committed_frames=len(committed_frames),
                            conflicts=len(conflict_reports),
                            cache_hits=stats["hits"],
                            cache_misses=stats["misses"],
                            strategy=chosen,
                            committed_ranges=_ranges_from_frames(committed_frames),
                            frame_results=tuple(chunk_results),
                        )
                    )
                if crash_after_chunks and chunks_committed >= crash_after_chunks:
                    # Deliberately bypass cleanup to emulate a hard process
                    # termination.  The OS releases the advisory job lock; the
                    # last atomically renamed interval remains readable.
                    os._exit(23)
            if conflict_reports:
                status = "completed_with_conflicts"
                message = (
                    f"{len(conflict_reports)} frame(s) could not satisfy all constraints; "
                    "those frames were excluded from committed intervals"
                )
        except BusinessError as exc:
            if exc.code == E_CANCELLED:
                status = "cancelled"
                error_code = E_CANCELLED
                message = exc.message
            elif exc.code == E_BUDGET_EXCEEDED:
                status = "budget_exceeded"
                error_code = E_BUDGET_EXCEEDED
                message = exc.message
            else:
                raise
        except Exception as exc:
            status = "failed"
            error_code = "E_INTERNAL"
            message = f"{type(exc).__name__}: {exc}"
        finally:
            results.sort(key=lambda item: item.frame)
            committed_ranges = _ranges_from_frames(committed_frames)
        return RunReport(
            status=status,
            frame_count=document.frame_count,
            strategy=chosen,
            results=results,
            committed_ranges=committed_ranges,
            conflict_reports=conflict_reports,
            recovered=recovery,
            cache_stats=cache.stats(),
            error_code=error_code,
            message=message,
            crashed_after_chunks=chunks_committed,
            constraints_revision=int(constraints.revision),
            constraints_hash=constraints.semantic_hash(),
        )

    @staticmethod
    def _raise_cancelled(cancel_event: threading.Event) -> None:
        if cancel_event.is_set():
            raise BusinessError(
                E_CANCELLED,
                "solve task cancelled",
            )

    def recover_only(self) -> RecoveryState:
        lock = JobLock(self.bvh_path)
        lock.acquire()
        try:
            store = self._store()
            store.open()
            return store.recover(require_manifest=True)
        finally:
            lock.release()


def run_to_completion(
    bvh_path: str | Path,
    constraints_path: str | Path,
    snapshot_dir: str | Path,
    config: SolverConfig | None = None,
) -> RunReport:
    return SolvePipeline(
        bvh_path,
        constraints_path,
        snapshot_dir,
        config,
    ).run(resume=False)

