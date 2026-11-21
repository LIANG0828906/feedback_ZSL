"""Immutable units plus the local binary battlefield-scene format."""

from __future__ import annotations

import hashlib
import hmac
import json
import math
import os
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Sequence

import numpy as np

from .errors import ErrorCode, TacticalError, scene_invalid

MAGIC = b"BFSCENE1"
FORMAT_VERSION = 1
HEADER_STRUCT = struct.Struct("<8sHHQII")
UNIT_STRUCT = struct.Struct("<IHBB16d7d")
CHECKSUM_SIZE = 32
MAX_NAME_BYTES = 65_535
TRANSFORM_EPS = 1e-12
BOUNDS_EPS = 1e-9


def _canonical_float(value: float) -> str:
    value = float(value)
    if value == 0.0:
        value = 0.0
    return format(value, ".17g")


def _finite_tuple(values: Iterable[float], *, field: str, unit_id: int) -> tuple[float, ...]:
    result = tuple(float(value) for value in values)
    if not all(math.isfinite(value) for value in result):
        raise scene_invalid(f"{field} contains non-finite values", unit_id=unit_id)
    return result


@dataclass(frozen=True, slots=True)
class AABBShape:
    minimum: tuple[float, float, float]
    maximum: tuple[float, float, float]
    kind: str = "aabb"

    def __post_init__(self) -> None:
        minimum = tuple(float(v) for v in self.minimum)
        maximum = tuple(float(v) for v in self.maximum)
        object.__setattr__(self, "minimum", minimum)
        object.__setattr__(self, "maximum", maximum)
        if not all(math.isfinite(v) for v in minimum + maximum):
            raise scene_invalid("AABB contains non-finite coordinates")
        extents = tuple(maximum[index] - minimum[index] for index in range(3))
        if any(extent <= BOUNDS_EPS for extent in extents):
            raise scene_invalid(
                "AABB is degenerate",
                extents=list(extents),
            )

    @property
    def corners(self) -> np.ndarray:
        x0, y0, z0 = self.minimum
        x1, y1, z1 = self.maximum
        return np.asarray(
            [
                [x0, y0, z0],
                [x0, y0, z1],
                [x0, y1, z0],
                [x0, y1, z1],
                [x1, y0, z0],
                [x1, y0, z1],
                [x1, y1, z0],
                [x1, y1, z1],
            ],
            dtype=np.float64,
        )

    @property
    def edges(self) -> tuple[tuple[int, int], ...]:
        return (
            (0, 1),
            (0, 2),
            (0, 4),
            (1, 3),
            (1, 5),
            (2, 3),
            (2, 6),
            (3, 7),
            (4, 5),
            (4, 6),
            (5, 7),
            (6, 7),
        )


@dataclass(frozen=True, slots=True)
class CapsuleShape:
    a: tuple[float, float, float]
    b: tuple[float, float, float]
    radius: float
    kind: str = "capsule"

    def __post_init__(self) -> None:
        endpoint_a = tuple(float(v) for v in self.a)
        endpoint_b = tuple(float(v) for v in self.b)
        radius = float(self.radius)
        object.__setattr__(self, "a", endpoint_a)
        object.__setattr__(self, "b", endpoint_b)
        object.__setattr__(self, "radius", radius)
        if not all(math.isfinite(v) for v in endpoint_a + endpoint_b + (radius,)):
            raise scene_invalid("capsule contains non-finite values")
        if radius <= BOUNDS_EPS:
            raise scene_invalid("capsule radius is degenerate", radius=radius)


Shape = AABBShape | CapsuleShape


@dataclass(frozen=True, slots=True)
class SceneUnit:
    unit_id: int
    name: str
    shape: Shape
    transform: tuple[float, ...]
    visible: bool = True

    def __post_init__(self) -> None:
        if self.unit_id < 0:
            raise scene_invalid("unit id must be non-negative", unit_id=self.unit_id)
        if not self.name:
            raise scene_invalid("unit name is empty", unit_id=self.unit_id)
        transform = _finite_tuple(self.transform, field="world transform", unit_id=self.unit_id)
        if len(transform) != 16:
            raise scene_invalid("world transform must contain 16 values", unit_id=self.unit_id)
        object.__setattr__(self, "transform", transform)
        matrix = np.asarray(transform, dtype=np.float64).reshape(4, 4)
        affine_error = float(np.max(np.abs(matrix[3] - np.array([0.0, 0.0, 0.0, 1.0]))))
        if affine_error > 1e-9:
            raise scene_invalid(
                "world transform is not affine",
                unit_id=self.unit_id,
                affine_error=affine_error,
            )
        determinant = float(np.linalg.det(matrix))
        if not math.isfinite(determinant) or abs(determinant) < TRANSFORM_EPS:
            raise scene_invalid(
                "world transform is not invertible",
                unit_id=self.unit_id,
                determinant=determinant,
            )
        shape = self.shape
        if not isinstance(shape, (AABBShape, CapsuleShape)):
            raise scene_invalid("unsupported shape type", unit_id=self.unit_id)
        # Constructors on file records need the unit id in diagnostics.  Re-run
        # local validation here so directly-created units get the same contract.
        try:
            shape.__post_init__()
        except TacticalError as exc:
            exc.details.setdefault("unit_id", self.unit_id)
            raise

    @property
    def transform_array(self) -> np.ndarray:
        return np.asarray(self.transform, dtype=np.float64).reshape(4, 4)

    def world_aabb(self) -> tuple[np.ndarray, np.ndarray]:
        if isinstance(self.shape, AABBShape):
            local = self.shape.corners
        else:
            radius = self.shape.radius
            local = np.asarray(
                [
                    self.shape.a,
                    self.shape.b,
                    np.asarray(self.shape.a) + radius,
                    np.asarray(self.shape.a) - radius,
                    np.asarray(self.shape.b) + radius,
                    np.asarray(self.shape.b) - radius,
                ],
                dtype=np.float64,
            )
        homogeneous = np.concatenate([local, np.ones((local.shape[0], 1))], axis=1)
        world = (self.transform_array @ homogeneous.T).T[:, :3]
        return world.min(axis=0), world.max(axis=0)


