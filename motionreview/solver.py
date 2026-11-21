"""Per-frame deterministic local constraint solver with cached results."""

from __future__ import annotations

import math
from dataclasses import dataclass, replace
from typing import Any, Iterable, Mapping, Sequence

import numpy as np

from .bvh import BVHDocument, evaluate_channel_values
from .constraints import (
    ConstraintSet,
    ConstraintSpec,
    constraint_residuals,
    total_residual,
)
from .hashing import frame_content_hash, pose_hash


@dataclass(frozen=True)
class FrameSolution:
    clip_id: str
    frame: int
    transforms: np.ndarray
    residuals: tuple[dict[str, Any], ...]
    total_residual: float
    pose_hash: str
    content_hash: str
    parameter_version: str
    iterations: int
    converged: bool
    status: str
    conflict: Mapping[str, Any] | None = None
    reused: bool = False

    def residual_for(self, constraint_id: str) -> float:
        for record in self.residuals:
            if record["constraint_id"] == constraint_id:
                return float(record["residual"])
        raise KeyError(constraint_id)

    @property
    def total_raw_residual(self) -> float:
        total = 0.0
        for record in self.residuals:
            total += float(record["residual"])
        return total

    def to_commit_record(self) -> dict[str, Any]:
        return {
            "frame": self.frame,
            "status": self.status,
            "iterations": self.iterations,
            "converged": self.converged,
            "pose_hash": self.pose_hash,
            "content_hash": self.content_hash,
            "parameter_version": self.parameter_version,
            "total_residual": self.total_residual,
            "total_raw_residual": self.total_raw_residual,
            "joint_transforms_hex": [
                float(value).hex()
                for value in np.asarray(
                    self.transforms, dtype=np.float64
                ).ravel(order="C")
            ],
            "residuals": [dict(record) for record in self.residuals],
            "conflict": dict(self.conflict) if self.conflict else None,
        }


@dataclass(frozen=True)
class SolveBatch:
    solutions: tuple[FrameSolution, ...]
    affected: tuple[int, int] | None
    frames_solved: tuple[int, ...]
    frames_reused: tuple[int, ...]


