from __future__ import annotations

import math
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, Iterable, Sequence

from .bvh import BVHData
from .errors import E_CONSTRAINT_INVALID, WorkbenchError
from .hashing import content_hash
from .math3d import Vec3, validate_euler_order

FrameSelection = tuple[tuple[int, int], ...]


def _constraint_error(message: str, constraint_id: str | None = None, **extra):
    details = {"constraint_id": constraint_id}
    details.update(extra)
    return WorkbenchError(E_CONSTRAINT_INVALID, message, details)


def _normalize_ranges(
    ranges: Iterable[Sequence[int]] | None,
    frame_count: int,
    *,
    constraint_id: str,
    default_all: bool = True,
) -> FrameSelection:
    if ranges is None:
        return ((0, frame_count - 1),) if default_all and frame_count > 0 else ()
    normalized: list[tuple[int, int]] = []
    for pair in ranges:
        if len(pair) != 2:
            raise _constraint_error(
                "Frame range must contain start and end",
                constraint_id,
                frame_range=list(pair),
            )
        start, end = int(pair[0]), int(pair[1])
        if start < 0 or end < start or end >= frame_count:
            raise _constraint_error(
                "Frame range is outside BVH frame count",
                constraint_id,
                frame_range=[start, end],
                frame_count=frame_count,
            )
        normalized.append((start, end))
    normalized.sort()
    merged: list[tuple[int, int]] = []
    for start, end in normalized:
        if merged and start <= merged[-1][1] + 1:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    return tuple(merged)


def ranges_contain(ranges: FrameSelection, frame: int) -> bool:
    for start, end in ranges:
        if start <= frame <= end:
            return True
        if frame < start:
            return False
    return False


def ranges_to_frames(ranges: FrameSelection) -> set[int]:
    result: set[int] = set()
    for start, end in ranges:
        result.update(range(start, end + 1))
    return result


def frames_to_ranges(frames: Iterable[int]) -> tuple[tuple[int, int], ...]:
    ordered = sorted(set(int(frame) for frame in frames))
    if not ordered:
        return ()
    result: list[tuple[int, int]] = []
    start = previous = ordered[0]
    for frame in ordered[1:]:
        if frame == previous + 1:
            previous = frame
            continue
        result.append((start, previous))
        start = previous = frame
    result.append((start, previous))
    return tuple(result)


def union_ranges(*selections: FrameSelection) -> FrameSelection:
    return frames_to_ranges(
        frame
        for selection in selections
        for frame in ranges_to_frames(selection)
    )


@dataclass(frozen=True)
class SolveConfig:
    max_iterations: int = 32
    rotation_step_deg: float = 6.0
    scale_step: float = 0.03
    min_rotation_step_deg: float = 0.01
    min_scale_step: float = 1e-5

    def validate(self) -> None:
        if self.max_iterations < 1:
            raise _constraint_error("max_iterations must be positive")
        if self.rotation_step_deg <= 0 or self.min_rotation_step_deg <= 0:
            raise _constraint_error("Rotation step sizes must be positive")
        if self.scale_step <= 0 or self.min_scale_step <= 0:
            raise _constraint_error("Scale step sizes must be positive")
        if self.min_rotation_step_deg > self.rotation_step_deg:
            raise _constraint_error("min_rotation_step_deg exceeds rotation_step_deg")
        if self.min_scale_step > self.scale_step:
            raise _constraint_error("min_scale_step exceeds scale_step")

    def to_dict(self) -> dict[str, Any]:
        return {
            "max_iterations": self.max_iterations,
            "rotation_step_deg": self.rotation_step_deg,
            "scale_step": self.scale_step,
            "min_rotation_step_deg": self.min_rotation_step_deg,
            "min_scale_step": self.min_scale_step,
        }

    @classmethod
    def from_dict(cls, value: dict[str, Any] | None) -> "SolveConfig":
        value = value or {}
        return cls(
            max_iterations=int(value.get("max_iterations", cls.max_iterations)),
            rotation_step_deg=float(
                value.get("rotation_step_deg", cls.rotation_step_deg)
            ),
            scale_step=float(value.get("scale_step", cls.scale_step)),
            min_rotation_step_deg=float(
                value.get("min_rotation_step_deg", cls.min_rotation_step_deg)
            ),
            min_scale_step=float(
                value.get("min_scale_step", cls.min_scale_step)
            ),
        )