def _validate_units(units: Sequence[SceneUnit]) -> tuple[SceneUnit, ...]:
    ordered = tuple(sorted(units, key=lambda unit: unit.unit_id))
    seen: set[int] = set()
    for unit in ordered:
        if unit.unit_id in seen:
            raise scene_invalid("duplicate unit id", unit_id=unit.unit_id)
        seen.add(unit.unit_id)
    return ordered


def _units_payload(version: int, units: Sequence[SceneUnit]) -> bytes:
    parts: list[bytes] = []
    for unit in units:
        name = unit.name.encode("utf-8")
        if len(name) > MAX_NAME_BYTES:
            raise scene_invalid("unit name is too long", unit_id=unit.unit_id)
        shape_type = 0 if isinstance(unit.shape, AABBShape) else 1
        parts.append(UNIT_STRUCT.pack(
            unit.unit_id,
            len(name),
            shape_type,
            int(unit.visible),
            *unit.transform,
            *(
                (*unit.shape.minimum, *unit.shape.maximum, 0.0)
                if isinstance(unit.shape, AABBShape)
                else (*unit.shape.a, *unit.shape.b, unit.shape.radius)
            ),
        ))
        parts.append(name)
    return b"".join(parts)


def scene_content_hash(version: int, units: Sequence[SceneUnit]) -> str:
    payload = {
        "format": "tactical-scene-content-v1",
        "version": int(version),
        "units": [
            {
                "unit_id": unit.unit_id,
                "name": unit.name,
                "shape": (
                    {
                        "kind": "aabb",
                        "minimum": [_canonical_float(v) for v in unit.shape.minimum],
                        "maximum": [_canonical_float(v) for v in unit.shape.maximum],
                    }
                    if isinstance(unit.shape, AABBShape)
                    else {
                        "kind": "capsule",
                        "a": [_canonical_float(v) for v in unit.shape.a],
                        "b": [_canonical_float(v) for v in unit.shape.b],
                        "radius": _canonical_float(unit.shape.radius),
                    }
                ),
                "transform": [_canonical_float(v) for v in unit.transform],
                "visible": bool(unit.visible),
            }
            for unit in units
        ],
    }
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


@dataclass(frozen=True, slots=True)
class Scene:
    version: int
    units: tuple[SceneUnit, ...]
    content_hash: str = ""
    source: str | None = None

    def __post_init__(self) -> None:
        if self.version < 1:
            raise scene_invalid("scene version must be a positive integer", version=self.version)
        ordered = _validate_units(self.units)
        object.__setattr__(self, "units", ordered)
        expected = scene_content_hash(self.version, ordered)
        if self.content_hash and self.content_hash != expected:
            raise scene_invalid(
                "scene content hash does not match scene contents",
                expected_hash=expected,
                actual_hash=self.content_hash,
            )
        object.__setattr__(self, "content_hash", expected)

    @property
    def unit_count(self) -> int:
        return len(self.units)

    def unit_map(self) -> dict[int, SceneUnit]:
        return {unit.unit_id: unit for unit in self.units}

    def with_updates(
        self,
        updates: Sequence[SceneUnit],
        *,
        version: int | None = None,
    ) -> "Scene":
        by_id = {unit.unit_id: unit for unit in self.units}
        for unit in updates:
            if unit.unit_id not in by_id:
                raise scene_invalid("cannot update unknown unit", unit_id=unit.unit_id)
            by_id[unit.unit_id] = unit
        return Scene(
            version=self.version + 1 if version is None else int(version),
            units=tuple(by_id.values()),
            source=self.source,
        )


def write_scene(path: str | os.PathLike[str], scene: Scene) -> str:
    """Write a scene atomically and return its canonical content hash."""

    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    body_units = _units_payload(scene.version, scene.units)
    header = HEADER_STRUCT.pack(
        MAGIC,
        FORMAT_VERSION,
        HEADER_STRUCT.size,
        scene.version,
        len(scene.units),
        0,
    )
    body = header + body_units
    checksum = hashlib.sha256(body).digest()
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temporary.open("wb") as handle:
        handle.write(body)
        handle.write(checksum)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    return scene.content_hash


