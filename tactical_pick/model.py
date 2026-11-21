"""Immutable battlefield state and strict validation."""

from __future__ import annotations

import math
from dataclasses import dataclass, replace
from typing import Sequence

from .errors import E_SCENE_INVALID, fail
from .geometry import EPS, Vec3, finite_vec

MAX_ENTITY_ID = 2**31 - 1


def _require_finite(value: float, field: str, entity_id: int | None = None) -> None:
    if not math.isfinite(value):
        fail(
            E_SCENE_INVALID,
            "scene contains a non-finite number",
            field=field,
            entity_id=entity_id,
            value=str(value),
        )


@dataclass(frozen=True)
class Unit:
    unit_id: int
    name: str
    bounds_kind: str
    minimum: Vec3
    maximum: Vec3
    capsule_a: Vec3 | None = None
    capsule_b: Vec3 | None = None
    radius: float = 0.0
    active: bool = True

    def translated(self, delta: Vec3) -> "Unit":
        dx, dy, dz = delta
        minimum = (self.minimum[0] + dx, self.minimum[1] + dy, self.minimum[2] + dz)
        maximum = (self.maximum[0] + dx, self.maximum[1] + dy, self.maximum[2] + dz)
        capsule_a = None
        capsule_b = None
        if self.capsule_a is not None:
            capsule_a = (
                self.capsule_a[0] + dx,
                self.capsule_a[1] + dy,
                self.capsule_a[2] + dz,
            )
        if self.capsule_b is not None:
            capsule_b = (
                self.capsule_b[0] + dx,
                self.capsule_b[1] + dy,
                self.capsule_b[2] + dz,
            )
        return replace(
            self,
            minimum=minimum,
            maximum=maximum,
            capsule_a=capsule_a,
            capsule_b=capsule_b,
        )


@dataclass(frozen=True)
class Cover:
    cover_id: int
    i0: int
    i1: int
    i2: int
    name: str = ""


@dataclass(frozen=True)
class Grid:
    cols: int
    rows: int
    cell_size: float
    origin_x: float
    origin_y: float
    walkable: bytes

    @property
    def cell_count(self) -> int:
        return self.cols * self.rows

    @property
    def required_mask_bytes(self) -> int:
        return (self.cell_count + 7) // 8

    @property
    def maximum_x(self) -> float:
        return self.origin_x + self.cols * self.cell_size

    @property
    def maximum_y(self) -> float:
        return self.origin_y + self.rows * self.cell_size

    def cell_center(self, row: int, col: int) -> tuple[float, float]:
        return (
            self.origin_x + (col + 0.5) * self.cell_size,
            self.origin_y + (row + 0.5) * self.cell_size,
        )

    def cell_at(self, x: float, y: float) -> tuple[int, int] | None:
        if (
            x < self.origin_x
            or y < self.origin_y
            or x >= self.maximum_x
            or y >= self.maximum_y
        ):
            return None
        col = int(math.floor((x - self.origin_x) / self.cell_size))
        row = int(math.floor((y - self.origin_y) / self.cell_size))
        col = min(max(col, 0), self.cols - 1)
        row = min(max(row, 0), self.rows - 1)
        return (row, col)

    def is_walkable(self, row: int, col: int) -> bool:
        if row < 0 or col < 0 or row >= self.rows or col >= self.cols:
            return False
        bit = row * self.cols + col
        return bool(self.walkable[bit >> 3] & (1 << (bit & 7)))

    def set_walkable(self, row: int, col: int, walkable: bool) -> "Grid":
        if row < 0 or col < 0 or row >= self.rows or col >= self.cols:
            fail(
                E_SCENE_INVALID,
                "grid coordinate is outside the battlefield",
                field="grid.cell",
                row=row,
                col=col,
            )
        mask = bytearray(self.walkable)
        bit = row * self.cols + col
        if walkable:
            mask[bit >> 3] |= 1 << (bit & 7)
        else:
            mask[bit >> 3] &= ~(1 << (bit & 7))
        return replace(self, walkable=bytes(mask))

    def with_walkable_mask(self, mask: bytes) -> "Grid":
        return replace(self, walkable=bytes(mask))


@dataclass(frozen=True)
class Scene:
    structure_version: int
    ground_height: float
    grid: Grid
    vertices: tuple[Vec3, ...]
    units: tuple[Unit, ...]
    covers: tuple[Cover, ...]

    def triangle(self, cover: Cover) -> tuple[Vec3, Vec3, Vec3]:
        return (
            self.vertices[cover.i0],
            self.vertices[cover.i1],
            self.vertices[cover.i2],
        )

    def active_units(self) -> tuple[Unit, ...]:
        return tuple(unit for unit in self.units if unit.active)

    def unit_by_id(self, unit_id: int) -> Unit | None:
        for unit in self.units:
            if unit.unit_id == unit_id:
                return unit
        return None

    def cover_by_id(self, cover_id: int) -> Cover | None:
        for cover in self.covers:
            if cover.cover_id == cover_id:
                return cover
        return None


