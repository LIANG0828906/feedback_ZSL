from __future__ import annotations

import math
from collections.abc import Iterable, Sequence

from .model import (
    FillRule,
    FlattenedPath,
    FlattenedSubpath,
    PathSpec,
    Point,
    Segment,
    SegmentKind,
    Subpath,
)

EPSILON = 1.0e-15


def lerp(a: Point, b: Point, t: float) -> Point:
    return Point(a.x + (b.x - a.x) * t, a.y + (b.y - a.y) * t)


def bezier_point(segment: Segment, t: float) -> Point:
    t = min(1.0, max(0.0, float(t)))
    points = segment.points
    if segment.kind is SegmentKind.LINE:
        return lerp(points[0], points[1], t)
    if segment.kind is SegmentKind.QUADRATIC:
        u = 1.0 - t
        return Point(
            u * u * points[0].x + 2.0 * u * t * points[1].x + t * t * points[2].x,
            u * u * points[0].y + 2.0 * u * t * points[1].y + t * t * points[2].y,
        )
    u = 1.0 - t
    return Point(
        u * u * u * points[0].x
        + 3.0 * u * u * t * points[1].x
        + 3.0 * u * t * t * points[2].x
        + t * t * t * points[3].x,
        u * u * u * points[0].y
        + 3.0 * u * u * t * points[1].y
        + 3.0 * u * t * t * points[2].y
        + t * t * t * points[3].y,
    )


def bezier_tangent(segment: Segment, t: float) -> Point:
    points = segment.points
    if segment.kind is SegmentKind.LINE:
        return (points[1] - points[0]).normalized()
    if segment.kind is SegmentKind.QUADRATIC:
        value = (points[1] - points[0]) * (2.0 * (1.0 - t)) + (
            points[2] - points[1]
        ) * (2.0 * t)
        return value.normalized()
    u = 1.0 - t
    value = (
        (points[1] - points[0]) * (3.0 * u * u)
        + (points[2] - points[1]) * (6.0 * u * t)
        + (points[3] - points[2]) * (3.0 * t * t)
    )
    return value.normalized()


def point_segment_distance(point: Point, start: Point, end: Point) -> float:
    delta = end - start
    length_squared = delta.dot(delta)
    if length_squared <= EPSILON:
        return point.distance_to(start)
    t = min(1.0, max(0.0, (point - start).dot(delta) / length_squared))
    return point.distance_to(start + delta * t)


def closest_point_on_segment(
    point: Point, start: Point, end: Point
) -> tuple[Point, float, float]:
    delta = end - start
    length_squared = delta.dot(delta)
    if length_squared <= EPSILON:
        return start, 0.0, point.distance_to(start)
    t = min(1.0, max(0.0, (point - start).dot(delta) / length_squared))
    closest = start + delta * t
    return closest, t, point.distance_to(closest)


def flatten_segment(segment: Segment, tolerance: float) -> list[tuple[float, Point]]:
    if segment.kind is SegmentKind.LINE:
        return [(0.0, segment.points[0]), (1.0, segment.points[-1])]

    tolerance = max(float(tolerance), EPSILON)
    result: list[tuple[float, Point]] = []

    def emit(t: float, point: Point) -> None:
        if result and result[-1][1].distance_to(point) <= tolerance * 0.05:
            return
        result.append((t, point))

    def split(
        p0: Point,
        p1: Point,
        p2: Point,
        p3: Point | None,
        t0: float,
        t1: float,
        depth: int,
    ) -> None:
        start = p0
        end = p3 if p3 is not None else p2
        if p3 is None:
            flatness = point_segment_distance(p1, start, end)
        else:
            flatness = max(
                point_segment_distance(p1, start, end),
                point_segment_distance(p2, start, end),
            )

        if flatness <= tolerance or depth >= 26 or t1 - t0 <= 1.0e-14:
            emit(t1, end)
            return

        midpoint = (t0 + t1) * 0.5
        if p3 is None:
            left, right = split_quadratic(p0, p1, p2)
            split(left[0], left[1], left[2], None, t0, midpoint, depth + 1)
            split(right[0], right[1], right[2], None, midpoint, t1, depth + 1)
        else:
            left, right = split_cubic(p0, p1, p2, p3)
            split(left[0], left[1], left[2], left[3], t0, midpoint, depth + 1)
            split(right[0], right[1], right[2], right[3], midpoint, t1, depth + 1)

    result.append((0.0, segment.points[0]))
    if segment.kind is SegmentKind.QUADRATIC:
        split(
            segment.points[0],
            segment.points[1],
            segment.points[2],
            None,
            0.0,
            1.0,
            0,
        )
    else:
        split(
            segment.points[0],
            segment.points[1],
            segment.points[2],
            segment.points[3],
            0.0,
            1.0,
            0,
        )
    return result


def split_quadratic(
    p0: Point, p1: Point, p2: Point
) -> tuple[tuple[Point, Point, Point], tuple[Point, Point, Point]]:
    p01 = lerp(p0, p1, 0.5)
    p12 = lerp(p1, p2, 0.5)
    midpoint = lerp(p01, p12, 0.5)
    return (p0, p01, midpoint), (midpoint, p12, p2)