def _read_exact(data: bytes, offset: int, length: int, *, unit_index: int | None = None) -> bytes:
    end = offset + length
    if end > len(data):
        details: dict[str, Any] = {"offset": offset, "required_bytes": length}
        if unit_index is not None:
            details["unit_index"] = unit_index
        raise scene_invalid("truncated binary scene record", **details)
    return data[offset:end]


def _decode_scene(data: bytes, *, source: str | None = None) -> Scene:
    if len(data) < HEADER_STRUCT.size + CHECKSUM_SIZE:
        raise scene_invalid("scene file is shorter than header plus checksum")
    body = data[:-CHECKSUM_SIZE]
    stored_checksum = data[-CHECKSUM_SIZE:]
    actual_checksum = hashlib.sha256(body).digest()
    if not hmac.compare_digest(stored_checksum, actual_checksum):
        raise scene_invalid(
            "scene checksum mismatch",
            expected_checksum=actual_checksum.hex(),
            stored_checksum=stored_checksum.hex(),
        )
    magic, format_version, header_size, version, unit_count, flags = HEADER_STRUCT.unpack_from(body, 0)
    if magic != MAGIC:
        raise scene_invalid("unrecognized scene magic")
    if format_version != FORMAT_VERSION:
        raise scene_invalid(
            "unsupported scene format version",
            format_version=format_version,
            supported=FORMAT_VERSION,
        )
    if header_size != HEADER_STRUCT.size:
        raise scene_invalid("invalid scene header size", header_size=header_size)
    if version < 1:
        raise scene_invalid("scene version field is incomplete", version=version)
    if flags != 0:
        raise scene_invalid("unsupported scene flags", flags=flags)

    offset = header_size
    units: list[SceneUnit] = []
    seen_ids: set[int] = set()
    for unit_index in range(unit_count):
        record = _read_exact(body, offset, UNIT_STRUCT.size, unit_index=unit_index)
        (
            unit_id,
            name_length,
            shape_type,
            visible_value,
            *numeric_values,
        ) = UNIT_STRUCT.unpack(record)
        offset += UNIT_STRUCT.size
        if unit_id in seen_ids:
            raise scene_invalid("duplicate unit id", unit_id=unit_id, unit_index=unit_index)
        seen_ids.add(unit_id)
        name_bytes = _read_exact(body, offset, name_length, unit_index=unit_index)
        offset += name_length
        try:
            name = name_bytes.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise scene_invalid(
                "unit name is not valid UTF-8",
                unit_id=unit_id,
                unit_index=unit_index,
            ) from exc
        if visible_value not in (0, 1):
            raise scene_invalid(
                "visibility field must be 0 or 1",
                unit_id=unit_id,
                unit_index=unit_index,
                value=visible_value,
            )
        transform = tuple(float(v) for v in numeric_values[:16])
        geometry = tuple(float(v) for v in numeric_values[16:23])
        try:
            if shape_type == 0:
                minimum = geometry[0:3]
                maximum = geometry[3:6]
                if any(not math.isfinite(v) for v in minimum + maximum):
                    raise scene_invalid("AABB contains non-finite coordinates", unit_id=unit_id)
                if any(maximum[index] - minimum[index] <= BOUNDS_EPS for index in range(3)):
                    raise scene_invalid("AABB is degenerate", unit_id=unit_id)
                shape: Shape = AABBShape(minimum, maximum)
            elif shape_type == 1:
                radius = geometry[6]
                if not math.isfinite(radius) or radius <= BOUNDS_EPS:
                    raise scene_invalid(
                        "capsule radius is degenerate",
                        unit_id=unit_id,
                        radius=radius,
                    )
                shape = CapsuleShape(geometry[0:3], geometry[3:6], radius)
            else:
                raise scene_invalid(
                    "unsupported shape type",
                    unit_id=unit_id,
                    shape_type=shape_type,
                )
            unit = SceneUnit(
                unit_id=unit_id,
                name=name,
                shape=shape,
                transform=transform,
                visible=bool(visible_value),
            )
        except TacticalError as exc:
            exc.details.setdefault("unit_id", unit_id)
            exc.details.setdefault("unit_index", unit_index)
            raise
        units.append(unit)
    if offset != len(body):
        raise scene_invalid(
            "scene contains trailing record bytes",
            expected_offset=offset,
            actual_size=len(body),
        )
    try:
        return Scene(version=version, units=tuple(units), source=source)
    except TacticalError as exc:
        exc.details.setdefault("unit_count", unit_count)
        raise


def load_scene(path: str | os.PathLike[str]) -> Scene:
    path = Path(path)
    try:
        data = path.read_bytes()
    except OSError as exc:
        raise scene_invalid("cannot read scene file", path=str(path), reason=str(exc)) from exc
    return _decode_scene(data, source=str(path))