def _validate_unit(unit: Unit, index: int) -> None:
    entity = unit.unit_id
    if unit.unit_id < 0 or unit.unit_id > MAX_ENTITY_ID:
        fail(
            E_SCENE_INVALID,
            "unit id is outside the supported integer range",
            field=f"units[{index}].unit_id",
            entity_id=entity,
        )
    if not unit.name:
        fail(
            E_SCENE_INVALID,
            "unit name must not be empty",
            field=f"units[{index}].name",
            entity_id=entity,
        )
    if unit.bounds_kind not in ("aabb", "capsule"):
        fail(
            E_SCENE_INVALID,
            "unit bounds kind must be aabb or capsule",
            field=f"units[{index}].bounds_kind",
            entity_id=entity,
        )
    if not finite_vec((*unit.minimum, *unit.maximum)):
        fail(
            E_SCENE_INVALID,
            "unit bounds contain a non-finite number",
            field=f"units[{index}].minimum",
            entity_id=entity,
        )
    for axis in range(3):
        if unit.minimum[axis] > unit.maximum[axis] + EPS:
            fail(
                E_SCENE_INVALID,
                "unit AABB minimum exceeds maximum",
                field=f"units[{index}].minimum",
                entity_id=entity,
                axis=axis,
            )
        if abs(unit.maximum[axis] - unit.minimum[axis]) <= EPS:
            fail(
                E_SCENE_INVALID,
                "unit bounds are degenerate on at least one axis",
                field=f"units[{index}].maximum",
                entity_id=entity,
                axis=axis,
            )
    if unit.bounds_kind == "capsule":
        if unit.capsule_a is None or unit.capsule_b is None:
            fail(
                E_SCENE_INVALID,
                "capsule bounds require both segment endpoints",
                field=f"units[{index}].capsule_a",
                entity_id=entity,
            )
        if not finite_vec((*unit.capsule_a, *unit.capsule_b, unit.radius)):
            fail(
                E_SCENE_INVALID,
                "capsule bounds contain a non-finite number",
                field=f"units[{index}].capsule_a",
                entity_id=entity,
            )
        if not math.isfinite(unit.radius) or unit.radius <= EPS:
            fail(
                E_SCENE_INVALID,
                "capsule radius must be positive",
                field=f"units[{index}].radius",
                entity_id=entity,
            )
        derived_min = tuple(
            min(unit.capsule_a[axis], unit.capsule_b[axis]) - unit.radius
            for axis in range(3)
        )
        derived_max = tuple(
            max(unit.capsule_a[axis], unit.capsule_b[axis]) + unit.radius
            for axis in range(3)
        )
        for axis in range(3):
            if (
                unit.minimum[axis] > derived_min[axis] + EPS
                or unit.maximum[axis] < derived_max[axis] - EPS
            ):
                fail(
                    E_SCENE_INVALID,
                    "unit AABB does not contain its capsule bounds",
                    field=f"units[{index}].minimum",
                    entity_id=entity,
                    axis=axis,
                )
    elif unit.radius != 0.0:
        fail(
            E_SCENE_INVALID,
            "AABB units must not carry a capsule radius",
            field=f"units[{index}].radius",
            entity_id=entity,
        )


def _validate_grid(grid: Grid) -> None:
    if grid.cols <= 0 or grid.rows <= 0:
        fail(
            E_SCENE_INVALID,
            "grid dimensions must be positive",
            field="grid.size",
            cols=grid.cols,
            rows=grid.rows,
        )
    if not math.isfinite(grid.cell_size) or grid.cell_size <= EPS:
        fail(E_SCENE_INVALID, "grid cell size must be positive", field="grid.cell_size")
    if not math.isfinite(grid.origin_x) or not math.isfinite(grid.origin_y):
        fail(E_SCENE_INVALID, "grid origin must be finite", field="grid.origin")
    required = grid.required_mask_bytes
    if len(grid.walkable) != required:
        fail(
            E_SCENE_INVALID,
            "walkable mask byte length does not match the cell count",
            field="grid.walkable",
            expected=required,
            actual=len(grid.walkable),
            cells=grid.cell_count,
        )
    unused = grid.cell_count & 7
    if unused and len(grid.walkable):
        high_mask = (0xFF << unused) & 0xFF
        if grid.walkable[-1] & high_mask:
            fail(
                E_SCENE_INVALID,
                "walkable mask has non-zero padding bits",
                field="grid.walkable",
                byte_index=len(grid.walkable) - 1,
            )


