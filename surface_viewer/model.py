from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

import numpy as np

from .errors import invalid
from .hashing import sha256_json

MIN_PRECISION_LEVEL = 0
MAX_PRECISION_LEVEL = 4
SURFACE_TYPES = ("bezier", "bspline", "nurbs")
PARAMETER_NAMES = (
    "control_points",
    "surface_type",
    "precision_level",
    "flip_normal",
)

SEGMENTS_BY_PRECISION = {
    0: 4,
    1: 8,
    2: 16,
    3: 24,
    4: 32,
}


def _is_number(value: Any) -> bool:
    return (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(float(value))
    )


def _normalize_control_points(
    value: Any, field: str, surface_type: str
) -> tuple[tuple[tuple[float, ...], ...], ...]:
    if not isinstance(value, list):
        raise invalid(field, "control_points must be a two-dimensional array")
    if len(value) < 2:
        raise invalid(field, "control point grid must have at least two rows")

    rows: list[tuple[tuple[float, ...], ...]] = []
    expected_cols: int | None = None
    expected_point_len = 4 if surface_type == "nurbs" else 3
    actual_point_len: int | None = None

    for row_index, raw_row in enumerate(value):
        row_field = f"{field}[{row_index}]"
        if not isinstance(raw_row, list):
            raise invalid(row_field, "control point row must be an array")
        if expected_cols is None:
            expected_cols = len(raw_row)
            if expected_cols < 2:
                raise invalid(
                    field, "control point grid must have at least two columns"
                )
        elif len(raw_row) != expected_cols:
            raise invalid(
                row_field,
                "control point grid has inconsistent row dimensions",
                expected=expected_cols,
                actual=len(raw_row),
            )

        normalized_row: list[tuple[float, ...]] = []
        for col_index, raw_point in enumerate(raw_row):
            point_field = f"{row_field}[{col_index}]"
            if not isinstance(raw_point, list):
                raise invalid(point_field, "control point must be an array")
            if surface_type == "nurbs":
                if len(raw_point) not in (3, 4):
                    raise invalid(
                        point_field,
                        "nurbs control point must have 3 or 4 coordinates",
                        actual=len(raw_point),
                    )
                if actual_point_len is None:
                    actual_point_len = len(raw_point)
                elif len(raw_point) != actual_point_len:
                    raise invalid(
                        point_field,
                        "nurbs control point dimensions are inconsistent",
                        expected=actual_point_len,
                        actual=len(raw_point),
                    )
            elif len(raw_point) != expected_point_len:
                raise invalid(
                    point_field,
                    "control point must have exactly three coordinates",
                    actual=len(raw_point),
                )
            if not all(_is_number(coordinate) for coordinate in raw_point):
                raise invalid(
                    point_field, "control point coordinates must be finite numbers"
                )
            point = [float(coordinate) for coordinate in raw_point]
            if len(point) == 4 and point[3] <= 0.0:
                raise invalid(
                    point_field, "nurbs control point weight must be positive"
                )
            normalized_row.append(tuple(point))
        rows.append(tuple(normalized_row))

    return tuple(rows)


@dataclass(frozen=True)
class SurfacePatch:
    patch_id: str
    block_id: int
    surface_type: str
    precision_level: int
    flip_normal: bool
    control_points: tuple[tuple[tuple[float, ...], ...], ...]

    @property
    def segments(self) -> int:
        return SEGMENTS_BY_PRECISION[self.precision_level]

    @property
    def control_shape(self) -> tuple[int, int]:
        return (len(self.control_points), len(self.control_points[0]))

    def parameter_value(self, name: str) -> Any:
        if name == "control_points":
            return self.control_points
        if name == "surface_type":
            return self.surface_type
        if name == "precision_level":
            return self.precision_level
        if name == "flip_normal":
            return self.flip_normal
        raise KeyError(name)

    @property
    def parameter_versions(self) -> dict[str, str]:
        return {
            name: sha256_json(
                {
                    "patch_id": self.patch_id,
                    "parameter": name,
                    "value": self.parameter_value(name),
                }
            )
            for name in PARAMETER_NAMES
        }

    @property
    def version_hash(self) -> str:
        return sha256_json(self.to_dict())

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.patch_id,
            "block_id": self.block_id,
            "surface_type": self.surface_type,
            "precision_level": self.precision_level,
            "flip_normal": self.flip_normal,
            "control_points": [
                [list(point) for point in row] for row in self.control_points
            ],
        }

    def with_changes(self, changes: Mapping[str, Any]) -> "SurfacePatch":
        unknown = sorted(set(changes) - set(PARAMETER_NAMES))
        if unknown:
            raise invalid(
                f"patches[{self.patch_id}].{unknown[0]}",
                "unsupported surface parameter",
                parameter=unknown[0],
            )
        data = self.to_dict()
        for name, value in changes.items():
            if name == "control_points" and isinstance(value, np.ndarray):
                value = value.tolist()
            data[name] = value
        return _parse_patch(data, f"patches[{self.patch_id}]")


