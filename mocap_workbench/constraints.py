"""Constraint model, validation and per-frame applicability."""

from __future__ import annotations

import json
import math
from collections.abc import Iterable, Mapping
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

import numpy as np

from .bvh import BvhDocument
from .errors import E_CONSTRAINT_INVALID, E_IO, BusinessError
from .math3d import canonical_json_bytes, hash_text

CONSTRAINT_TYPES = {"bone_length", "joint_limit", "effector"}
TYPE_ALIASES = {
    "length": "bone_length",
    "bone": "bone_length",
    "bone_length": "bone_length",
    "limit": "joint_limit",
    "angle_limit": "joint_limit",
    "joint_limit": "joint_limit",
    "effector": "effector",
    "position": "effector",
    "end_effector": "effector",
}


def _fail(message: str, **details: object) -> None:
    raise BusinessError(E_CONSTRAINT_INVALID, message, details)


def _number(value: Any, label: str) -> float:
    try:
        out = float(value)
    except (TypeError, ValueError):
        _fail(f"{label} must be numeric", value=value)
    if not math.isfinite(out):
        _fail(f"{label} must be finite", value=value)
    return out


def _vector3(value: Any, label: str) -> np.ndarray:
    if not isinstance(value, (list, tuple, np.ndarray)) or len(value) != 3:
        _fail(f"{label} must contain three numbers", value=value)
    return np.array([_number(item, label) for item in value], dtype=np.float64)


@dataclass(frozen=True)
class ConstraintSpec:
    id: str
    kind: str
    joint: str
    weight: float
    tolerance: float
    axis: str | None = None
    minimum: float | None = None
    maximum: float | None = None
    target_length: float | None = None
    target_default: np.ndarray | None = None
    frame_targets: tuple[tuple[int, np.ndarray | None], ...] = ()
    missing_frames: frozenset[int] = frozenset()

    @property
    def active(self) -> bool:
        return self.weight > 0.0

    def target_for_frame(self, frame_index: int) -> np.ndarray | None:
        if frame_index in self.missing_frames:
            return None
        for idx, target in self.frame_targets:
            if idx == frame_index:
                return target
        return self.target_default

    def applicable(self, frame_index: int) -> bool:
        if self.kind != "effector":
            return True
        return self.target_for_frame(frame_index) is not None

    def solver_record(self, frame_index: int) -> dict[str, Any]:
        record: dict[str, Any] = {
            "id": self.id,
            "kind": self.kind,
            "joint": self.joint,
            "weight": self.weight,
        }
        if self.kind == "bone_length":
            record["target_length"] = self.target_length
        elif self.kind == "joint_limit":
            record["axis"] = self.axis
            record["minimum"] = self.minimum
            record["maximum"] = self.maximum
        else:
            target = self.target_for_frame(frame_index)
            record["target"] = None if target is None else [float(x) for x in target]
        return record

    def to_dict(self) -> dict[str, Any]:
        base: dict[str, Any] = {
            "id": self.id,
            "type": self.kind,
            "joint": self.joint,
            "weight": self.weight,
        }
        if self.kind == "bone_length":
            base["tolerance"] = self.tolerance
            base["target"] = self.target_length
        elif self.kind == "joint_limit":
            base["tolerance_deg"] = math.degrees(self.tolerance)
            base["axis"] = self.axis
            base["min_deg"] = math.degrees(float(self.minimum))
            base["max_deg"] = math.degrees(float(self.maximum))
        else:
            base["tolerance"] = self.tolerance
            if self.target_default is not None:
                base["target"] = [float(x) for x in self.target_default]
            if self.frame_targets:
                base["frame_targets"] = {
                    str(frame): (
                        None if target is None else [float(x) for x in target]
                    )
                    for frame, target in self.frame_targets
                }
            if self.missing_frames:
                base["missing_frames"] = sorted(self.missing_frames)
        return base


