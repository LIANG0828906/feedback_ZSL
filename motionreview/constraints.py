"""Constraint definitions, deterministic residual evaluation and diffs."""

from __future__ import annotations

import math
from dataclasses import dataclass, field, replace
from typing import Any, Iterable, Mapping, Sequence

import numpy as np

from .bvh import BVHDocument, bone_rest_length
from .hashing import sha256_json

_KIND_ALIASES = {
    "bone_length": "bone_length",
    "bone": "bone_length",
    "length": "bone_length",
    "angle_limit": "angle_limit",
    "angle": "angle_limit",
    "joint_limit": "angle_limit",
    "effector_position": "effector_position",
    "effector": "effector_position",
    "position": "effector_position",
}


@dataclass(frozen=True)
class ConstraintSpec:
    index: int
    id: str
    kind: str
    weight: float
    tolerance: float
    joint: str
    frame_start: int
    frame_end: int
    axis: str | None = None
    minimum: float | None = None
    maximum: float | None = None
    target: tuple[float, float, float] | None = None
    targets: Mapping[int, tuple[float, float, float]] = field(
        default_factory=dict
    )
    rest_length: float | None = None

    def __post_init__(self) -> None:
        if self.index < 0:
            raise ValueError("constraint index must be non-negative")
        if self.kind not in _KIND_ALIASES.values():
            raise ValueError(f"unknown constraint kind: {self.kind}")
        if not self.id:
            raise ValueError("constraint id must be non-empty")
        if not math.isfinite(self.weight) or self.weight < 0.0:
            raise ValueError("constraint weight must be finite and >= 0")
        if not math.isfinite(self.tolerance) or self.tolerance < 0.0:
            raise ValueError("constraint tolerance must be finite and >= 0")
        if self.frame_start < 0 or self.frame_end <= self.frame_start:
            raise ValueError("constraint frame interval is empty or invalid")
        if self.kind == "angle_limit":
            if self.axis not in {"X", "Y", "Z"}:
                raise ValueError("angle_limit axis must be X, Y or Z")
            if self.minimum is None or self.maximum is None:
                raise ValueError("angle_limit needs minimum and maximum")
            if self.minimum > self.maximum:
                raise ValueError("angle_limit minimum exceeds maximum")
        if self.kind == "effector_position":
            if self.target is None and not self.targets:
                raise ValueError(
                    "effector_position needs target or per-frame targets"
                )
        if self.kind == "bone_length":
            if self.rest_length is None or self.rest_length < 0:
                raise ValueError("bone_length needs a non-negative length")

    def applies_to(self, frame: int) -> bool:
        if not self.frame_start <= frame < self.frame_end:
            return False
        if self.kind == "effector_position" and self.targets:
            return frame in self.targets or self.target is not None
        return True

    def target_for_frame(
        self, frame: int
    ) -> tuple[float, float, float] | None:
        if frame in self.targets:
            return self.targets[frame]
        return self.target

    def affected_frames(self) -> range:
        return range(self.frame_start, self.frame_end)

    def explicit_target_frames(self) -> set[int]:
        if self.target is not None and self.targets:
            return set(range(self.frame_start, self.frame_end))
        if self.targets:
            return set(self.targets)
        return set()

    def with_changes(self, **changes: Any) -> "ConstraintSpec":
        return replace(self, **changes)

    def canonical(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "id": self.id,
            "kind": self.kind,
            "weight": float(self.weight).hex(),
            "tolerance": float(self.tolerance).hex(),
            "joint": self.joint,
            "frame_start": self.frame_start,
            "frame_end": self.frame_end,
            "axis": self.axis,
            "minimum": (
                float(self.minimum).hex()
                if self.minimum is not None
                else None
            ),
            "maximum": (
                float(self.maximum).hex()
                if self.maximum is not None
                else None
            ),
            "target": (
                [float(v).hex() for v in self.target]
                if self.target is not None
                else None
            ),
            "targets": {
                str(frame): [float(v).hex() for v in target]
                for frame, target in sorted(self.targets.items())
            },
            "rest_length": (
                float(self.rest_length).hex()
                if self.rest_length is not None
                else None
            ),
        }


