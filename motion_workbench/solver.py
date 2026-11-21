from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Iterable, Sequence

from .bvh import BVHData
from .constraints import (
    BoneLengthConstraint,
    Constraint,
    ConstraintSet,
    EffectorTargetConstraint,
    JointLimitConstraint,
)
from .hashing import content_hash
from .math3d import (
    Quat,
    Vec3,
    quat_axis_angle,
    quat_canonical,
    quat_mul,
    quat_normalize,
    quat_to_euler_order,
    v_distance,
    v_length,
)

MIN_SCALE = 0.05
MAX_SCALE = 5.0


@dataclass(frozen=True)
class Residual:
    constraint_id: str
    type: str
    joint: str
    raw: float
    tolerance: float
    weight: float
    normalized: float
    evidence: dict[str, Any]

    @property
    def enforced(self) -> bool:
        return self.weight > 0.0

    @property
    def satisfied(self) -> bool:
        return (not self.enforced) or self.raw <= self.tolerance + 1e-12

    def to_dict(self) -> dict[str, Any]:
        return {
            "constraint_id": self.constraint_id,
            "type": self.type,
            "joint": self.joint,
            "raw": self.raw,
            "tolerance": self.tolerance,
            "weight": self.weight,
            "normalized": self.normalized,
            "enforced": self.enforced,
            "satisfied": self.satisfied,
            "evidence": self.evidence,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "Residual":
        return cls(
            constraint_id=str(value["constraint_id"]),
            type=str(value["type"]),
            joint=str(value["joint"]),
            raw=float(value["raw"]),
            tolerance=float(value["tolerance"]),
            weight=float(value["weight"]),
            normalized=float(value["normalized"]),
            evidence=dict(value.get("evidence", {})),
        )


@dataclass(frozen=True)
class PoseFrame:
    frame: int
    rotations: tuple[Quat, ...]
    scales: tuple[float, ...]
    translations: tuple[Vec3, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame": self.frame,
            "rotations": [list(rotation) for rotation in self.rotations],
            "scales": list(self.scales),
            "translations": [list(translation) for translation in self.translations],
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "PoseFrame":
        return cls(
            frame=int(value["frame"]),
            rotations=tuple(
                tuple(float(component) for component in rotation)
                for rotation in value["rotations"]
            ),
            scales=tuple(float(scale) for scale in value["scales"]),
            translations=tuple(
                tuple(float(component) for component in translation)
                for translation in value["translations"]
            ),
        )


@dataclass(frozen=True)
class ConflictDiagnostic:
    frame: int
    constraints: tuple[dict[str, Any], ...]
    joints: tuple[str, ...]
    worst: dict[str, Any]
    iterations: int
    max_iterations: int
    message: str

    @property
    def report_hash(self) -> str:
        return content_hash(self.to_dict())

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": "motion-workbench/conflict/v1",
            "frame": self.frame,
            "constraints": list(self.constraints),
            "joints": list(self.joints),
            "worst": self.worst,
            "iterations": self.iterations,
            "max_iterations": self.max_iterations,
            "message": self.message,
        }


@dataclass(frozen=True)
class FrameSolution:
    frame: int
    pose: PoseFrame
    residuals: tuple[Residual, ...]
    total_residual: float
    raw_weighted_total: float
    converged: bool
    dependency_hash: str
    frame_hash: str
    conflict: ConflictDiagnostic | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame": self.frame,
            "pose": self.pose.to_dict(),
            "residuals": [residual.to_dict() for residual in self.residuals],
            "total_residual": self.total_residual,
            "raw_weighted_total": self.raw_weighted_total,
            "converged": self.converged,
            "dependency_hash": self.dependency_hash,
            "frame_hash": self.frame_hash,
            "conflict": None if self.conflict is None else self.conflict.to_dict(),
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "FrameSolution":
        conflict_value = value.get("conflict")
        conflict = None
        if conflict_value:
            conflict = ConflictDiagnostic(
                frame=int(conflict_value["frame"]),
                constraints=tuple(conflict_value["constraints"]),
                joints=tuple(conflict_value["joints"]),
                worst=dict(conflict_value["worst"]),
                iterations=int(conflict_value["iterations"]),
                max_iterations=int(conflict_value["max_iterations"]),
                message=str(conflict_value["message"]),
            )
        return cls(
            frame=int(value["frame"]),
            pose=PoseFrame.from_dict(value["pose"]),
            residuals=tuple(
                Residual.from_dict(item) for item in value["residuals"]
            ),
            total_residual=float(value["total_residual"]),
            raw_weighted_total=float(value.get("raw_weighted_total", 0.0)),
            converged=bool(value["converged"]),
            dependency_hash=str(value["dependency_hash"]),
            frame_hash=str(value["frame_hash"]),
            conflict=conflict,
        )


@dataclass(frozen=True)
class _Evaluation:
    residuals: tuple[Residual, ...]
    cost: float
    raw_weighted_total: float
    normalized_total: float
    positions: tuple[Vec3, ...]


def _frame_pose_dict(
    frame: int,
    pose: PoseFrame,
    residuals: Sequence[Residual],
    total_residual: float,
    raw_weighted_total: float,
    converged: bool,
    dependency_hash: str,
) -> dict[str, Any]:
    return {
        "frame": frame,
        "pose": pose.to_dict(),
        "residuals": [residual.to_dict() for residual in residuals],
        "total_residual": total_residual,
        "raw_weighted_total": raw_weighted_total,
        "converged": converged,
        "dependency_hash": dependency_hash,
    }


class FrameSolver:
    def __init__(self, bvh: BVHData, constraints: ConstraintSet):
        self.bvh = bvh
        self.constraints = constraints
        constraints.validate(bvh)
        self._ancestors_cache: dict[int, tuple[int, ...]] = {}

    def _ancestors(self, joint_index: int) -> tuple[int, ...]:
        cached = self._ancestors_cache.get(joint_index)
        if cached is not None:
            return cached
        result: list[int] = []
        cursor = self.bvh.joints[joint_index].parent_index
        while cursor >= 0:
            result.append(cursor)
            cursor = self.bvh.joints[cursor].parent_index
        value = tuple(result)
        self._ancestors_cache[joint_index] = value
        return value

    def _bone_target(
        self,
        constraint: BoneLengthConstraint,
        frame: int,
        translations: Sequence[Vec3],
    ) -> float:
        if constraint.target_length is not None:
            return constraint.target_length
        joint_index = self.bvh.joint_by_name[constraint.joint]
        return v_length(translations[joint_index])

    def _evaluate(
        self,
        frame: int,
        rotations: Sequence[Quat],
        scales: Sequence[float],
        translations: Sequence[Vec3],
        active: Sequence[Constraint],
        transform=None,
    ) -> _Evaluation:
        if transform is None:
            transform = self.bvh.evaluate(
                frame,
                rotations=rotations,
                scales=scales,
                translations=translations,
            )
        residuals: list[Residual] = []
        cost = 0.0
        raw_weighted_total = 0.0
        for constraint in active:
            if isinstance(constraint, BoneLengthConstraint):
                joint_index = self.bvh.joint_by_name[constraint.joint]
                actual_length = v_length(translations[joint_index]) * float(
                    scales[joint_index]
                )
                target_length = self._bone_target(
                    constraint, frame, translations
                )
                raw = abs(actual_length - target_length)
                evidence = {
                    "frame": frame,
                    "joint": constraint.joint,
                    "actual_length": actual_length,
                    "target_length": target_length,
                    "scale": scales[joint_index],
                }
            elif isinstance(constraint, JointLimitConstraint):
                joint_index = self.bvh.joint_by_name[constraint.joint]
                order = self.bvh.joints[joint_index].rotation_order
                euler_rad = quat_to_euler_order(rotations[joint_index], order)
                euler_deg = tuple(math.degrees(value) for value in euler_rad)
                violation = 0.0
                worst_axis = -1
                worst_delta = 0.0
                for axis_index, (value, minimum, maximum) in enumerate(
                    zip(euler_deg, constraint.min_deg, constraint.max_deg)
                ):
                    delta = 0.0
                    if value < minimum:
                        delta = minimum - value
                    elif value > maximum:
                        delta = value - maximum
                    if delta > violation:
                        violation = delta
                        worst_axis = axis_index
                        worst_delta = delta
                raw = violation
                evidence = {
                    "frame": frame,
                    "joint": constraint.joint,
                    "rotation_order": order,
                    "euler_deg": list(euler_deg),
                    "min_deg": list(constraint.min_deg),
                    "max_deg": list(constraint.max_deg),
                    "violation_axis": (
                        order[worst_axis] if worst_axis >= 0 else None
                    ),
                    "violation_deg": worst_delta,
                }
            elif isinstance(constraint, EffectorTargetConstraint):
                target = constraint.target_at(frame)
                if target is None:
                    continue
                joint_index = self.bvh.joint_by_name[constraint.joint]
                actual = transform.world_positions[joint_index]
                raw = v_distance(actual, target)
                evidence = {
                    "frame": frame,
                    "joint": constraint.joint,
                    "target": list(target),
                    "actual_world": list(actual),
                    "distance": raw,
                }
            else:
                raise TypeError(f"Unsupported constraint: {type(constraint)!r}")
            normalized = raw / constraint.tolerance
            residuals.append(
                Residual(
                    constraint_id=constraint.id,
                    type=constraint.type,
                    joint=constraint.joint,
                    raw=raw,
                    tolerance=constraint.tolerance,
                    weight=constraint.weight,
                    normalized=normalized,
                    evidence=evidence,
                )
            )
            if constraint.weight > 0.0:
                weighted = constraint.weight * normalized * normalized
                cost += weighted
                raw_weighted_total += constraint.weight * raw
        normalized_total = math.sqrt(max(0.0, cost))
        return _Evaluation(
            residuals=tuple(residuals),
            cost=cost,
            raw_weighted_total=raw_weighted_total,
            normalized_total=normalized_total,
            positions=transform.world_positions,
        )

    def _variables_for(
        self, active: Sequence[Constraint], frame: int
    ) -> tuple[tuple[int, ...], tuple[int, ...]]:
        rotation_variables: set[int] = set()
        scale_variables: set[int] = set()
        for constraint in active:
            joint_index = self.bvh.joint_by_name[constraint.joint]
            if isinstance(constraint, BoneLengthConstraint):
                scale_variables.add(joint_index)
                continue
            if isinstance(constraint, EffectorTargetConstraint):
                if constraint.target_at(frame) is None:
                    continue
                scale_variables.update(self._ancestors(joint_index))
                scale_variables.add(joint_index)
            rotation_variables.add(joint_index)
            rotation_variables.update(self._ancestors(joint_index))
        # Scale on the root's local translation is unconstrained in normal rigs.
        scale_variables.discard(0)
        rotations = tuple(
            sorted(
                rotation_variables,
                key=lambda index: (
                    -self._depth(index),
                    self.bvh.joints[index].name,
                ),
            )
        )
        scales = tuple(
            sorted(
                scale_variables,
                key=lambda index: (
                    -self._depth(index),
                    self.bvh.joints[index].name,
                ),
            )
        )
        return rotations, scales

    def _depth(self, joint_index: int) -> int:
        depth = 0
        cursor = joint_index
        while self.bvh.joints[cursor].parent_index >= 0:
            depth += 1
            cursor = self.bvh.joints[cursor].parent_index
        return depth

    def solve_frame(self, frame: int, run_id: str = "run") -> FrameSolution:
        if frame < 0 or frame >= self.bvh.frame_count:
            raise IndexError(frame)
        active = tuple(
            sorted(self.constraints.applicable(frame), key=lambda item: item.id)
        )
        dependency_hash = self.constraints.frame_dependency_hash(self.bvh, frame)
        base_rotations, translations = self.bvh.base_pose_arrays(frame)
        rotations = list(base_rotations)
        scales = [1.0] * len(self.bvh.joints)
        rotation_variables, scale_variables = self._variables_for(active, frame)

        evaluation = self._evaluate(
            frame, rotations, scales, translations, active
        )
        rotation_step = math.radians(self.constraints.solve.rotation_step_deg)
        min_rotation_step = math.radians(
            self.constraints.solve.min_rotation_step_deg
        )
        scale_step = self.constraints.solve.scale_step
        min_scale_step = self.constraints.solve.min_scale_step
        iterations = 0

        while iterations < self.constraints.solve.max_iterations:
            if all(residual.satisfied for residual in evaluation.residuals):
                break
            iterations += 1
            improved_any = False
            for joint_index in rotation_variables:
                current_q = rotations[joint_index]
                for axis in ("X", "Y", "Z"):
                    best_q = current_q
                    best_cost = evaluation.cost
                    accepted = False
                    for sign in (1.0, -1.0):
                        candidate_q = quat_normalize(
                            quat_mul(
                                current_q,
                                quat_axis_angle(axis, sign * rotation_step),
                            )
                        )
                        rotations[joint_index] = candidate_q
                        candidate = self._evaluate(
                            frame, rotations, scales, translations, active
                        )
                        if candidate.cost + 1e-15 < best_cost:
                            best_cost = candidate.cost
                            best_q = candidate_q
                            evaluation = candidate
                            accepted = True
                    rotations[joint_index] = best_q
                    if accepted:
                        improved_any = True
                        current_q = best_q

            for joint_index in scale_variables:
                current_scale = scales[joint_index]
                best_scale = current_scale
                best_cost = evaluation.cost
                for sign in (1.0, -1.0):
                    candidate_scale = max(
                        MIN_SCALE,
                        min(MAX_SCALE, current_scale + sign * scale_step),
                    )
                    if candidate_scale == current_scale:
                        continue
                    scales[joint_index] = candidate_scale
                    candidate = self._evaluate(
                        frame, rotations, scales, translations, active
                    )
                    if candidate.cost + 1e-15 < best_cost:
                        best_cost = candidate.cost
                        best_scale = candidate_scale
                        evaluation = candidate
                scales[joint_index] = best_scale
                if best_scale != current_scale:
                    improved_any = True

            if not improved_any:
                rotation_step = max(min_rotation_step, rotation_step * 0.5)
                scale_step = max(min_scale_step, scale_step * 0.5)

        evaluation = self._evaluate(
            frame, rotations, scales, translations, active
        )
        converged = all(
            residual.satisfied for residual in evaluation.residuals
        )
        conflict: ConflictDiagnostic | None = None
        if not converged:
            unsatisfied = [
                residual
                for residual in evaluation.residuals
                if not residual.satisfied
            ]
            worst = max(
                unsatisfied,
                key=lambda item: (item.normalized, item.constraint_id),
            )
            constraint_entries = tuple(
                {
                    "constraint_id": residual.constraint_id,
                    "type": residual.type,
                    "joint": residual.joint,
                    "raw_residual": residual.raw,
                    "tolerance": residual.tolerance,
                    "weight": residual.weight,
                    "normalized_residual": residual.normalized,
                    "evidence": residual.evidence,
                }
                for residual in unsatisfied
            )
            conflict = ConflictDiagnostic(
                frame=frame,
                constraints=constraint_entries,
                joints=tuple(
                    sorted({residual.joint for residual in unsatisfied})
                ),
                worst={
                    "constraint_id": worst.constraint_id,
                    "type": worst.type,
                    "joint": worst.joint,
                    "raw_residual": worst.raw,
                    "tolerance": worst.tolerance,
                    "normalized_residual": worst.normalized,
                    "evidence": worst.evidence,
                },
                iterations=iterations,
                max_iterations=self.constraints.solve.max_iterations,
                message=(
                    "Iteration limit reached before all enforced constraints "
                    "met their tolerances"
                ),
            )

        canonical_rotations = tuple(quat_canonical(rotation) for rotation in rotations)
        canonical_scales = tuple(float(scale) for scale in scales)
        pose = PoseFrame(
            frame=frame,
            rotations=canonical_rotations,
            scales=canonical_scales,
            translations=tuple(translations),
        )
        hash_payload = _frame_pose_dict(
            frame,
            pose,
            evaluation.residuals,
            evaluation.normalized_total,
            evaluation.raw_weighted_total,
            converged,
            dependency_hash,
        )
        frame_hash = content_hash(hash_payload)
        return FrameSolution(
            frame=frame,
            pose=pose,
            residuals=evaluation.residuals,
            total_residual=evaluation.normalized_total,
            raw_weighted_total=evaluation.raw_weighted_total,
            converged=converged,
            dependency_hash=dependency_hash,
            frame_hash=frame_hash,
            conflict=conflict,
        )

    def solve_range(
        self,
        start: int,
        end: int,
        *,
        run_id: str = "run",
    ) -> tuple[FrameSolution, ...]:
        return tuple(
            self.solve_frame(frame, run_id=run_id)
            for frame in range(start, end + 1)
        )


def residual_digest(solutions: Iterable[FrameSolution]) -> str:
    return content_hash(
        [
            {
                "frame": solution.frame,
                "residuals": [
                    {
                        "constraint_id": residual.constraint_id,
                        "raw": residual.raw,
                        "tolerance": residual.tolerance,
                    }
                    for residual in solution.residuals
                ],
                "converged": solution.converged,
            }
            for solution in sorted(solutions, key=lambda item: item.frame)
        ]
    )


def frame_solution_hash(solution: FrameSolution) -> str:
    return content_hash(
        _frame_pose_dict(
            solution.frame,
            solution.pose,
            solution.residuals,
            solution.total_residual,
            solution.raw_weighted_total,
            solution.converged,
            solution.dependency_hash,
        )
    )