@dataclass(frozen=True)
class SurfaceAssembly:
    assembly_id: str
    patches: tuple[SurfacePatch, ...]

    @property
    def by_id(self) -> dict[str, SurfacePatch]:
        return {patch.patch_id: patch for patch in self.patches}

    @property
    def by_block(self) -> dict[int, SurfacePatch]:
        return {patch.block_id: patch for patch in self.patches}

    @property
    def version_hash(self) -> str:
        return sha256_json(self.to_dict())

    def patch(self, patch_id: str) -> SurfacePatch:
        for patch in self.patches:
            if patch.patch_id == patch_id:
                return patch
        raise invalid(
            f"patches[{patch_id}]", "surface patch does not exist", patch_id=patch_id
        )

    def apply_changes(
        self, patch_id: str, changes: Mapping[str, Any]
    ) -> "SurfaceAssembly":
        old_patch = self.patch(patch_id)
        replacement = old_patch.with_changes(changes)
        patch_data = [
            patch.to_dict() if patch.patch_id != patch_id else replacement.to_dict()
            for patch in self.patches
        ]
        if "control_points" in changes:
            self._propagate_shared_boundary_changes(
                old_patch, replacement, patch_data
            )
        return parse_assembly(
            {
                "schema_version": 1,
                "assembly_id": self.assembly_id,
                "patches": patch_data,
            }
        )

    def _propagate_shared_boundary_changes(
        self,
        old_patch: SurfacePatch,
        replacement: SurfacePatch,
        patch_data: list[dict[str, Any]],
    ) -> None:
        sides = ("u0", "u1", "v0", "v1")
        for side in sides:
            old_boundary = _boundary_points(old_patch, side)
            new_boundary = _boundary_points(replacement, side)
            if old_boundary == new_boundary:
                continue
            match = _find_shared_boundary(self, old_patch, side)
            if match is None:
                continue
            neighbor, neighbor_side, reversed_order = match
            neighbor_data = next(
                data
                for data in patch_data
                if data["id"] == neighbor.patch_id
            )
            _set_boundary_points(
                neighbor_data,
                neighbor_side,
                tuple(reversed(new_boundary)) if reversed_order else new_boundary,
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "assembly_id": self.assembly_id,
            "patches": [patch.to_dict() for patch in self.patches],
        }


def _parse_patch(raw: Any, field: str) -> SurfacePatch:
    if not isinstance(raw, dict):
        raise invalid(field, "surface patch must be an object")

    required = (
        "id",
        "control_points",
        "surface_type",
        "precision_level",
        "flip_normal",
        "block_id",
    )
    for name in required:
        if name not in raw:
            raise invalid(
                f"{field}.{name}",
                "required surface patch field is missing",
                missing_field=name,
            )

    patch_id = raw["id"]
    if not isinstance(patch_id, str) or not patch_id.strip():
        raise invalid(f"{field}.id", "patch id must be a non-empty string")

    surface_type = raw["surface_type"]
    if surface_type not in SURFACE_TYPES:
        raise invalid(
            f"{field}.surface_type",
            "surface_type is not supported",
            allowed=list(SURFACE_TYPES),
            actual=surface_type,
        )

    precision = raw["precision_level"]
    if (
        not isinstance(precision, int)
        or isinstance(precision, bool)
        or not MIN_PRECISION_LEVEL <= precision <= MAX_PRECISION_LEVEL
    ):
        raise invalid(
            f"{field}.precision_level",
            "precision level is out of range",
            minimum=MIN_PRECISION_LEVEL,
            maximum=MAX_PRECISION_LEVEL,
            actual=precision,
        )

    flip_normal = raw["flip_normal"]
    if not isinstance(flip_normal, bool):
        raise invalid(f"{field}.flip_normal", "flip_normal must be a boolean")

    block_id = raw["block_id"]
    if (
        not isinstance(block_id, int)
        or isinstance(block_id, bool)
        or block_id < 0
    ):
        raise invalid(
            f"{field}.block_id", "block_id must be a non-negative integer"
        )

    control_points = _normalize_control_points(
        raw["control_points"], f"{field}.control_points", surface_type
    )
    return SurfacePatch(
        patch_id=patch_id,
        block_id=block_id,
        surface_type=surface_type,
        precision_level=precision,
        flip_normal=flip_normal,
        control_points=control_points,
    )