@dataclass(frozen=True)
class BoneLengthConstraint:
    id: str
    joint: str
    weight: float
    tolerance: float
    frames: FrameSelection
    target_length: float | None = None
    type: str = "bone_length"

    def validate(self, bvh: BVHData) -> None:
        if self.joint not in bvh.joint_by_name:
            raise _constraint_error(
                "Bone-length joint does not exist", self.id, joint=self.joint
            )
        joint_index = bvh.joint_by_name[self.joint]
        if bvh.joints[joint_index].parent_index < 0:
            raise _constraint_error(
                "Bone-length constraint cannot target the root",
                self.id,
                joint=self.joint,
            )
        if self.weight < 0 or not math.isfinite(self.weight):
            raise _constraint_error("Constraint weight is invalid", self.id)
        if self.tolerance <= 0 or not math.isfinite(self.tolerance):
            raise _constraint_error("Constraint tolerance must be positive", self.id)
        if self.target_length is not None and (
            self.target_length <= 0 or not math.isfinite(self.target_length)
        ):
            raise _constraint_error(
                "Target bone length must be positive", self.id
            )
        for start, end in self.frames:
            if start < 0 or end < start or end >= bvh.frame_count:
                raise _constraint_error(
                    "Bone-length frame range is invalid",
                    self.id,
                    frame_range=[start, end],
                )

    def active_at(self, frame: int) -> bool:
        return ranges_contain(self.frames, frame)

    def to_dict(self) -> dict[str, Any]:
        result = {
            "id": self.id,
            "type": self.type,
            "joint": self.joint,
            "weight": self.weight,
            "tolerance": self.tolerance,
            "frames": [list(pair) for pair in self.frames],
        }
        if self.target_length is not None:
            result["target_length"] = self.target_length
        return result

    def dependency_dict(self, bvh: BVHData, frame: int) -> dict[str, Any]:
        joint_index = bvh.joint_by_name[self.joint]
        target = (
            self.target_length
            if self.target_length is not None
            else _vector_length(bvh.base_local_translation(joint_index, frame))
        )
        return {
            "id": self.id,
            "type": self.type,
            "joint": self.joint,
            "weight": self.weight,
            "tolerance": self.tolerance,
            "target_length": target,
        }

    def with_changes(self, **changes: Any) -> "BoneLengthConstraint":
        return replace(self, **changes)