def validate_scene(scene: Scene) -> Scene:
    """Validate every scene invariant and return the scene unchanged."""

    if scene.structure_version < 0:
        fail(
            E_SCENE_INVALID,
            "scene structure version must be non-negative",
            field="structure_version",
            entity_id=scene.structure_version,
        )
    if not math.isfinite(scene.ground_height):
        fail(E_SCENE_INVALID, "ground height must be finite", field="ground_height")
    _validate_grid(scene.grid)

    seen_unit_ids: set[int] = set()
    for index, unit in enumerate(scene.units):
        _validate_unit(unit, index)
        if unit.unit_id in seen_unit_ids:
            fail(
                E_SCENE_INVALID,
                "duplicate unit id",
                field=f"units[{index}].unit_id",
                entity_id=unit.unit_id,
            )
        seen_unit_ids.add(unit.unit_id)

    for index, vertex in enumerate(scene.vertices):
        if not finite_vec(vertex):
            fail(
                E_SCENE_INVALID,
                "cover vertex contains a non-finite number",
                field=f"vertices[{index}]",
                entity_id=index,
            )

    seen_cover_ids: set[int] = set()
    vertex_count = len(scene.vertices)
    for index, cover in enumerate(scene.covers):
        if cover.cover_id < 0 or cover.cover_id > MAX_ENTITY_ID:
            fail(
                E_SCENE_INVALID,
                "cover id is outside the supported integer range",
                field=f"covers[{index}].cover_id",
                entity_id=cover.cover_id,
            )
        if cover.cover_id in seen_cover_ids:
            fail(
                E_SCENE_INVALID,
                "duplicate cover id",
                field=f"covers[{index}].cover_id",
                entity_id=cover.cover_id,
            )
        seen_cover_ids.add(cover.cover_id)
        for label, vertex_index in (("i0", cover.i0), ("i1", cover.i1), ("i2", cover.i2)):
            if vertex_index < 0 or vertex_index >= vertex_count:
                fail(
                    E_SCENE_INVALID,
                    "cover face references an invalid vertex index",
                    field=f"covers[{index}].{label}",
                    entity_id=cover.cover_id,
                    index=vertex_index,
                    vertex_count=vertex_count,
                )
        if cover.i0 == cover.i1 or cover.i1 == cover.i2 or cover.i2 == cover.i0:
            fail(
                E_SCENE_INVALID,
                "cover face indices must identify three distinct vertices",
                field=f"covers[{index}].indices",
                entity_id=cover.cover_id,
            )
        a, b, c = scene.triangle(cover)
        ab = (b[0] - a[0], b[1] - a[1], b[2] - a[2])
        ac = (c[0] - a[0], c[1] - a[1], c[2] - a[2])
        cx = ab[1] * ac[2] - ab[2] * ac[1]
        cy = ab[2] * ac[0] - ab[0] * ac[2]
        cz = ab[0] * ac[1] - ab[1] * ac[0]
        if math.sqrt(cx * cx + cy * cy + cz * cz) <= EPS:
            fail(
                E_SCENE_INVALID,
                "cover triangle is degenerate",
                field=f"covers[{index}].indices",
                entity_id=cover.cover_id,
            )
    return scene


def scene_replace_units(scene: Scene, units: Sequence[Unit]) -> Scene:
    return replace(scene, units=tuple(units))


def scene_replace_covers(
    scene: Scene,
    vertices: Sequence[Vec3],
    covers: Sequence[Cover],
) -> Scene:
    return replace(
        scene,
        vertices=tuple(vertices),
        covers=tuple(covers),
    )


def scene_with_grid(scene: Scene, grid: Grid) -> Scene:
    return replace(scene, grid=grid)


def scene_structure(scene: Scene) -> dict[str, object]:
    """Stable structural representation used by tests and diagnostics."""

    return {
        "structure_version": scene.structure_version,
        "ground_height": scene.ground_height,
        "grid": {
            "cols": scene.grid.cols,
            "rows": scene.grid.rows,
            "cell_size": scene.grid.cell_size,
            "origin_x": scene.grid.origin_x,
            "origin_y": scene.grid.origin_y,
            "walkable_hex": scene.grid.walkable.hex(),
        },
        "vertices": scene.vertices,
        "units": scene.units,
        "covers": scene.covers,
    }
