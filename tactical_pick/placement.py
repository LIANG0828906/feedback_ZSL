"""Draft placement-region validation on one immutable snapshot."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Sequence

from .errors import (
    E_REGION_COVER_OVERLAP,
    E_REGION_INVALID,
    E_REGION_UNIT_OVERLAP,
    E_REGION_UNWALKABLE,
    E_VERSION_STALE,
    fail,
)
from .geometry import EPS, Vec2, clamp, point_segment_distance_sq_2d
from .versioning import RegionSnapshot, SnapshotHandle, VersionStore


@dataclass(frozen=True)
class PlacementResult:
    valid: bool
    failure_code: str | None
    message: str
    failed_vertex: int | None
    conflict_entity: str | None
    conflict_point: Vec2 | None
    covered_cells: tuple[tuple[int, int], ...]
    version: int
    content_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "valid": self.valid,
            "failure_code": self.failure_code,
            "message": self.message,
            "failed_vertex": self.failed_vertex,
            "conflict_entity": self.conflict_entity,
            "conflict_point": list(self.conflict_point)
            if self.conflict_point is not None
            else None,
            "covered_cells": [list(cell) for cell in self.covered_cells],
            "region_version": self.version,
            "region_hash": self.content_hash,
        }


def _cross2(a: Vec2, b: Vec2, c: Vec2) -> float:
    return (b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0])


def _point_on_segment(point: Vec2, a: Vec2, b: Vec2) -> bool:
    return point_segment_distance_sq_2d(point, a, b) <= EPS * EPS


def _segments_intersect(a: Vec2, b: Vec2, c: Vec2, d: Vec2) -> bool:
    o1 = _cross2(a, b, c)
    o2 = _cross2(a, b, d)
    o3 = _cross2(c, d, a)
    o4 = _cross2(c, d, b)
    if ((o1 > EPS and o2 < -EPS) or (o1 < -EPS and o2 > EPS)) and (
        (o3 > EPS and o4 < -EPS) or (o3 < -EPS and o4 > EPS)
    ):
        return True
    if abs(o1) <= EPS and _point_on_segment(c, a, b):
        return True
    if abs(o2) <= EPS and _point_on_segment(d, a, b):
        return True
    if abs(o3) <= EPS and _point_on_segment(a, c, d):
        return True
    if abs(o4) <= EPS and _point_on_segment(b, c, d):
        return True
    return False


def _point_in_polygon(point: Vec2, polygon: Sequence[Vec2]) -> bool:
    inside = False
    count = len(polygon)
    for index in range(count):
        a = polygon[index]
        b = polygon[(index + 1) % count]
        if _point_on_segment(point, a, b):
            return True
        if (a[1] > point[1]) != (b[1] > point[1]):
            x_intersection = (b[0] - a[0]) * (point[1] - a[1]) / (b[1] - a[1]) + a[0]
            if point[0] < x_intersection:
                inside = not inside
    return inside


def _polygon_area(polygon: Sequence[Vec2]) -> float:
    total = 0.0
    count = len(polygon)
    for index in range(count):
        x1, y1 = polygon[index]
        x2, y2 = polygon[(index + 1) % count]
        total += x1 * y2 - x2 * y1
    return total * 0.5


def _polygon_edges_overlap(poly_a: Sequence[Vec2], poly_b: Sequence[Vec2]) -> bool:
    for index in range(len(poly_a)):
        a = poly_a[index]
        b = poly_a[(index + 1) % len(poly_a)]
        for other_index in range(len(poly_b)):
            c = poly_b[other_index]
            d = poly_b[(other_index + 1) % len(poly_b)]
            if _segments_intersect(a, b, c, d):
                return True
    return False


def _polygons_overlap(poly_a: Sequence[Vec2], poly_b: Sequence[Vec2]) -> bool:
    if _polygon_edges_overlap(poly_a, poly_b):
        return True
    if any(_point_in_polygon(point, poly_b) for point in poly_a):
        return True
    if any(_point_in_polygon(point, poly_a) for point in poly_b):
        return True
    return False


def _polygon_rectangle_overlap(
    polygon: Sequence[Vec2],
    minimum: Vec2,
    maximum: Vec2,
) -> bool:
    rectangle = (
        (minimum[0], minimum[1]),
        (maximum[0], minimum[1]),
        (maximum[0], maximum[1]),
        (minimum[0], maximum[1]),
    )
    return _polygons_overlap(polygon, rectangle)


def _segment_polygon_distance_sq(a: Vec2, b: Vec2, polygon: Sequence[Vec2]) -> float:
    if _point_in_polygon(a, polygon) or _point_in_polygon(b, polygon):
        return 0.0
    for index in range(len(polygon)):
        if _segments_intersect(a, b, polygon[index], polygon[(index + 1) % len(polygon)]):
            return 0.0
    best = math.inf
    for point in polygon:
        best = min(best, point_segment_distance_sq_2d(point, a, b))
    for index in range(len(polygon)):
        c = polygon[index]
        d = polygon[(index + 1) % len(polygon)]
        best = min(best, point_segment_distance_sq_2d(a, c, d))
        best = min(best, point_segment_distance_sq_2d(b, c, d))
    return best


def _nearest_vertex(polygon: Sequence[Vec2], point: Vec2) -> int:
    best_index = 0
    best_distance = math.inf
    for index, vertex in enumerate(polygon):
        dx = vertex[0] - point[0]
        dy = vertex[1] - point[1]
        distance = dx * dx + dy * dy
        if distance < best_distance:
            best_distance = distance
            best_index = index
    return best_index


def _validate_region(
    polygon: Sequence[Vec2],
    snapshot: RegionSnapshot,
) -> tuple[tuple[int, int], ...]:
    if len(polygon) < 3:
        fail(
            E_REGION_INVALID,
            "placement region requires at least three vertices",
            field="region.vertices",
            actual=len(polygon),
        )
    for index, vertex in enumerate(polygon):
        if not math.isfinite(vertex[0]) or not math.isfinite(vertex[1]):
            fail(
                E_REGION_INVALID,
                "placement vertex is not finite",
                field=f"region.vertices[{index}]",
                vertex_index=index,
            )
    grid = snapshot.scene.grid
    for index, vertex in enumerate(polygon):
        if (
            vertex[0] < grid.origin_x - EPS
            or vertex[1] < grid.origin_y - EPS
            or vertex[0] > grid.maximum_x + EPS
            or vertex[1] > grid.maximum_y + EPS
        ):
            fail(
                E_REGION_INVALID,
                "placement vertex is outside the grid bounds",
                field=f"region.vertices[{index}]",
                vertex_index=index,
                vertex=list(vertex),
                grid_bounds=[
                    grid.origin_x,
                    grid.origin_y,
                    grid.maximum_x,
                    grid.maximum_y,
                ],
            )
    for first in range(len(polygon)):
        next_vertex = polygon[(first + 1) % len(polygon)]
        dx = next_vertex[0] - polygon[first][0]
        dy = next_vertex[1] - polygon[first][1]
        if dx * dx + dy * dy <= EPS * EPS:
            fail(
                E_REGION_INVALID,
                "placement region has zero-length edges",
                field=f"region.vertices[{first}]",
                vertex_index=first,
            )
        for second in range(first + 1, len(polygon)):
            dx = polygon[first][0] - polygon[second][0]
            dy = polygon[first][1] - polygon[second][1]
            if dx * dx + dy * dy <= EPS * EPS:
                fail(
                    E_REGION_INVALID,
                    "placement region has duplicate vertices",
                    field=f"region.vertices[{second}]",
                    vertex_index=second,
                )
    count = len(polygon)
    for first in range(count):
        a = polygon[first]
        b = polygon[(first + 1) % count]
        for second in range(first + 1, count):
            if second == first + 1 or (first == 0 and second == count - 1):
                continue
            c = polygon[second]
            d = polygon[(second + 1) % count]
            if _segments_intersect(a, b, c, d):
                fail(
                    E_REGION_INVALID,
                    "placement region polygon self-intersects",
                    field=f"region.vertices[{second}]",
                    vertex_index=second,
                    edges=[first, second],
                )
    if abs(_polygon_area(polygon)) <= EPS:
        fail(
            E_REGION_INVALID,
            "placement region has zero area",
            field="region.vertices",
            vertex_index=0,
        )

    min_col = max(
        0,
        int(math.floor((min(v[0] for v in polygon) - grid.origin_x) / grid.cell_size)),
    )
    max_col = min(
        grid.cols - 1,
        int(math.floor((max(v[0] for v in polygon) - grid.origin_x) / grid.cell_size)),
    )
    min_row = max(
        0,
        int(math.floor((min(v[1] for v in polygon) - grid.origin_y) / grid.cell_size)),
    )
    max_row = min(
        grid.rows - 1,
        int(math.floor((max(v[1] for v in polygon) - grid.origin_y) / grid.cell_size)),
    )
    covered: list[tuple[int, int]] = []
    for row in range(min_row, max_row + 1):
        for col in range(min_col, max_col + 1):
            x0 = grid.origin_x + col * grid.cell_size
            y0 = grid.origin_y + row * grid.cell_size
            x1 = x0 + grid.cell_size
            y1 = y0 + grid.cell_size
            square = ((x0, y0), (x1, y0), (x1, y1), (x0, y1))
            if _polygons_overlap(polygon, square):
                covered.append((row, col))
    if not covered:
        fail(
            E_REGION_INVALID,
            "placement region does not cover any grid cell",
            field="region.vertices",
            vertex_index=0,
        )
    return tuple(covered)


def check_placement_handle(
    handle: SnapshotHandle,
    vertices: Sequence[Sequence[float]],
    *,
    expected_version: int | None = None,
    expected_hash: str | None = None,
) -> PlacementResult:
    handle.ensure_active()
    snapshot = handle.snapshot
    if expected_version is not None and expected_version != snapshot.version:
        fail(
            E_VERSION_STALE,
            "placement request version is not the handle version",
            field="region.version",
            expected=expected_version,
            actual=snapshot.version,
        )
    if expected_hash is not None and expected_hash != snapshot.content_hash:
        fail(
            E_VERSION_STALE,
            "placement request hash is not the handle hash",
            field="region.content_hash",
            expected=expected_hash,
            actual=snapshot.content_hash,
        )
    polygon: list[Vec2] = []
    for index, value in enumerate(vertices):
        try:
            if len(value) != 2:
                raise ValueError
            polygon.append((float(value[0]), float(value[1])))
        except (TypeError, ValueError):
            fail(
                E_REGION_INVALID,
                "placement vertex must contain two numeric values",
                field=f"region.vertices[{index}]",
                vertex_index=index,
            )
    covered = _validate_region(polygon, snapshot)
    grid = snapshot.scene.grid

    unwalkable = [
        (row, col) for row, col in covered if not grid.is_walkable(row, col)
    ]
    if unwalkable:
        row, col = unwalkable[0]
        point = grid.cell_center(row, col)
        return PlacementResult(
            valid=False,
            failure_code=E_REGION_UNWALKABLE,
            message="placement region covers a non-walkable cell",
            failed_vertex=_nearest_vertex(polygon, point),
            conflict_entity=f"grid:{row},{col}",
            conflict_point=point,
            covered_cells=covered,
            version=snapshot.version,
            content_hash=snapshot.content_hash,
        )

    for unit in snapshot.scene.active_units():
        conflict_point: Vec2 | None = None
        if unit.bounds_kind == "capsule" and unit.capsule_a is not None and unit.capsule_b is not None:
            a = (unit.capsule_a[0], unit.capsule_a[1])
            b = (unit.capsule_b[0], unit.capsule_b[1])
            distance_sq = _segment_polygon_distance_sq(a, b, polygon)
            if distance_sq <= unit.radius * unit.radius + EPS:
                conflict_point = a if _point_in_polygon(a, polygon) else b
        else:
            if _polygon_rectangle_overlap(
                polygon,
                (unit.minimum[0], unit.minimum[1]),
                (unit.maximum[0], unit.maximum[1]),
            ):
                conflict_point = (
                    clamp(unit.minimum[0], min(v[0] for v in polygon), max(v[0] for v in polygon)),
                    clamp(unit.minimum[1], min(v[1] for v in polygon), max(v[1] for v in polygon)),
                )
        if conflict_point is not None:
            return PlacementResult(
                valid=False,
                failure_code=E_REGION_UNIT_OVERLAP,
                message="placement region overlaps a unit footprint",
                failed_vertex=_nearest_vertex(polygon, conflict_point),
                conflict_entity=f"unit:{unit.unit_id}",
                conflict_point=conflict_point,
                covered_cells=covered,
                version=snapshot.version,
                content_hash=snapshot.content_hash,
            )

    for cover in snapshot.scene.covers:
        a3, b3, c3 = snapshot.scene.triangle(cover)
        triangle = ((a3[0], a3[1]), (b3[0], b3[1]), (c3[0], c3[1]))
        if _polygons_overlap(polygon, triangle):
            conflict_point = triangle[0]
            return PlacementResult(
                valid=False,
                failure_code=E_REGION_COVER_OVERLAP,
                message="placement region intersects cover geometry",
                failed_vertex=_nearest_vertex(polygon, conflict_point),
                conflict_entity=f"cover:{cover.cover_id}",
                conflict_point=conflict_point,
                covered_cells=covered,
                version=snapshot.version,
                content_hash=snapshot.content_hash,
            )

    return PlacementResult(
        valid=True,
        failure_code=None,
        message="placement region is valid",
        failed_vertex=None,
        conflict_entity=None,
        conflict_point=None,
        covered_cells=covered,
        version=snapshot.version,
        content_hash=snapshot.content_hash,
    )


def check_placement(
    store: VersionStore,
    vertices: Sequence[Sequence[float]],
    *,
    expected_version: int | None = None,
    expected_hash: str | None = None,
) -> PlacementResult:
    handle = store.acquire(
        expected_version=expected_version,
        expected_hash=expected_hash,
    )
    try:
        return check_placement_handle(handle, vertices)
    finally:
        handle.release()