@dataclass(frozen=True)
class JointLimitConstraint:
    id: str
    joint: str
    min_deg: tuple[float, float, float]
    max_deg: tuple[float, float, float]
    weight: float
    tolerance: float
    frames: FrameSelection
    type: str = "joint_limit"

    def validate(self, bvh: BVHData) -> None:
        if self.joint not in bvh.joint_by_name:
            raise _constraint_error(
                "Joint-limit joint does not exist", self.id, joint=self.joint
            )
        joint = bvh.joints[bvh.joint_by_name[self.joint]]
        order = joint.rotation_order
        try:
            validate_euler_order(order)
        except ValueError as exc:
            raise _constraint_error(
                "Joint has no usable rotation channel order",
                self.id,
                joint=self.joint,
                rotation_order=order,
            ) from exc
        if len(order) != 3:
            raise _constraint_error(
                "Joint limit requires three rotation channels",
                self.id,
                joint=self.joint,
                rotation_order=order,
            )
        if len(self.min_deg) != 3 or len(self.max_deg) != 3:
            raise _constraint_error(
                "Joint limit requires three min and max values", self.id
            )
        for minimum, maximum in zip(self.min_deg, self.max_deg):
            if (
                not math.isfinite(minimum)
                or not math.isfinite(maximum)
                or minimum > maximum
            ):
                raise _constraint_error(
                    "Joint limit bounds are invalid", self.id
                )
        if self.weight < 0 or not math.isfinite(self.weight):
            raise _constraint_error("Constraint weight is invalid", self.id)
        if self.tolerance <= 0 or not math.isfinite(self.tolerance):
            raise _constraint_error("Constraint tolerance must be positive", self.id)
        for start, end in self.frames:
            if start < 0 or end < start or end >= bvh.frame_count:
                raise _constraint_error(
                    "Joint-limit frame range is invalid",
                    self.id,
                    frame_range=[start, end],
                )

    def active_at(self, frame: int) -> bool:
        return ranges_contain(self.frames, frame)

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "type": self.type,
            "joint": self.joint,
            "min_deg": list(self.min_deg),
            "max_deg": list(self.max_deg),
            "weight": self.weight,
            "tolerance": self.tolerance,
            "frames": [list(pair) for pair in self.frames],
        }

    def dependency_dict(self, bvh: BVHData, frame: int) -> dict[str, Any]:
        return {
            "id": self.id,
            "type": self.type,
            "joint": self.joint,
            "min_deg": list(self.min_deg),
            "max_deg": list(self.max_deg),
            "weight": self.weight,
            "tolerance": self.tolerance,
            "rotation_order": bvh.joints[bvh.joint_by_name[self.joint]].rotation_order,
        }

    def with_changes(self, **changes: Any) -> "JointLimitConstraint":
        return replace(self, **changes)


@dataclass(frozen=True)
class EffectorTargetConstraint:
    id: str
    joint: str
    targets: tuple[tuple[int, Vec3], ...]
    weight: float
    tolerance: float
    frames: FrameSelection
    type: str = "effector_target"

    def target_at(self, frame: int) -> Vec3 | None:
        for target_frame, target in self.targets:
            if target_frame == frame:
                return target
            if target_frame > frame:
                return None
        return None

    def validate(self, bvh: BVHData) -> None:
        if self.joint not in bvh.joint_by_name:
            raise _constraint_error(
                "Effector joint does not exist", self.id, joint=self.joint
            )
        if self.weight < 0 or not math.isfinite(self.weight):
            raise _constraint_error("Constraint weight is invalid", self.id)
        if self.tolerance <= 0 or not math.isfinite(self.tolerance):
            raise _constraint_error("Constraint tolerance must be positive", self.id)
        previous = -1
        for frame, target in self.targets:
            if frame <= previous:
                raise _constraint_error(
                    "Effector target frames must be strictly increasing",
                    self.id,
                    frame=frame,
                )
            previous = frame
            if frame < 0 or frame >= bvh.frame_count:
                raise _constraint_error(
                    "Effector target frame is outside BVH",
                    self.id,
                    frame=frame,
                )
            if len(target) != 3 or not all(math.isfinite(value) for value in target):
                raise _constraint_error(
                    "Effector target is invalid",
                    self.id,
                    frame=frame,
                    target=list(target),
                )
        target_frames = {frame for frame, _ in self.targets}
        selected_frames = ranges_to_frames(self.frames)
        if target_frames != selected_frames:
            missing = sorted(selected_frames - target_frames)
            extra = sorted(target_frames - selected_frames)
            raise _constraint_error(
                "Effector frame selection does not match target frames",
                self.id,
                missing_target_frames=missing[:20],
                targets_outside_selection=extra[:20],
            )

    def active_at(self, frame: int) -> bool:
        return self.target_at(frame) is not None

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "type": self.type,
            "joint": self.joint,
            "weight": self.weight,
            "tolerance": self.tolerance,
            "targets": [
                [frame, list(target)] for frame, target in self.targets
            ],
            "frames": [list(pair) for pair in self.frames],
        }

    def dependency_dict(self, bvh: BVHData, frame: int) -> dict[str, Any]:
        return {
            "id": self.id,
            "type": self.type,
            "joint": self.joint,
            "weight": self.weight,
            "tolerance": self.tolerance,
            "target": list(self.target_at(frame) or (0.0, 0.0, 0.0)),
        }

    def with_changes(self, **changes: Any) -> "EffectorTargetConstraint":
        return replace(self, **changes)