@dataclass(frozen=True)
class ConstraintSet:
    constraints: tuple[ConstraintSpec, ...]
    revision: int = 1
    name: str = "constraints"

    @property
    def by_id(self) -> dict[str, ConstraintSpec]:
        return {item.id: item for item in self.constraints}

    def get(self, constraint_id: str) -> ConstraintSpec:
        try:
            return self.by_id[constraint_id]
        except KeyError as exc:
            raise BusinessError(
                E_CONSTRAINT_INVALID,
                "unknown constraint id",
                {"constraint_id": constraint_id},
            ) from exc

    def semantic_hash(self) -> str:
        return hash_text(canonical_json_bytes(self.to_dict()).decode("utf-8"))

    def solver_signature(self, document: BvhDocument, frame_index: int) -> str:
        payload = {
            "frame": int(frame_index),
            "constraints": [
                item.solver_record(frame_index)
                for item in self.constraints
                if item.applicable(frame_index)
            ],
        }
        return hash_text(canonical_json_bytes(payload).decode("utf-8"))

    def with_parameter(
        self,
        constraint_id: str,
        *,
        weight: float | None = None,
        tolerance: float | None = None,
    ) -> ConstraintSet:
        updated: list[ConstraintSpec] = []
        found = False
        for item in self.constraints:
            if item.id != constraint_id:
                updated.append(item)
                continue
            found = True
            new_weight = item.weight if weight is None else _number(weight, "weight")
            new_tolerance = (
                item.tolerance if tolerance is None else _number(tolerance, "tolerance")
            )
            if new_weight < 0.0:
                _fail("constraint weight must not be negative", constraint_id=constraint_id)
            if new_tolerance <= 0.0:
                _fail("constraint tolerance must be positive", constraint_id=constraint_id)
            updated.append(
                replace(item, weight=new_weight, tolerance=new_tolerance)
            )
        if not found:
            _fail("unknown constraint id", constraint_id=constraint_id)
        return ConstraintSet(
            constraints=tuple(updated),
            revision=self.revision + 1,
            name=self.name,
        )

    def affected_frames(
        self, changed_ids: Iterable[str], frame_count: int
    ) -> tuple[int, ...]:
        changed = set(changed_ids)
        frames: set[int] = set()
        for constraint in self.constraints:
            if constraint.id not in changed:
                continue
            if constraint.kind != "effector":
                frames.update(range(frame_count))
                continue
            if constraint.target_default is not None:
                frames.update(range(frame_count))
                continue
            for frame, target in constraint.frame_targets:
                if 0 <= frame < frame_count and target is not None:
                    frames.add(frame)
        return tuple(sorted(frames))

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": "mocap-constraints-v1",
            "name": self.name,
            "revision": self.revision,
            "constraints": [item.to_dict() for item in self.constraints],
        }

    def validate(self, document: BvhDocument, *, allow_unknown_frames: bool = False) -> None:
        names = set(document.joint_names)
        ids: set[str] = set()
        for constraint in self.constraints:
            if not constraint.id:
                _fail("constraint id must not be empty")
            if constraint.id in ids:
                _fail("duplicate constraint id", constraint_id=constraint.id)
            ids.add(constraint.id)
            if constraint.kind not in CONSTRAINT_TYPES:
                _fail("unknown constraint type", constraint_id=constraint.id)
            if constraint.joint not in names:
                _fail(
                    "constraint references an unknown joint",
                    constraint_id=constraint.id,
                    joint=constraint.joint,
                )
            if constraint.weight < 0.0:
                _fail(
                    "constraint weight must not be negative",
                    constraint_id=constraint.id,
                )
            if constraint.tolerance <= 0.0:
                _fail(
                    "constraint tolerance must be positive",
                    constraint_id=constraint.id,
                )
            if constraint.kind == "bone_length":
                joint_index = document.joint_names.index(constraint.joint)
                if document.joints[joint_index].parent is None:
                    _fail(
                        "root joint cannot have a bone-length constraint",
                        constraint_id=constraint.id,
                    )
                if constraint.target_length is None or constraint.target_length < 0.0:
                    _fail(
                        "bone-length target must be non-negative",
                        constraint_id=constraint.id,
                    )
            elif constraint.kind == "joint_limit":
                if constraint.axis not in {"x", "y", "z"}:
                    _fail(
                        "joint-limit axis must be x, y or z",
                        constraint_id=constraint.id,
                    )
                if constraint.minimum is None or constraint.maximum is None:
                    _fail(
                        "joint-limit minimum and maximum are required",
                        constraint_id=constraint.id,
                    )
                if constraint.minimum > constraint.maximum:
                    _fail(
                        "joint-limit minimum exceeds maximum",
                        constraint_id=constraint.id,
                    )
            else:
                if constraint.target_default is None and not constraint.frame_targets:
                    _fail(
                        "effector constraint has no target",
                        constraint_id=constraint.id,
                    )
                for frame, target in constraint.frame_targets:
                    if target is None:
                        continue
                    if frame < 0 or (not allow_unknown_frames and frame >= document.frame_count):
                        _fail(
                            "effector target frame is outside the motion",
                            constraint_id=constraint.id,
                            frame=frame,
                            frame_count=document.frame_count,
                        )