@dataclass(frozen=True)
class ConstraintSet:
    constraints: tuple[ConstraintSpec, ...]
    document_hash: str

    def __post_init__(self) -> None:
        indices = [constraint.index for constraint in self.constraints]
        if indices != list(range(len(self.constraints))):
            raise ValueError("constraint indices must be contiguous from zero")
        ids = [constraint.id for constraint in self.constraints]
        if len(set(ids)) != len(ids):
            raise ValueError("constraint ids must be unique")

    @property
    def parameter_version(self) -> str:
        return sha256_json(
            {
                "schema": "constraint-set-v1",
                "document_hash": self.document_hash,
                "constraints": [
                    constraint.canonical() for constraint in self.constraints
                ],
            }
        )

    def __len__(self) -> int:
        return len(self.constraints)

    def __iter__(self) -> Iterable[ConstraintSpec]:
        return iter(self.constraints)

    def by_id(self, constraint_id: str) -> ConstraintSpec:
        for constraint in self.constraints:
            if constraint.id == constraint_id:
                return constraint
        raise KeyError(constraint_id)

    def with_updates(
        self,
        constraint_id: str,
        *,
        weight: float | None = None,
        tolerance: float | None = None,
        target: Sequence[float] | None = None,
        frame_start: int | None = None,
        frame_end: int | None = None,
    ) -> "ConstraintSet":
        constraints = list(self.constraints)
        current = self.by_id(constraint_id)
        changes: dict[str, Any] = {}
        if weight is not None:
            changes["weight"] = float(weight)
        if tolerance is not None:
            changes["tolerance"] = float(tolerance)
        if target is not None:
            changes["target"] = tuple(float(v) for v in target)
            changes["targets"] = {}
        if frame_start is not None:
            changes["frame_start"] = int(frame_start)
        if frame_end is not None:
            changes["frame_end"] = int(frame_end)
        constraints[current.index] = current.with_changes(**changes)
        return ConstraintSet(tuple(constraints), self.document_hash)

    def changed_frames(
        self, previous: "ConstraintSet", constraint_ids: Iterable[str] | None = None
    ) -> list[int]:
        """Return the exact affected frame interval(s) as a sorted union set.

        Weight/tolerance changes affect every frame where the constraint is
        active. Target/frame-interval changes affect only the union of old and
        new targets. A changed constraint never dirties unrelated frames where
        it does not participate.
        """

        if previous.document_hash != self.document_hash:
            raise ValueError("cannot diff constraint sets for different clips")
        ids = (
            list(constraint_ids)
            if constraint_ids is not None
            else [
                constraint.id
                for constraint in self.constraints
                if constraint != previous.by_id(constraint.id)
            ]
        )
        affected: set[int] = set()
        for constraint_id in ids:
            old = previous.by_id(constraint_id)
            new = self.by_id(constraint_id)
            if (
                old.weight != new.weight
                or old.tolerance != new.tolerance
            ):
                affected.update(old.affected_frames())
                affected.update(new.affected_frames())
            elif old.target != new.target or old.targets != new.targets:
                affected.update(old.explicit_target_frames())
                affected.update(new.explicit_target_frames())
                if old.target is not None or new.target is not None:
                    affected.update(old.affected_frames())
                    affected.update(new.affected_frames())
            else:
                affected.update(old.affected_frames())
                affected.update(new.affected_frames())
        return sorted(affected)

    def affected_range(
        self, previous: "ConstraintSet", constraint_ids: Iterable[str] | None = None
    ) -> tuple[int, int] | None:
        frames = self.changed_frames(previous, constraint_ids)
        if not frames:
            return None
        return frames[0], frames[-1] + 1


def _parse_frames(raw: Mapping[str, Any], frame_count: int) -> tuple[int, int]:
    if "frames" in raw:
        frames = raw["frames"]
        if (
            not isinstance(frames, (list, tuple))
            or len(frames) != 2
        ):
            raise ValueError("constraint frames must be [start, end)")
        start, end = int(frames[0]), int(frames[1])
    else:
        start = int(raw.get("frame_start", 0))
        end = int(raw.get("frame_end", frame_count))
    if start < 0 or end > frame_count or end <= start:
        raise ValueError(
            f"constraint frame interval [{start}, {end}) is outside clip"
        )
    return start, end


def _parse_target(value: Any) -> tuple[float, float, float]:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        raise ValueError("effector target must contain three numbers")
    target = tuple(float(v) for v in value)
    if not all(math.isfinite(v) for v in target):
        raise ValueError("effector target contains a non-finite value")
    return (target[0], target[1], target[2])