Constraint = BoneLengthConstraint | JointLimitConstraint | EffectorTargetConstraint


@dataclass(frozen=True)
class ConstraintSet:
    constraints: tuple[Constraint, ...]
    solve: SolveConfig = SolveConfig()
    name: str = "constraints"
    schema: str = "motion-workbench/constraints/v1"

    def __post_init__(self) -> None:
        ids = [constraint.id for constraint in self.constraints]
        if len(ids) != len(set(ids)):
            raise _constraint_error("Constraint identifiers must be unique")
        self.solve.validate()

    @property
    def by_id(self) -> dict[str, Constraint]:
        return {constraint.id: constraint for constraint in self.constraints}

    @property
    def version(self) -> str:
        return content_hash(self.to_dict())

    def validate(self, bvh: BVHData) -> "ConstraintSet":
        if not self.constraints:
            raise _constraint_error("Constraint set is empty")
        for constraint in self.constraints:
            constraint.validate(bvh)
        return self

    def applicable(self, frame: int) -> tuple[Constraint, ...]:
        return tuple(
            constraint
            for constraint in self.constraints
            if constraint.active_at(frame)
        )

    def all_frames(self) -> set[int]:
        result: set[int] = set()
        for constraint in self.constraints:
            result.update(ranges_to_frames(constraint.frames))
        return result

    def frame_dependency(self, bvh: BVHData, frame: int) -> dict[str, Any]:
        active = self.applicable(frame)
        return {
            "solve": self.solve.to_dict(),
            "constraints": [
                constraint.dependency_dict(bvh, frame)
                for constraint in active
            ],
        }

    def frame_dependency_hash(self, bvh: BVHData, frame: int) -> str:
        return content_hash(self.frame_dependency(bvh, frame))

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": self.schema,
            "name": self.name,
            "solver": self.solve.to_dict(),
            "constraints": [
                constraint.to_dict()
                for constraint in sorted(self.constraints, key=lambda item: item.id)
            ],
        }

    def with_constraint(self, constraint: Constraint) -> "ConstraintSet":
        replacement = {
            item.id: item for item in self.constraints
        }
        replacement[constraint.id] = constraint
        return ConstraintSet(
            constraints=tuple(
                replacement[key] for key in sorted(replacement)
            ),
            solve=self.solve,
            name=self.name,
            schema=self.schema,
        )

    def without_constraint(self, constraint_id: str) -> "ConstraintSet":
        return ConstraintSet(
            constraints=tuple(
                item
                for item in self.constraints
                if item.id != constraint_id
            ),
            solve=self.solve,
            name=self.name,
            schema=self.schema,
        )

    def with_solve(self, solve: SolveConfig) -> "ConstraintSet":
        return ConstraintSet(
            constraints=self.constraints,
            solve=solve,
            name=self.name,
            schema=self.schema,
        )


def _vector_length(value: Vec3) -> float:
    return math.sqrt(
        value[0] * value[0] + value[1] * value[1] + value[2] * value[2]
    )


def _parse_targets(
    raw_targets: Any, constraint_id: str
) -> tuple[tuple[int, Vec3], ...]:
    if not isinstance(raw_targets, list):
        raise _constraint_error(
            "Effector targets must be a list", constraint_id
        )
    targets: list[tuple[int, Vec3]] = []
    for item in raw_targets:
        if isinstance(item, dict):
            frame = int(item["frame"])
            raw_target = item["target"]
        elif isinstance(item, (list, tuple)) and len(item) == 2:
            frame = int(item[0])
            raw_target = item[1]
        else:
            raise _constraint_error(
                "Invalid effector target entry", constraint_id
            )
        if not isinstance(raw_target, (list, tuple)) or len(raw_target) != 3:
            raise _constraint_error(
                "Effector target must contain three coordinates",
                constraint_id,
                frame=frame,
            )
        target = tuple(float(value) for value in raw_target)
        targets.append((frame, target))
    targets.sort(key=lambda entry: entry[0])
    return tuple(targets)