def split_cubic(
    p0: Point, p1: Point, p2: Point, p3: Point
) -> tuple[tuple[Point, Point, Point, Point], tuple[Point, Point, Point, Point]]:
    p01 = lerp(p0, p1, 0.5)
    p12 = lerp(p1, p2, 0.5)
    p23 = lerp(p2, p3, 0.5)
    p012 = lerp(p01, p12, 0.5)
    p123 = lerp(p12, p23, 0.5)
    midpoint = lerp(p012, p123, 0.5)
    return (p0, p01, p012, midpoint), (midpoint, p123, p23, p3)


def flatten_subpath(subpath: Subpath, tolerance: float) -> FlattenedSubpath:
    points: list[Point] = []
    for segment in subpath.segments:
        flattened = flatten_segment(segment, tolerance)
        if not points:
            points.append(flattened[0][1])
        for _, point in flattened[1:]:
            if points and points[-1].distance_to(point) <= max(tolerance * 0.02, 1e-14):
                continue
            points.append(point)
    if len(points) > 1 and points[0].distance_to(points[-1]) <= tolerance:
        points[-1] = points[0]
    elif subpath.closed and points:
        points.append(points[0])
    return FlattenedSubpath(points=points, source_index=subpath.source_index)


def flatten_path(path: PathSpec, tolerance: float) -> FlattenedPath:
    return FlattenedPath(
        subpaths=[flatten_subpath(subpath, tolerance) for subpath in path.subpaths],
        fill_rule=path.fill_rule,
    )


def contour_segments(points: Sequence[Point]) -> Iterable[tuple[Point, Point]]:
    if len(points) < 2:
        return
    for index in range(len(points) - 1):
        yield points[index], points[index + 1]


def signed_area(points: Sequence[Point]) -> float:
    if len(points) < 3:
        return 0.0
    total = 0.0
    previous = points[-1]
    for point in points:
        total += previous.x * point.y - point.x * previous.y
        previous = point
    return total * 0.5


def path_area(flattened: FlattenedPath) -> float:
    return sum(signed_area(subpath.points) for subpath in flattened.subpaths)


def ray_crossings(point: Point, points: Sequence[Point]) -> int:
    if len(points) < 2:
        return 0
    crossings = 0
    previous = points[-1]
    for current in points:
        if (previous.y > point.y) != (current.y > point.y):
            x = previous.x + (point.y - previous.y) * (
                (current.x - previous.x) / (current.y - previous.y)
            )
            if x > point.x:
                crossings += 1
        previous = current
    return crossings


def winding_number(point: Point, points: Sequence[Point]) -> int:
    if len(points) < 2:
        return 0
    winding = 0
    previous = points[-1]
    for current in points:
        cross = (current - previous).cross(point - previous)
        if previous.y <= point.y:
            if current.y > point.y and cross > 0.0:
                winding += 1
        elif current.y <= point.y and cross < 0.0:
            winding -= 1
        previous = current
    return winding


def point_in_flattened_path(point: Point, path: FlattenedPath) -> bool:
    if path.fill_rule is FillRule.EVENODD:
        parity = 0
        for subpath in path.subpaths:
            parity ^= ray_crossings(point, subpath.points) & 1
        return bool(parity)
    winding = sum(winding_number(point, subpath.points) for subpath in path.subpaths)
    return winding != 0


def point_in_contours(point: Point, contours: Sequence[Sequence[Point]]) -> bool:
    winding = sum(winding_number(point, points) for points in contours)
    return winding != 0


def is_near_collinear(
    previous: Point, current: Point, following: Point, tolerance: float
) -> bool:
    first = current - previous
    second = following - current
    if first.length() <= tolerance or second.length() <= tolerance:
        return True
    if first.dot(second) <= 0.0:
        return False
    return point_segment_distance(current, previous, following) <= tolerance


def deduplicate_points(points: Sequence[Point], tolerance: float) -> list[Point]:
    result: list[Point] = []
    for point in points:
        if not result or result[-1].distance_to(point) > tolerance:
            result.append(point)
        else:
            result[-1] = point
    if len(result) > 1 and result[0].distance_to(result[-1]) <= tolerance:
        result[0] = (result[0] + result[-1]) * 0.5
        result.pop()
    return result


def simplify_contour(points: Sequence[Point], tolerance: float) -> list[Point]:
    points = deduplicate_points(points, tolerance)
    if len(points) < 4:
        return points
    changed = True
    while changed and len(points) >= 4:
        changed = False
        result: list[Point] = []
        count = len(points)
        for index, point in enumerate(points):
            previous = points[(index - 1) % count]
            following = points[(index + 1) % count]
            if is_near_collinear(previous, point, following, tolerance):
                changed = True
                continue
            result.append(point)
        if len(result) < 3:
            break
        points = result
    return points


def sample_polyline(points: Sequence[Point], sample_tolerance: float) -> list[Point]:
    result: list[Point] = []
    for start, end in contour_segments(points):
        length = start.distance_to(end)
        steps = max(1, math.ceil(length / max(sample_tolerance, EPSILON)))
        for index in range(steps):
            result.append(lerp(start, end, index / steps))
    return result