def parse_assembly(raw: Any) -> SurfaceAssembly:
    if not isinstance(raw, dict):
        raise invalid("$", "surface description must be a JSON object")

    if "patches" not in raw:
        raise invalid(
            "$.patches", "required field is missing", missing_field="patches"
        )
    patches_raw = raw["patches"]
    if not isinstance(patches_raw, list) or not patches_raw:
        raise invalid("$.patches", "patches must be a non-empty array")

    assembly_id = raw.get("assembly_id")
    if not isinstance(assembly_id, str) or not assembly_id.strip():
        raise invalid("$.assembly_id", "assembly_id must be a non-empty string")

    patches = [
        _parse_patch(value, f"patches[{index}]")
        for index, value in enumerate(patches_raw)
    ]

    seen_ids: dict[str, int] = {}
    seen_blocks: dict[int, int] = {}
    for index, patch in enumerate(patches):
        if patch.patch_id in seen_ids:
            raise invalid(
                f"patches[{index}].id",
                "patch id is duplicated",
                duplicate_id=patch.patch_id,
                first_index=seen_ids[patch.patch_id],
            )
        seen_ids[patch.patch_id] = index
        if patch.block_id in seen_blocks:
            raise invalid(
                f"patches[{index}].block_id",
                "block id is duplicated",
                duplicate_block_id=patch.block_id,
                first_index=seen_blocks[patch.block_id],
            )
        seen_blocks[patch.block_id] = index

    return SurfaceAssembly(assembly_id=assembly_id, patches=tuple(patches))


def load_assembly(path: str | Path) -> SurfaceAssembly:
    source = Path(path)
    try:
        raw = json.loads(source.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise invalid("$", "surface description file does not exist", path=str(source))
    except json.JSONDecodeError as exc:
        raise invalid(
            "$",
            "surface description is not valid JSON",
            path=str(source),
            line=exc.lineno,
            column=exc.colno,
        )
    return parse_assembly(raw)


def save_assembly(assembly: SurfaceAssembly, path: str | Path) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(
        json.dumps(
            assembly.to_dict(),
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
            allow_nan=False,
        )
        + "\n",
        encoding="utf-8",
    )


def changed_parameter_names(
    old_patch: SurfacePatch, new_patch: SurfacePatch
) -> tuple[str, ...]:
    return tuple(
        name
        for name in PARAMETER_NAMES
        if old_patch.parameter_value(name) != new_patch.parameter_value(name)
    )


def _boundary_points(
    patch: SurfacePatch, side: str
) -> tuple[tuple[float, ...], ...]:
    if side == "u0":
        return patch.control_points[0]
    if side == "u1":
        return patch.control_points[-1]
    if side == "v0":
        return tuple(row[0] for row in patch.control_points)
    if side == "v1":
        return tuple(row[-1] for row in patch.control_points)
    raise ValueError(side)


def _find_shared_boundary(
    assembly: SurfaceAssembly, patch: SurfacePatch, side: str
) -> tuple[SurfacePatch, str, bool] | None:
    own = _boundary_points(patch, side)
    for neighbor in assembly.patches:
        if neighbor.patch_id == patch.patch_id:
            continue
        if neighbor.surface_type != patch.surface_type:
            continue
        for neighbor_side in ("u0", "u1", "v0", "v1"):
            other = _boundary_points(neighbor, neighbor_side)
            if len(other) != len(own):
                continue
            if own == other:
                return neighbor, neighbor_side, False
            if own == tuple(reversed(other)):
                return neighbor, neighbor_side, True
    return None


def _set_boundary_points(
    patch_data: dict[str, Any],
    side: str,
    points: Sequence[tuple[float, ...]],
) -> None:
    control = patch_data["control_points"]
    serialized = [list(point) for point in points]
    if side == "u0":
        control[0] = serialized
    elif side == "u1":
        control[-1] = serialized
    elif side == "v0":
        if len(control) != len(serialized):
            raise invalid(
                "control_points", "shared boundary dimensions are inconsistent"
            )
        for row, point in zip(control, serialized, strict=True):
            row[0] = point
    elif side == "v1":
        if len(control) != len(serialized):
            raise invalid(
                "control_points", "shared boundary dimensions are inconsistent"
            )
        for row, point in zip(control, serialized, strict=True):
            row[-1] = point
    else:
        raise ValueError(side)


def assembly_artifact_ids(patches: Iterable[SurfacePatch]) -> tuple[str, ...]:
    return tuple(
        f"artifact:{patch.patch_id}:{kind}"
        for patch in patches
        for kind in ("mesh", "normals", "density", "preview")
    )