def parse_constraints(
    value: dict[str, Any], bvh: BVHData
) -> ConstraintSet:
    if not isinstance(value, dict):
        raise _constraint_error("Constraint document must be an object")
    schema = value.get("schema", "motion-workbench/constraints/v1")
    if schema != "motion-workbench/constraints/v1":
        raise _constraint_error("Unsupported constraint schema", schema=schema)
    solve = SolveConfig.from_dict(value.get("solver"))
    raw_constraints = value.get("constraints")
    if not isinstance(raw_constraints, list) or not raw_constraints:
        raise _constraint_error("Constraint document must contain constraints")
    parsed: list[Constraint] = []
    seen: set[str] = set()
    for item in raw_constraints:
        if not isinstance(item, dict):
            raise _constraint_error("Constraint entry must be an object")
        constraint_id = str(item.get("id", "")).strip()
        if not constraint_id:
            raise _constraint_error("Constraint id is required")
        if constraint_id in seen:
            raise _constraint_error(
                "Constraint identifiers must be unique", constraint_id
            )
        seen.add(constraint_id)
        kind = item.get("type")
        joint = str(item.get("joint", ""))
        weight = float(item.get("weight", 1.0))
        tolerance = float(item.get("tolerance", 1e-3))
        if kind == "bone_length":
            target_length = item.get("target_length")
            parsed.append(
                BoneLengthConstraint(
                    id=constraint_id,
                    joint=joint,
                    weight=weight,
                    tolerance=tolerance,
                    frames=_normalize_ranges(
                        item.get("frames"),
                        bvh.frame_count,
                        constraint_id=constraint_id,
                    ),
                    target_length=(
                        None if target_length is None else float(target_length)
                    ),
                )
            )
        elif kind == "joint_limit":
            min_deg = item.get("min_deg", item.get("min"))
            max_deg = item.get("max_deg", item.get("max"))
            if (
                not isinstance(min_deg, (list, tuple))
                or not isinstance(max_deg, (list, tuple))
                or len(min_deg) != 3
                or len(max_deg) != 3
            ):
                raise _constraint_error(
                    "Joint limit requires three min_deg and max_deg values",
                    constraint_id,
                )
            parsed.append(
                JointLimitConstraint(
                    id=constraint_id,
                    joint=joint,
                    min_deg=tuple(float(value) for value in min_deg),
                    max_deg=tuple(float(value) for value in max_deg),
                    weight=weight,
                    tolerance=tolerance,
                    frames=_normalize_ranges(
                        item.get("frames"),
                        bvh.frame_count,
                        constraint_id=constraint_id,
                    ),
                )
            )
        elif kind == "effector_target":
            targets = _parse_targets(item.get("targets"), constraint_id)
            target_frames = {frame for frame, _ in targets}
            if "frames" in item:
                frames = _normalize_ranges(
                    item.get("frames"),
                    bvh.frame_count,
                    constraint_id=constraint_id,
                )
            else:
                frames = frames_to_ranges(target_frames)
            parsed.append(
                EffectorTargetConstraint(
                    id=constraint_id,
                    joint=joint,
                    targets=targets,
                    weight=weight,
                    tolerance=tolerance,
                    frames=frames,
                )
            )
        else:
            raise _constraint_error(
                "Unsupported constraint type",
                constraint_id,
                type=kind,
            )
    result = ConstraintSet(
        constraints=tuple(sorted(parsed, key=lambda item: item.id)),
        solve=solve,
        name=str(value.get("name", "constraints")),
        schema=schema,
    )
    result.validate(bvh)
    return result


def load_constraints(path: str | Path, bvh: BVHData) -> ConstraintSet:
    import json

    path = Path(path)
    try:
        text = path.read_text(encoding="utf-8-sig")
        value = json.loads(text)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise _constraint_error(
            "Unable to read constraint document",
            path=str(path),
            reason=str(exc),
        ) from exc
    return parse_constraints(value, bvh)