def _parse_constraint(raw: Mapping[str, Any], document: BvhDocument) -> ConstraintSpec:
    try:
        constraint_id = str(raw["id"])
        raw_type = str(raw.get("type", raw.get("kind", ""))).lower()
        joint = str(raw["joint"])
    except KeyError as exc:
        _fail("constraint is missing a required field", field=str(exc))
    kind = TYPE_ALIASES.get(raw_type)
    if kind is None:
        _fail("unknown constraint type", constraint_id=constraint_id, type=raw_type)
    weight = _number(raw.get("weight", 1.0), "weight")
    if weight < 0.0:
        _fail("constraint weight must not be negative", constraint_id=constraint_id)

    if kind == "bone_length":
        if "tolerance_deg" in raw:
            _fail("bone-length tolerance must use length units", constraint_id=constraint_id)
        tolerance = _number(raw.get("tolerance", 0.005), "tolerance")
        target_length = _number(raw["target"], "target")
        return ConstraintSpec(
            id=constraint_id,
            kind=kind,
            joint=joint,
            weight=weight,
            tolerance=tolerance,
            target_length=target_length,
        )

    if kind == "joint_limit":
        tolerance = _number(raw.get("tolerance_deg", 1.0), "tolerance_deg")
        tolerance = math.radians(tolerance)
        axis = str(raw.get("axis", "x")).lower()
        if "min_deg" in raw or "max_deg" in raw:
            minimum = math.radians(_number(raw["min_deg"], "min_deg"))
            maximum = math.radians(_number(raw["max_deg"], "max_deg"))
        else:
            minimum = _number(raw["min"], "min")
            maximum = _number(raw["max"], "max")
            if str(raw.get("units", "deg")).lower() == "deg":
                minimum = math.radians(minimum)
                maximum = math.radians(maximum)
        return ConstraintSpec(
            id=constraint_id,
            kind=kind,
            joint=joint,
            weight=weight,
            tolerance=tolerance,
            axis=axis,
            minimum=minimum,
            maximum=maximum,
        )

    tolerance = _number(raw.get("tolerance", 0.01), "tolerance")
    target_default: np.ndarray | None = None
    frame_targets: dict[int, np.ndarray | None] = {}
    missing_frames: set[int] = set()
    if "target" in raw and raw["target"] is not None:
        target_default = _vector3(raw["target"], "target")
    if "targets" in raw:
        values = raw["targets"]
        if not isinstance(values, list):
            _fail("effector targets must be a list", constraint_id=constraint_id)
        for frame, value in enumerate(values):
            if value is None:
                missing_frames.add(frame)
                frame_targets[frame] = None
            else:
                frame_targets[frame] = _vector3(value, f"targets[{frame}]")
    if "frame_targets" in raw:
        values = raw["frame_targets"]
        if not isinstance(values, Mapping):
            _fail("frame_targets must be an object", constraint_id=constraint_id)
        for key, value in values.items():
            try:
                frame = int(key)
            except (TypeError, ValueError):
                _fail("frame_targets key must be an integer", constraint_id=constraint_id, key=key)
            if value is None:
                missing_frames.add(frame)
                frame_targets[frame] = None
            else:
                frame_targets[frame] = _vector3(value, f"frame_targets[{frame}]")
    for value in raw.get("missing_frames", []):
        try:
            missing_frames.add(int(value))
        except (TypeError, ValueError):
            _fail("missing_frames must contain integers", constraint_id=constraint_id)
    for frame in missing_frames:
        frame_targets[frame] = None
    return ConstraintSpec(
        id=constraint_id,
        kind=kind,
        joint=joint,
        weight=weight,
        tolerance=tolerance,
        target_default=target_default,
        frame_targets=tuple(sorted(frame_targets.items())),
        missing_frames=frozenset(missing_frames),
    )


def load_constraints(path: str | Path, document: BvhDocument) -> ConstraintSet:
    target = Path(path)
    try:
        raw = json.loads(target.read_text(encoding="utf-8-sig"))
    except OSError as exc:
        raise BusinessError(
            E_IO, "unable to read constraint file", {"path": str(target), "error": str(exc)}
        ) from exc
    except json.JSONDecodeError as exc:
        raise BusinessError(
            E_CONSTRAINT_INVALID,
            "constraint file is not valid JSON",
            {"path": str(target), "line": exc.lineno, "column": exc.colno},
        ) from exc
    if not isinstance(raw, Mapping):
        _fail("constraint document must be an object")
    entries: list[Mapping[str, Any]] = []
    if "constraints" in raw:
        if not isinstance(raw["constraints"], list):
            _fail("constraints must be a list")
        entries.extend(raw["constraints"])
    for key, kind in (
        ("bone_lengths", "bone_length"),
        ("joint_limits", "joint_limit"),
        ("effectors", "effector"),
    ):
        values = raw.get(key, [])
        if not isinstance(values, list):
            _fail(f"{key} must be a list")
        for item in values:
            merged = dict(item)
            merged["type"] = kind
            entries.append(merged)
    if not entries:
        _fail("constraint document contains no constraints")
    constraints = tuple(_parse_constraint(item, document) for item in entries)
    try:
        revision = int(raw.get("revision", 1))
    except (TypeError, ValueError):
        _fail("constraint revision must be an integer")
    result = ConstraintSet(
        constraints=constraints,
        revision=revision,
        name=str(raw.get("name", target.stem)),
    )
    result.validate(document)
    return result


def constraint_set_from_dict(raw: Mapping[str, Any], document: BvhDocument) -> ConstraintSet:
    entries = raw.get("constraints", [])
    if not isinstance(entries, list):
        _fail("constraints must be a list")
    constraints = tuple(_parse_constraint(item, document) for item in entries)
    try:
        revision = int(raw.get("revision", 1))
    except (TypeError, ValueError):
        _fail("constraint revision must be an integer")
    result = ConstraintSet(
        constraints=constraints,
        revision=revision,
        name=str(raw.get("name", "constraints")),
    )
    result.validate(document)
    return result