def parse_constraints(
    document: BVHDocument, data: Any
) -> ConstraintSet:
    """Build a :class:`ConstraintSet` from strict JSON-compatible data."""

    if isinstance(data, Mapping) and "constraints" in data:
        items = data["constraints"]
    else:
        items = data
    if not isinstance(items, list):
        raise ValueError("constraints must be a list")
    parsed: list[ConstraintSpec] = []
    seen_ids: set[str] = set()
    for index, raw in enumerate(items):
        if not isinstance(raw, Mapping):
            raise ValueError(f"constraint {index} must be an object")
        raw_kind = str(raw.get("kind", ""))
        kind = _KIND_ALIASES.get(raw_kind)
        if kind is None:
            raise ValueError(f"constraint {index} has unknown kind '{raw_kind}'")
        constraint_id = str(raw.get("id", f"{kind}_{index}"))
        if constraint_id in seen_ids:
            raise ValueError(f"duplicate constraint id: {constraint_id}")
        seen_ids.add(constraint_id)
        joint_name = str(raw.get("joint", document.joints[-1].name))
        document.joint_index(joint_name)
        start, end = _parse_frames(raw, document.frame_count)
        weight = float(raw.get("weight", 1.0))
        tolerance = float(raw.get("tolerance", 1e-5))

        axis: str | None = None
        minimum: float | None = None
        maximum: float | None = None
        target: tuple[float, float, float] | None = None
        targets: dict[int, tuple[float, float, float]] = {}
        rest_length: float | None = None

        if kind == "angle_limit":
            axis = str(raw.get("axis", "X")).upper()
            if "min_degrees" in raw or "max_degrees" in raw:
                minimum = math.radians(float(raw["min_degrees"]))
                maximum = math.radians(float(raw["max_degrees"]))
            else:
                minimum = float(raw.get("min", raw.get("minimum", -math.pi)))
                maximum = float(raw.get("max", raw.get("maximum", math.pi)))
        elif kind == "effector_position":
            target = _parse_target(raw["target"]) if "target" in raw else None
            raw_targets = raw.get("targets", {})
            if not isinstance(raw_targets, Mapping):
                raise ValueError("effector targets must be an object")
            for key, value in raw_targets.items():
                frame = int(key)
                if not start <= frame < end:
                    raise ValueError(
                        f"effector target frame {frame} is outside constraint interval"
                    )
                targets[frame] = _parse_target(value)
            if target is None and not targets:
                raise ValueError(
                    "effector_position requires target or targets"
                )
        elif kind == "bone_length":
            joint = document.joints[document.joint_index(joint_name)]
            if "length" in raw:
                rest_length = float(raw["length"])
            else:
                rest_length = bone_rest_length(joint)

        parsed.append(
            ConstraintSpec(
                index=index,
                id=constraint_id,
                kind=kind,
                weight=weight,
                tolerance=tolerance,
                joint=joint_name,
                frame_start=start,
                frame_end=end,
                axis=axis,
                minimum=minimum,
                maximum=maximum,
                target=target,
                targets=targets,
                rest_length=rest_length,
            )
        )
    return ConstraintSet(tuple(parsed), document.content_hash)


def constraint_residuals(
    document: BVHDocument,
    constraints: ConstraintSet,
    frame: int,
    transforms: np.ndarray,
    channel_values: np.ndarray | None = None,
) -> list[dict[str, Any]]:
    """Evaluate residuals in fixed constraint-index order."""

    result: list[dict[str, Any]] = []
    for constraint in constraints:
        if not constraint.applies_to(frame):
            continue
        joint_index = document.joint_index(constraint.joint)
        raw = 0.0
        detail: dict[str, Any] = {}
        if constraint.kind == "bone_length":
            joint = document.joints[joint_index]
            position = transforms[joint_index, :3, 3]
            if joint.parent is None:
                measured = float(np.linalg.norm(position))
            else:
                parent_position = transforms[joint.parent, :3, 3]
                measured = float(np.linalg.norm(position - parent_position))
            assert constraint.rest_length is not None
            raw = abs(measured - constraint.rest_length)
            detail = {
                "measured_length": measured,
                "rest_length": constraint.rest_length,
            }
        elif constraint.kind == "angle_limit":
            values = (
                channel_values
                if channel_values is not None
                else document.frame_values[frame]
            )
            channel = f"{constraint.axis}rotation"
            angle = float(
                values[document.channel_offset(joint_index, channel)]
            )
            assert constraint.minimum is not None
            assert constraint.maximum is not None
            raw = max(
                constraint.minimum - angle,
                angle - constraint.maximum,
                0.0,
            )
            detail = {
                "angle": angle,
                "minimum": constraint.minimum,
                "maximum": constraint.maximum,
            }
        elif constraint.kind == "effector_position":
            target = constraint.target_for_frame(frame)
            if target is None:
                continue
            position = transforms[joint_index, :3, 3]
            raw = float(
                np.linalg.norm(
                    np.asarray(target, dtype=np.float64) - position
                )
            )
            detail = {
                "position": [float(v) for v in position],
                "target": [float(v) for v in target],
            }
        weighted = constraint.weight * raw
        result.append(
            {
                "constraint_id": constraint.id,
                "constraint_index": constraint.index,
                "kind": constraint.kind,
                "joint": constraint.joint,
                "residual": raw,
                "weighted_residual": weighted,
                "weight": constraint.weight,
                "tolerance": constraint.tolerance,
                "satisfied": raw <= constraint.tolerance,
                "detail": detail,
            }
        )
    return result


def total_residual(residuals: Sequence[Mapping[str, Any]]) -> float:
    total = 0.0
    for record in residuals:
        total += float(record["weighted_residual"])
    return total
