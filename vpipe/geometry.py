from __future__ import annotations

import math
from collections import defaultdict
from dataclasses import dataclass
from typing import Callable, Iterable, Sequence

from .model import Contour, EdgeTrace, PathData, Point, Segment


DEFAULT_FLATTEN_RATIO = 0.25
MIN_FLATTEN_ERROR = 1e-10


@dataclass(frozen=True, slots=True)
class FlattenVertex:
    point: Point
    t: float


@dataclass(frozen=True, slots=True)
class FlatEdge:
    a: Point
    b: Point
    t_start: float
    t_end: float
    trace: EdgeTrace

    @property
    def midpoint(self) -> Point:
        return self.a.lerp(self.b, 0.5)

    @property
    def length(self) -> float:
        return self.a.distance_to(self.b)

    def direction(self) -> Point:
        return (self.b - self.a).normalized()

    def left_normal(self) -> Point:
        direction = self.direction()
        return Point(-direction.y, direction.x)

    def reversed(self) -> "FlatEdge":
        return FlatEdge(
            self.b,
            self.a,
            self.t_end,
            self.t_start,
            self.trace,
        )


def segment_tangent(segment: Segment, t: float) -> Point:
    tangent = segment.tangent_at(t)
    if tangent.norm() > 1e-12:
        return tangent
    for fraction in (0.25, 0.5, 0.75, 0.1, 0.9):
        nearby = segment.tangent_at(fraction)
        if nearby.norm() > 1e-12:
            return nearby
    return Point(1.0, 0.0)


def _flatness(segment: Segment, t0: float, t1: float, start: Point, end: Point) -> float:
    if segment.kind == "line":
        return 0.0
    chord = end - start
    chord_len = chord.norm()
    if chord_len <= 1e-14:
        controls = [
            p
            for p in (segment.control1, segment.control2)
            if p is not None
        ]
        if not controls:
            return 0.0
        return max(p.distance_to(start) for p in controls)
    normal = Point(-chord.y / chord_len, chord.x / chord_len)
    samples = [segment.point_at(t0 + (t1 - t0) * f) for f in (0.25, 0.5, 0.75)]
    return max(abs((sample - start).dot(normal)) for sample in samples)


def flatten_segment(
    segment: Segment,
    max_error: float,
    max_depth: int = 20,
) -> list[FlattenVertex]:
    """Adaptive subdivision with parameter values retained for intersection."""

    if max_error <= 0.0 or not math.isfinite(max_error):
        max_error = MIN_FLATTEN_ERROR
    result = [FlattenVertex(segment.start, 0.0)]

    def recurse(t0: float, t1: float, p0: Point, p1: Point, depth: int) -> None:
        if depth >= max_depth:
            result.append(FlattenVertex(p1, t1))
            return
        error = _flatness(segment, t0, t1, p0, p1)
        if error <= max_error:
            result.append(FlattenVertex(p1, t1))
            return
        tm = (t0 + t1) * 0.5
        pm = segment.point_at(tm)
        recurse(t0, tm, p0, pm, depth + 1)
        recurse(tm, t1, pm, p1, depth + 1)

    recurse(0.0, 1.0, segment.start, segment.end, 0)
    return result


def flatten_contour(
    contour: Contour,
    contour_index: int,
    operand: str,
    max_error: float,
    min_edge_length: float = 1e-12,
) -> list[FlatEdge]:
    edges: list[FlatEdge] = []
    for segment_index, segment in enumerate(contour.segments):
        vertices = flatten_segment(segment, max_error)
        for subsegment_index, (left, right) in enumerate(zip(vertices, vertices[1:])):
            if left.point.distance_to(right.point) <= min_edge_length:
                continue
            source_trace = (
                segment.trace
                if segment.trace
                else (
                    EdgeTrace(
                        operand=operand,
                        contour_index=contour_index,
                        contour_id=contour.contour_id,
                        segment_index=segment_index,
                        subsegment_index=subsegment_index,
                        t_start=left.t,
                        t_end=right.t,
                        source_kind="input",
                    ),
                )
            )
            edges.append(
                FlatEdge(
                    left.point,
                    right.point,
                    left.t,
                    right.t,
                    source_trace[0],
                )
            )
    return edges


def flatten_path(
    path: PathData,
    max_error: float,
    operand: str = "input",
    min_edge_length: float = 1e-12,
) -> list[FlatEdge]:
    edges: list[FlatEdge] = []
    for contour_index, contour in enumerate(path.contours):
        edges.extend(
            flatten_contour(
                contour,
                contour_index,
                operand,
                max_error,
                min_edge_length,
            )
        )
    return edges


def contour_polyline(
    contour: Contour,
    max_error: float,
) -> list[Point]:
    edges = flatten_contour(contour, 0, "input", max_error)
    if not edges:
        return []
    points = [edges[0].a]
    points.extend(edge.b for edge in edges)
    if points and points[-1].distance_to(points[0]) <= 1e-9:
        points[-1] = points[0]
    return points


