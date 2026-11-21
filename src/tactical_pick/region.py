"""Draft placement polygon validation against one published scene version."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any, Iterable, Sequence

from .errors import ToolError, fail
from .math3d import EPS
from .snapshot import SceneManager, SceneSnapshot, SnapshotHandle
from .util import canonical_json, sha256_hex

Vec2 = tuple[float, float]


@dataclass(frozen=True)
class PlacementRequest:
    polygon: tuple[Vec2, ...]
    requested_version: int | None = None
    height: float = 2.0


@dataclass(frozen=True)
class PlacementVerdict:
    valid: bool
    reason: str
    failure_vertex: int | None
    failure_point: Vec2 | None
    conflict_entity: str | None
    scene_version: int
    scene_hash: str
    decision_hash: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "valid": self.valid,
            "reason": self.reason,
            "failure_vertex": self.failure_vertex,
            "failure_point": None if self.failure_point is None else list(self.failure_point),
            "conflict_entity": self.conflict_entity,
            "scene_version": self.scene_version,
            "scene_hash": self.scene_hash,
            "decision_hash": self.decision_hash,
        }


def _orientation(a: Vec2, b: Vec2, c: Vec2) -> float:
    return (b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0])


def _on_segment(a: Vec2, b: Vec2, p: Vec2) -> bool:
    return (
        min(a[0], b[0]) - EPS <= p[0] <= max(a[0], b[0]) + EPS
        and min(a[1], b[1]) - EPS <= p[1] <= max(a[1], b[1]) + EPS
        and abs(_orientation(a, b, p)) <= EPS
    )


def _segments_intersect(a: Vec2, b: Vec2, c: Vec2, d: Vec2) -> bool:
    o1 = _orientation(a, b, c)
    o2 = _orientation(a, b, d)
    o3 = _orientation(c, d, a)
    o4 = _orientation(c, d, b)
    if ((o1 > EPS and o2 < -EPS) or (o1 < -EPS and o2 > EPS)) and (
        (o3 > EPS and o4 < -EPS) or (o3 < -EPS and o4 > EPS)
    ):
        return True
    if abs(o1) <= EPS and _on_segment(a, b, c):
        return True
    if abs(o2) <= EPS and _on_segment(a, b, d):
        return True
    if abs(o3) <= EPS and _on_segment(c, d, a):
        return True
    if abs(o4) <= EPS and _on_segment(c, d, b):
        return True
    return False


def _point_in_polygon(point: Vec2, polygon: Sequence[Vec2]) -> bool:
    inside = False
    x, y = point
    count = len(polygon)
    for index in range(count):
        a = polygon[index]
        b = polygon[(index + 1) % count]
        if _on_segment(a, b, point):
            return True
        if (a[1] > y) != (b[1] > y):
            x_cross = (b[0] - a[0]) * (y - a[1]) / (b[1] - a[1]) + a[0]
            if x_cross > x:
                inside = not inside
    return inside


def _polygon_area(polygon: Sequence[Vec2]) -> float:
    total = 0.0
    for index, point in enumerate(polygon):
        next_point = polygon[(index + 1) % len(polygon)]
        total += point[0] * next_point[1] - next_point[0] * point[1]
    return total * 0.5


def _point_segment_distance_2d(point: Vec2, a: Vec2, b: Vec2) -> float:
    ab = (b[0] - a[0], b[1] - a[1])
    denom = ab[0] * ab[0] + ab[1] * ab[1]
    if denom <= EPS:
        return math.hypot(point[0] - a[0], point[1] - a[1])
    t = max(0.0, min(1.0, ((point[0] - a[0]) * ab[0] + (point[1] - a[1]) * ab[1]) / denom))
    nearest = (a[0] + ab[0] * t, a[1] + ab[1] * t)
    return math.hypot(point[0] - nearest[0], point[1] - nearest[1])


def _segment_segment_distance_2d(a: Vec2, b: Vec2, c: Vec2, d: Vec2) -> float:
    if _segments_intersect(a, b, c, d):
        return 0.0
    return min(
        _point_segment_distance_2d(a, c, d),
        _point_segment_distance_2d(b, c, d),
        _point_segment_distance_2d(c, a, b),
        _point_segment_distance_2d(d, a, b),
    )


def _polygon_edges(polygon: Sequence[Vec2]) -> Iterable[tuple[Vec2, Vec2]]:
    for index in range(len(polygon)):
        yield polygon[index], polygon[(index + 1) % len(polygon)]


def _polygon_intersects_rect(polygon: Sequence[Vec2], lower: Vec2, upper: Vec2) -> bool:
    corners = (lower, (upper[0], lower[1]), upper, (lower[0], upper[1]))
    if any(lower[0] - EPS <= point[0] <= upper[0] + EPS and lower[1] - EPS <= point[1] <= upper[1] + EPS for point in polygon):
        return True
    if any(_point_in_polygon(corner, polygon) for corner in corners):
        return True
    rect_edges = list(_polygon_edges(corners))
    return any(_segments_intersect(a, b, c, d) for a, b in _polygon_edges(polygon) for c, d in rect_edges)


def _polygon_intersects_triangle(polygon: Sequence[Vec2], triangle: Sequence[Vec2]) -> bool:
    if any(_point_in_polygon(point, polygon) for point in triangle):
        return True
    if any(_point_in_polygon(point, triangle) for point in polygon):
        return True
    return any(
        _segments_intersect(a, b, c, d)
        for a, b in _polygon_edges(polygon)
        for c, d in _polygon_edges(triangle)
    )


def _failure_vertex_for_area(polygon: Sequence[Vec2], predicate: Any) -> int:
    for index, point in enumerate(polygon):
        if predicate(point):
            return index
    return 0


def _first_crossing_vertex(polygon: Sequence[Vec2], target: Sequence[Vec2]) -> int:
    for index in range(len(polygon)):
        a = polygon[index]
        b = polygon[(index + 1) % len(polygon)]
        for c, d in _polygon_edges(target):
            if _segments_intersect(a, b, c, d):
                return index
    return 0


def validate_polygon(scene: SceneSnapshot, request: PlacementRequest) -> tuple[Vec2, ...]:
    polygon = tuple((float(point[0]), float(point[1])) for point in request.polygon)
    if len(polygon) < 3:
        fail(
            "E_REGION_INVALID",
            "placement polygon needs at least three vertices",
            "region.vertices",
            first_conflict={"vertex_count": len(polygon)},
        )
    for index, point in enumerate(polygon):
        if not math.isfinite(point[0]) or not math.isfinite(point[1]):
            fail(
                "E_REGION_INVALID",
                "placement vertex is not finite",
                f"region.vertices[{index}]",
                first_conflict={"vertex_index": index},
            )
    for index in range(len(polygon)):
        if polygon[index] == polygon[(index + 1) % len(polygon)]:
            fail(
                "E_REGION_INVALID",
                "placement polygon has a zero-length edge",
                f"region.vertices[{(index + 1) % len(polygon)}]",
                first_conflict={"edge_index": index},
            )
    count = len(polygon)
    for first in range(count):
        for second in range(first + 1, count):
            if second == first + 1 or (first == 0 and second == count - 1):
                continue
            if _segments_intersect(
                polygon[first],
                polygon[(first + 1) % count],
                polygon[second],
                polygon[(second + 1) % count],
            ):
                fail(
                    "E_REGION_INVALID",
                    "placement polygon self-intersects",
                    f"region.vertices[{second}]",
                    first_conflict={"first_edge": first, "second_edge": second, "vertex_index": second},
                )

    if abs(_polygon_area(polygon)) <= EPS:
        fail("E_REGION_INVALID", "placement polygon has zero area", "region.area")

    world_width = scene.scene.world_width
    world_height = scene.scene.world_height
    for index, point in enumerate(polygon):
        if (
            point[0] < -EPS
            or point[1] < -EPS
            or point[0] > world_width + EPS
            or point[1] > world_height + EPS
        ):
            fail(
                "E_REGION_INVALID",
                "placement vertex is outside the battlefield grid",
                f"region.vertices[{index}]",
                first_conflict={
                    "vertex_index": index,
                    "point": list(point),
                    "bounds": [[0.0, 0.0], [world_width, world_height]],
                },
            )
    if not math.isfinite(request.height) or request.height <= 0.0:
        fail("E_REGION_INVALID", "placement height must be positive and finite", "region.height")
    return polygon


def _nearest_vertex(polygon: Sequence[Vec2], point: Vec2) -> int:
    return min(
        range(len(polygon)),
        key=lambda index: (
            (polygon[index][0] - point[0]) ** 2 + (polygon[index][1] - point[1]) ** 2,
            index,
        ),
    )


def _walkability_failure(scene: SceneSnapshot, polygon: Sequence[Vec2]) -> tuple[int, Vec2, str] | None:
    cell = scene.scene.cell_size
    min_x = min(point[0] for point in polygon)
    max_x = max(point[0] for point in polygon)
    min_z = min(point[1] for point in polygon)
    max_z = max(point[1] for point in polygon)
    x0 = max(0, int(math.floor(min_x / cell)))
    x1 = min(scene.scene.grid_width - 1, int(math.floor(max_x / cell)))
    z0 = max(0, int(math.floor(min_z / cell)))
    z1 = min(scene.scene.grid_height - 1, int(math.floor(max_z / cell)))
    for z in range(z0, z1 + 1):
        for x in range(x0, x1 + 1):
            lower = (x * cell, z * cell)
            upper = ((x + 1) * cell, (z + 1) * cell)
            if not _polygon_intersects_rect(polygon, lower, upper):
                continue
            if scene.scene.is_walkable(x, z):
                continue
            center = ((lower[0] + upper[0]) * 0.5, (lower[1] + upper[1]) * 0.5)
            vertex = _nearest_vertex(polygon, center)
            return vertex, polygon[vertex], f"grid:{x},{z}"
    return None


def _unit_footprint_intersection(scene: SceneSnapshot, polygon: Sequence[Vec2]) -> tuple[int, Vec2, str] | None:
    for unit in scene.scene.units:
        if unit.kind == "aabb":
            lower = (unit.aabb_min[0], unit.aabb_min[2])
            upper = (unit.aabb_max[0], unit.aabb_max[2])
            if _polygon_intersects_rect(polygon, lower, upper):
                vertex = _failure_vertex_for_area(
                    polygon,
                    lambda point: lower[0] - EPS <= point[0] <= upper[0] + EPS
                    and lower[1] - EPS <= point[1] <= upper[1] + EPS,
                )
                return vertex, polygon[vertex], f"unit:{unit.unit_id}"
        else:
            a = (unit.capsule_a[0], unit.capsule_a[2])
            b = (unit.capsule_b[0], unit.capsule_b[2])
            radius = unit.radius
            intersects = (
                _point_in_polygon(a, polygon)
                or _point_in_polygon(b, polygon)
                or any(_point_segment_distance_2d(point, a, b) <= radius + EPS for point in polygon)
                or any(_segment_segment_distance_2d(a, b, c, d) <= radius + EPS for c, d in _polygon_edges(polygon))
            )
            if intersects:
                center = ((a[0] + b[0]) * 0.5, (a[1] + b[1]) * 0.5)
                vertex = _nearest_vertex(polygon, center)
                return vertex, polygon[vertex], f"unit:{unit.unit_id}"
    return None


def _cover_intersection(scene: SceneSnapshot, polygon: Sequence[Vec2], height: float) -> tuple[int, Vec2, str] | None:
    ground = scene.scene.ground_y
    for triangle in scene.scene.cover_triangles:
        y_values = (triangle.a[1], triangle.b[1], triangle.c[1])
        if max(y_values) < ground - EPS or min(y_values) > ground + height + EPS:
            continue
        projected = (
            (triangle.a[0], triangle.a[2]),
            (triangle.b[0], triangle.b[2]),
            (triangle.c[0], triangle.c[2]),
        )
        if _polygon_intersects_triangle(polygon, projected):
            vertex = _failure_vertex_for_area(polygon, lambda point: _point_in_polygon(point, projected))
            if vertex == 0 and not _point_in_polygon(polygon[0], projected):
                vertex = _first_crossing_vertex(polygon, projected)
            return vertex, polygon[vertex], f"cover:{triangle.entity_id}"
    return None


def _decision_hash(
    polygon: Sequence[Vec2],
    height: float,
    scene_version: int,
    scene_hash: str,
    valid: bool,
    reason: str,
    failure_vertex: int | None,
    conflict_entity: str | None,
) -> str:
    payload = {
        "polygon": [list(point) for point in polygon],
        "height": height,
        "scene_version": scene_version,
        "scene_hash": scene_hash,
        "valid": valid,
        "reason": reason,
        "failure_vertex": failure_vertex,
        "conflict_entity": conflict_entity,
    }
    return sha256_hex(canonical_json(payload).encode("utf-8"))


def judge_snapshot(snapshot: SceneSnapshot, request: PlacementRequest) -> PlacementVerdict:
    polygon = validate_polygon(snapshot, request)
    failure = _walkability_failure(snapshot, polygon)
    reason = "IMPASSABLE_CELL"
    if failure is None:
        failure = _unit_footprint_intersection(snapshot, polygon)
        reason = "UNIT_OVERLAP"
    if failure is None:
        failure = _cover_intersection(snapshot, polygon, request.height)
        reason = "COVER_CLIP"

    if failure is None:
        valid = True
        reason = "OK"
        failure_vertex = None
        failure_point = None
        conflict_entity = None
    else:
        valid = False
        failure_vertex, failure_point, conflict_entity = failure

    decision_hash = _decision_hash(
        polygon,
        request.height,
        snapshot.version,
        snapshot.content_hash,
        valid,
        reason,
        failure_vertex,
        conflict_entity,
    )
    return PlacementVerdict(
        valid=valid,
        reason=reason,
        failure_vertex=failure_vertex,
        failure_point=failure_point,
        conflict_entity=conflict_entity,
        scene_version=snapshot.version,
        scene_hash=snapshot.content_hash,
        decision_hash=decision_hash,
    )


class RegionJudge:
    def __init__(self, manager: SceneManager) -> None:
        self.manager = manager

    def judge(self, request: PlacementRequest, handle: SnapshotHandle | None = None) -> PlacementVerdict:
        owned = handle is None
        active = self.manager.acquire(request.requested_version) if handle is None else handle
        try:
            snapshot = active.snapshot()
            if request.requested_version is not None and snapshot.version != request.requested_version:
                fail(
                    "E_VERSION_STALE",
                    "placement request and snapshot handle refer to different versions",
                    "snapshot.requested_version",
                    first_conflict={
                        "requested_version": request.requested_version,
                        "handle_version": snapshot.version,
                    },
                )
            return judge_snapshot(snapshot, request)
        finally:
            if owned:
                active.release()