class PoseSolver:
    """Serial frame solver; batches and threads may surround it safely.

    Iteration order and every floating-point reduction are explicit. The
    budget degradation path never changes ``max_iterations`` or this
    algorithm, so two runs with the same parameters produce identical bytes.
    """

    algorithm_version = "local-weighted-jacobian-v1"

    def __init__(
        self,
        document: BVHDocument,
        constraints: ConstraintSet,
        *,
        max_iterations: int = 80,
        cache_depth: int = 64,
    ) -> None:
        if constraints.document_hash != document.content_hash:
            raise ValueError("constraints do not belong to this BVH document")
        if max_iterations < 1:
            raise ValueError("max_iterations must be >= 1")
        if cache_depth < 1:
            raise ValueError("cache_depth must be >= 1")
        self.document = document
        self.constraints = constraints
        self.max_iterations = int(max_iterations)
        self.cache_depth = int(cache_depth)
        self._cache: dict[int, FrameSolution] = {}
        self.solved_count = 0
        self.reused_count = 0

    @property
    def parameter_version(self) -> str:
        return f"{self.constraints.parameter_version}:{self._algorithm_hash}"

    @property
    def _algorithm_hash(self) -> str:
        from .hashing import sha256_json

        return sha256_json(
            {
                "algorithm": self.algorithm_version,
                "max_iterations": self.max_iterations,
                "document": self.document.content_hash,
                "constraints": self.constraints.parameter_version,
            }
        )

    def update_constraints(
        self,
        constraints: ConstraintSet,
        *,
        changed_ids: Iterable[str] | None = None,
    ) -> tuple[int, int] | None:
        """Invalidate only exactly affected frames; return their interval."""

        if constraints.document_hash != self.document.content_hash:
            raise ValueError("constraints do not belong to this BVH document")
        affected_frames = constraints.changed_frames(
            self.constraints, changed_ids
        )
        for frame in affected_frames:
            self._cache.pop(frame, None)
        self.constraints = constraints
        if not affected_frames:
            return None
        return affected_frames[0], affected_frames[-1] + 1

    def resolve_affected(
        self,
        constraints: ConstraintSet,
        *,
        changed_ids: Iterable[str] | None = None,
    ) -> tuple[tuple[int, int] | None, SolveBatch | None]:
        """Apply a parameter edit and re-solve only its affected interval.

        Unaffected cached solutions are not touched. Within a sparse affected
        interval, frames without changed dependencies are served from cache.
        """

        affected = self.update_constraints(
            constraints, changed_ids=changed_ids
        )
        if affected is None:
            return None, None
        batch = self.solve_interval(
            affected[0],
            affected[1],
            force_frames=range(affected[0], affected[1]),
        )
        return affected, batch

    def invalidate(self, frames: Iterable[int]) -> None:
        for frame in frames:
            self._cache.pop(int(frame), None)

    def seed_solutions(
        self,
        solutions: Iterable[FrameSolution],
        *,
        require_parameter_version: bool = True,
    ) -> int:
        """Seed committed solutions during recovery without re-solving."""

        seeded = 0
        for solution in solutions:
            if (
                require_parameter_version
                and solution.parameter_version != self.parameter_version
            ):
                continue
            if solution.clip_id != self.document.clip_id:
                continue
            if not 0 <= solution.frame < self.document.frame_count:
                continue
            self._cache[solution.frame] = replace(solution, reused=True)
            seeded += 1
        return seeded

    def get_cached(self, frame: int) -> FrameSolution | None:
        return self._cache.get(frame)

    def solve_frame(
        self, frame: int, *, force: bool = False
    ) -> FrameSolution:
        if not 0 <= frame < self.document.frame_count:
            from .errors import BVHInvalidError

            raise BVHInvalidError(
                "frame index out of range",
                clip_id=self.document.clip_id,
                frame=frame,
            )
        cached = self._cache.get(frame)
        if cached is not None and not force:
            self.reused_count += 1
            return replace(cached, reused=True)
        solution = self._solve_frame_impl(frame)
        self.solved_count += 1
        self._cache[frame] = solution
        self._trim_cache()
        return solution

    def solve_interval(
        self,
        start: int,
        end: int,
        *,
        force_frames: Iterable[int] | None = None,
    ) -> SolveBatch:
        if start < 0 or end > self.document.frame_count or end <= start:
            from .errors import BVHInvalidError

            raise BVHInvalidError(
                "solve interval is outside clip",
                clip_id=self.document.clip_id,
                frame=start,
                details={"start": start, "end": end},
            )
        forced = set(int(frame) for frame in (force_frames or ()))
        solutions: list[FrameSolution] = []
        solved: list[int] = []
        reused: list[int] = []
        for frame in range(start, end):
            if frame in forced:
                solution = self.solve_frame(frame, force=True)
                solved.append(frame)
            elif frame in self._cache:
                solution = self.solve_frame(frame)
                reused.append(frame)
            else:
                solution = self.solve_frame(frame)
                solved.append(frame)
            solutions.append(solution)
        affected = (
            (solved[0], solved[-1] + 1) if solved else None
        )
        return SolveBatch(
            solutions=tuple(solutions),
            affected=affected,
            frames_solved=tuple(solved),
            frames_reused=tuple(reused),
        )

    def solve_all(self) -> SolveBatch:
        return self.solve_interval(0, self.document.frame_count)

    def _trim_cache(self) -> None:
        if len(self._cache) <= self.cache_depth:
            return
        excess = len(self._cache) - self.cache_depth
        for frame in sorted(self._cache)[:excess]:
            del self._cache[frame]

    def _clamp_angle_channels(self, row: np.ndarray) -> None:
        for constraint in self.constraints:
            if constraint.kind != "angle_limit":
                continue
            offset = self.document.channel_offset(
                constraint.joint, f"{constraint.axis}rotation"
            )
            assert constraint.minimum is not None
            assert constraint.maximum is not None
            value = float(row[offset])
            row[offset] = min(
                constraint.maximum, max(constraint.minimum, value)
            )

    def _effector_objective(
        self, row: np.ndarray, frame: int
    ) -> float:
        transforms = evaluate_channel_values(self.document, row)
        value = 0.0
        for constraint in self.constraints:
            if constraint.kind != "effector_position":
                continue
            if not constraint.applies_to(frame):
                continue
            target = constraint.target_for_frame(frame)
            if target is None:
                continue
            joint_index = self.document.joint_index(constraint.joint)
            error = (
                transforms[joint_index, :3, 3]
                - np.asarray(target, dtype=np.float64)
            )
            value += constraint.weight * float(error @ error)
        return value

    def _rotation_columns(
        self, row: np.ndarray, effector_joint: int
    ) -> list[tuple[int, np.ndarray]]:
        ancestors: list[int] = []
        current: int | None = effector_joint
        while current is not None:
            ancestors.append(current)
            current = self.document.joints[current].parent
        columns: list[tuple[int, np.ndarray]] = []
        eps = 1e-5
        for joint_index in ancestors:
            joint = self.document.joints[joint_index]
            for channel in joint.channels:
                if channel in {"Xposition", "Yposition", "Zposition"}:
                    continue
                offset = self.document.channel_offset(joint_index, channel)
                plus = row.copy()
                minus = row.copy()
                plus[offset] = float(row[offset]) + eps
                minus[offset] = float(row[offset]) - eps
                p_plus = evaluate_channel_values(
                    self.document, plus
                )[effector_joint, :3, 3]
                p_minus = evaluate_channel_values(
                    self.document, minus
                )[effector_joint, :3, 3]
                derivative = (p_plus - p_minus) / (2.0 * eps)
                if float(derivative @ derivative) < 1e-18:
                    continue
                columns.append((offset, derivative.copy()))
        return columns

    def _proposed_step(
        self, row: np.ndarray, frame: int
    ) -> tuple[np.ndarray, list[int], np.ndarray]:
        """Build one fixed-order weighted Gauss-Newton step.

        All active effector constraints are accumulated in constraint-index
        order. Columns are introduced in ancestor/declaration order, never in
        a thread-dependent order.
        """

        transforms = evaluate_channel_values(self.document, row)
        column_offsets: list[int] = []
        systems: list[
            tuple[ConstraintSpec, list[tuple[int, np.ndarray]], np.ndarray]
        ] = []
        for constraint in self.constraints:
            if constraint.kind != "effector_position":
                continue
            if not constraint.applies_to(frame):
                continue
            target = constraint.target_for_frame(frame)
            if target is None or constraint.weight == 0.0:
                continue
            joint_index = self.document.joint_index(constraint.joint)
            columns = self._rotation_columns(row, joint_index)
            if not columns:
                continue
            for offset, _ in columns:
                if offset not in column_offsets:
                    column_offsets.append(offset)
            error = (
                np.asarray(target, dtype=np.float64)
                - transforms[joint_index, :3, 3]
            )
            systems.append((constraint, columns, error))

        if not systems:
            return row.copy(), [], np.zeros(0, dtype=np.float64)

        h = np.zeros(
            (len(column_offsets), len(column_offsets)),
            dtype=np.float64,
        )
        g = np.zeros(len(column_offsets), dtype=np.float64)
        for constraint, columns, error in systems:
            weight = constraint.weight
            local_indices = [
                column_offsets.index(offset) for offset, _ in columns
            ]
            for local_row, global_row in enumerate(local_indices):
                derivative = columns[local_row][1]
                g[global_row] += weight * float(derivative @ error)
                for local_col, global_col in enumerate(local_indices):
                    h[global_row, global_col] += weight * float(
                        derivative @ columns[local_col][1]
                    )
        damping = max(
            1e-9,
            1e-4 * (float(np.trace(h)) / max(1, h.shape[0]) + 1.0),
        )
        h = h + np.eye(h.shape[0], dtype=np.float64) * damping
        try:
            delta = np.linalg.solve(h, g)
        except np.linalg.LinAlgError:
            delta = np.linalg.lstsq(h, g, rcond=None)[0]
        norm = math.sqrt(float(delta @ delta))
        if norm > 0.2:
            delta *= 0.2 / norm
        proposed = row.copy()
        for value, offset in zip(delta, column_offsets, strict=True):
            proposed[offset] = float(proposed[offset]) + float(value)
        self._clamp_angle_channels(proposed)
        return proposed, column_offsets, delta

    def _solve_frame_impl(self, frame: int) -> FrameSolution:
        row = np.asarray(
            self.document.frame_values[frame], dtype=np.float64
        ).copy()
        self._clamp_angle_channels(row)
        iterations = 0
        converged = False
        residuals: list[dict[str, Any]] = []
        for iterations in range(1, self.max_iterations + 1):
            transforms = evaluate_channel_values(self.document, row)
            residuals = constraint_residuals(
                self.document,
                self.constraints,
                frame,
                transforms,
                channel_values=row,
            )
            converged = all(
                record["satisfied"] for record in residuals
            )
            if converged:
                break
            proposed, offsets, delta = self._proposed_step(row, frame)
            if not offsets:
                break
            current_objective = self._effector_objective(row, frame)
            objective = self._effector_objective(proposed, frame)
            accepted: np.ndarray | None = None
            if objective <= current_objective:
                accepted = proposed
            else:
                scale = 1.0
                for _ in range(5):
                    scale *= 0.5
                    proposed = row.copy()
                    for value, offset in zip(
                        delta * scale, offsets, strict=True
                    ):
                        proposed[offset] = float(row[offset]) + float(value)
                    self._clamp_angle_channels(proposed)
                    objective = self._effector_objective(proposed, frame)
                    if objective <= current_objective:
                        accepted = proposed
                        break
            if accepted is None:
                break
            if not np.array_equal(row, accepted):
                row = accepted
            else:
                break

        transforms = evaluate_channel_values(self.document, row)
        residuals = constraint_residuals(
            self.document,
            self.constraints,
            frame,
            transforms,
            channel_values=row,
        )
        converged = all(record["satisfied"] for record in residuals)
        total = total_residual(residuals)
        pose_digest = pose_hash(transforms)
        content_digest = frame_content_hash(
            frame=frame,
            transforms=transforms,
            residuals=residuals,
            parameter_version=self.parameter_version,
        )
        conflict: dict[str, Any] | None = None
        status = "CONVERGED" if converged else "CONFLICT"
        if not converged:
            unsatisfied = [
                record for record in residuals if not record["satisfied"]
            ]
            worst = max(
                unsatisfied,
                key=lambda record: (
                    float(record["residual"])
                    / max(float(record["tolerance"]), 1e-15),
                    -int(record["constraint_index"]),
                ),
            )
            conflict = {
                "schema": "constraint-conflict-v1",
                "code": "E_CONSTRAINT_CONFLICT",
                "clip_id": self.document.clip_id,
                "frame": frame,
                "parameter_version": self.parameter_version,
                "pose_hash": pose_digest,
                "content_hash": content_digest,
                "iteration_count": iterations,
                "constraint_ids": [
                    str(record["constraint_id"]) for record in unsatisfied
                ],
                "joints": sorted(
                    {
                        str(record["joint"]) for record in unsatisfied
                    }
                ),
                "worst_residual": dict(worst),
                "residuals": [dict(record) for record in residuals],
                "message": (
                    f"frame {frame} did not satisfy constraints "
                    f"{[record['constraint_id'] for record in unsatisfied]}"
                ),
            }
        return FrameSolution(
            clip_id=self.document.clip_id,
            frame=frame,
            transforms=np.asarray(transforms, dtype=np.float64),
            residuals=tuple(dict(record) for record in residuals),
            total_residual=total,
            pose_hash=pose_digest,
            content_hash=content_digest,
            parameter_version=self.parameter_version,
            iterations=iterations,
            converged=converged,
            status=status,
            conflict=conflict,
        )


def solutions_from_commit_records(
    *,
    clip_id: str,
    parameter_version: str,
    records: Sequence[Mapping[str, Any]],
) -> list[FrameSolution]:
    solutions: list[FrameSolution] = []
    for record in records:
        array = np.asarray(
            [float.fromhex(value) for value in record["joint_transforms_hex"]],
            dtype=np.float64,
        )
        transform_shape = tuple(int(v) for v in record["transform_shape"])
        transforms = array.reshape(transform_shape)
        residuals = tuple(dict(item) for item in record["residuals"])
        solutions.append(
            FrameSolution(
                clip_id=clip_id,
                frame=int(record["frame"]),
                transforms=transforms,
                residuals=residuals,
                total_residual=float(record["total_residual"]),
                pose_hash=str(record["pose_hash"]),
                content_hash=str(record["content_hash"]),
                parameter_version=str(record["parameter_version"]),
                iterations=int(record["iterations"]),
                converged=bool(record["converged"]),
                status=str(record["status"]),
                conflict=(
                    dict(record["conflict"])
                    if record.get("conflict") is not None
                    else None
                ),
                reused=True,
            )
        )
    return solutions