def polygon_signed_area(points: Sequence[Point]) -> float:
    if len(points) < 3:
        return 0.0
    total = 0.0
    for a, b in zip(points, points[1:]):
        total += a.cross(b)
    if points[0].distance_to(points[-1]) > 1e-12:
        total += points[-1].cross(points[0])
    return total * 0.5


def contour_signed_area(contour: Contour, max_error: float = 1e-7) -> float:
    return polygon_signed_area(contour_polyline(contour, max_error))


def actual_direction(contour: Contour, max_error: float = 1e-7) -> str:
    return "ccw" if contour_signed_area(contour, max_error) >= 0.0 else "cw"


def point_on_segment(point: Point, a: Point, b: Point, tolerance: float) -> bool:
    ab = b - a
    ap = point - a
    length_sq = ab.dot(ab)
    if length_sq <= 1e-24:
        return point.distance_to(a) <= tolerance
    cross = abs(ab.cross(ap))
    scale = max(1.0, math.sqrt(length_sq))
    if cross > tolerance * scale:
        return False
    projection = ap.dot(ab)
    return -tolerance * scale <= projection <= length_sq + tolerance * scale


def crossing_number(polyline: Sequence[Point], point: Point) -> int:
    if len(polyline) < 2:
        return 0
    inside = False
    for a, b in zip(polyline, polyline[1:]):
        if (a.y > point.y) != (b.y > point.y):
            crossing_x = a.x + (point.y - a.y) * (b.x - a.x) / (b.y - a.y)
            if crossing_x > point.x:
                inside = not inside
    return 1 if inside else 0


def winding_number_edges(
    edges: Sequence[FlatEdge],
    point: Point,
    fill_rule: str,
    boundary_tolerance: float,
) -> tuple[bool, bool]:
    """Return (inside, on_boundary)."""

    winding = 0
    crossings = 0
    for edge in edges:
        if point_on_segment(point, edge.a, edge.b, boundary_tolerance):
            return True, True
        if edge.a.y <= point.y < edge.b.y:
            if (edge.b - edge.a).cross(point - edge.a) > 0:
                winding += 1
                crossings += 1
        elif edge.b.y <= point.y < edge.a.y:
            if (edge.b - edge.a).cross(point - edge.a) < 0:
                winding -= 1
                crossings += 1
    if fill_rule == "evenodd":
        return crossings % 2 == 1, False
    return winding != 0, False


@dataclass(frozen=True, slots=True)
class PreparedPath:
    path: PathData
    edges: tuple[FlatEdge, ...]
    tolerance: float
    flatten_error: float
    y_buckets: dict[int, tuple[FlatEdge, ...]]
    y_cell_size: float

    def membership(self, point: Point, boundary_tolerance: float | None = None) -> tuple[bool, bool]:
        tol = self.tolerance if boundary_tolerance is None else boundary_tolerance
        bucket = math.floor(point.y / self.y_cell_size)
        candidates = self.y_buckets.get(bucket, ())
        return winding_number_edges(
            candidates,
            point,
            self.path.fill_rule,
            tol,
        )

    def contains(self, point: Point, boundary_tolerance: float | None = None) -> bool:
        inside, _ = self.membership(point, boundary_tolerance)
        return inside


def prepare_path(path: PathData, tolerance: float | None = None) -> PreparedPath:
    tol = path.tolerance if tolerance is None else tolerance
    flatten_error = max(MIN_FLATTEN_ERROR, tol * DEFAULT_FLATTEN_RATIO)
    edges = tuple(flatten_path(path, flatten_error, "input", min_edge_length=1e-12))
    bounds = path.bounds()
    y_cell_size = max(
        tol * 4.0,
        bounds.height / 256.0 if bounds.height > 0 else 1.0,
        1e-9,
    )
    buckets: dict[int, list[FlatEdge]] = defaultdict(list)
    for edge in edges:
        low = math.floor(min(edge.a.y, edge.b.y) / y_cell_size)
        high = math.floor(max(edge.a.y, edge.b.y) / y_cell_size)
        for bucket in range(low, high + 1):
            buckets[bucket].append(edge)
    frozen_buckets = {
        bucket: tuple(bucket_edges) for bucket, bucket_edges in buckets.items()
    }
    return PreparedPath(
        path,
        edges,
        tol,
        flatten_error,
        frozen_buckets,
        y_cell_size,
    )


def closest_point_on_segment(
    point: Point,
    a: Point,
    b: Point,
) -> tuple[Point, float, float]:
    ab = b - a
    length_sq = ab.dot(ab)
    if length_sq <= 1e-24:
        return a, 0.0, point.distance_to(a)
    t = max(0.0, min(1.0, (point - a).dot(ab) / length_sq))
    closest = a.lerp(b, t)
    return closest, t, point.distance_to(closest)


def closest_points_between_segments(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
) -> tuple[float, float, float]:
    """Return (t_a, t_b, distance) for the closest pair."""

    candidates: list[tuple[float, float, float]] = []
    for t in (0.0, 1.0):
        p = a0.lerp(a1, t)
        _, u, dist = closest_point_on_segment(p, b0, b1)
        candidates.append((t, u, dist))
    for u in (0.0, 1.0):
        p = b0.lerp(b1, u)
        _, t, dist = closest_point_on_segment(p, a0, a1)
        candidates.append((t, u, dist))
    d = (a1 - a0).cross(b1 - b0)
    if abs(d) > 1e-15:
        t = (b0 - a0).cross(b1 - b0) / d
        u = (b0 - a0).cross(a1 - a0) / d
        if 0.0 <= t <= 1.0 and 0.0 <= u <= 1.0:
            pa = a0.lerp(a1, t)
            pb = b0.lerp(b1, u)
            candidates.append((t, u, pa.distance_to(pb)))
    return min(candidates, key=lambda item: item[2])


def point_to_polyline_distance(point: Point, polyline: Sequence[Point]) -> float:
    if len(polyline) < 2:
        return math.inf
    return min(
        closest_point_on_segment(point, a, b)[2]
        for a, b in zip(polyline, polyline[1:])
    )


def polyline_edges(polyline: Sequence[Point]) -> list[FlatEdge]:
    if len(polyline) < 2:
        return []
    edges: list[FlatEdge] = []
    for index, (a, b) in enumerate(zip(polyline, polyline[1:])):
        trace = EdgeTrace("truth", 0, "truth", index, 0, 0.0, 1.0, "truth")
        edges.append(FlatEdge(a, b, 0.0, 1.0, trace))
    return edges


def contour_interior_point(contour: Contour, max_error: float = 1e-6) -> Point:
    polyline = contour_polyline(contour, max_error)
    if len(polyline) < 3:
        return polyline[0] if polyline else Point(0.0, 0.0)
    area = polygon_signed_area(polyline)
    diagonal = math.hypot(
        max(p.x for p in polyline) - min(p.x for p in polyline),
        max(p.y for p in polyline) - min(p.y for p in polyline),
    )
    candidates: list[Point] = []
    avg = Point(
        sum(p.x for p in polyline[:-1]) / max(1, len(polyline) - 1),
        sum(p.y for p in polyline[:-1]) / max(1, len(polyline) - 1),
    )
    candidates.append(avg)
    for a, b in zip(polyline, polyline[1:]):
        direction = (b - a).normalized()
        normal = Point(-direction.y, direction.x)
        if area < 0:
            normal = Point(-normal.x, -normal.y)
        candidates.append(a.lerp(b, 0.5) + normal * max(diagonal * 1e-4, 1e-7))
    for candidate in candidates:
        crossings = crossing_number(polyline, candidate)
        if crossings == 1 and not any(
            point_on_segment(candidate, a, b, 1e-10)
            for a, b in zip(polyline, polyline[1:])
        ):
            return candidate
    return candidates[-1]


def contour_depth(
    target: Contour,
    all_contours: Sequence[Contour],
    max_error: float,
) -> int:
    target_polyline = contour_polyline(target, max_error)
    if len(target_polyline) < 3:
        return 0
    samples: list[Point] = []
    for a, b in zip(target_polyline, target_polyline[1:]):
        samples.append(a)
        samples.append(a.lerp(b, 0.5))
    if samples:
        stride = max(1, len(samples) // 24)
        samples = samples[::stride]
    depth = 0
    for contour in all_contours:
        if contour is target:
            continue
        polyline = contour_polyline(contour, max_error)
        if len(polyline) < 3:
            continue
        tested = 0
        inside = 0
        for candidate in samples:
            if any(
                point_on_segment(candidate, a, b, max_error)
                for a, b in zip(polyline, polyline[1:])
            ):
                continue
            tested += 1
            if crossing_number(polyline, candidate) == 1:
                inside += 1
        if tested > 0 and inside == tested:
            depth += 1
        elif tested > 0 and inside > 0:
            # A valid v1 nesting tree must be all-inside or all-outside.
            # Ambiguous overlap is left to the caller's closure validation.
            continue
    return depth


def run_with_cancel_check(
    callback: Callable[[], None] | None,
    index: int,
    interval: int = 32,
) -> None:
    if callback is not None and index % interval == 0:
        callback()


def iter_sample_distances(
    source: Sequence[Point],
    target: Sequence[Point],
    sample_step: float = 0.5,
) -> Iterable[float]:
    _ = sample_step
    if len(target) < 2:
        return
    for point in source:
        yield point_to_polyline_distance(point, target)

