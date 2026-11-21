from __future__ import annotations

import math
from collections.abc import Iterable, Sequence

Point = tuple[float, float]
EPS = 1e-9


def point(value: Sequence[float]) -> Point:
    return (float(value[0]), float(value[1]))


def add(a: Point, b: Point) -> Point:
    return (a[0] + b[0], a[1] + b[1])


def sub(a: Point, b: Point) -> Point:
    return (a[0] - b[0], a[1] - b[1])


def mul(a: Point, scalar: float) -> Point:
    return (a[0] * scalar, a[1] * scalar)


def dot(a: Point, b: Point) -> float:
    return a[0] * b[0] + a[1] * b[1]


def cross(a: Point, b: Point) -> float:
    return a[0] * b[1] - a[1] * b[0]


def length(a: Point) -> float:
    return math.hypot(a[0], a[1])


def distance(a: Point, b: Point) -> float:
    return math.hypot(a[0] - b[0], a[1] - b[1])


def normalize(a: Point) -> Point:
    size = length(a)
    if size <= EPS:
        return (1.0, 0.0)
    return (a[0] / size, a[1] / size)


def lerp(a: Point, b: Point, t: float) -> Point:
    return (a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t)


def quadratic_point(p0: Point, control: Point, p1: Point, t: float) -> Point:
    one = 1.0 - t
    return (
        one * one * p0[0] + 2.0 * one * t * control[0] + t * t * p1[0],
        one * one * p0[1] + 2.0 * one * t * control[1] + t * t * p1[1],
    )


def polyline_length(points: Sequence[Point]) -> float:
    return sum(distance(points[index - 1], points[index]) for index in range(1, len(points)))


def cumulative_lengths(points: Sequence[Point]) -> list[float]:
    result = [0.0]
    for index in range(1, len(points)):
        result.append(result[-1] + distance(points[index - 1], points[index]))
    return result


def bbox(points: Sequence[Point]) -> tuple[float, float, float, float]:
    xs = [item[0] for item in points]
    ys = [item[1] for item in points]
    return (min(xs), min(ys), max(xs), max(ys))


def bbox_expand(box: Sequence[float], amount: float) -> tuple[float, float, float, float]:
    return (
        float(box[0]) - amount,
        float(box[1]) - amount,
        float(box[2]) + amount,
        float(box[3]) + amount,
    )


def bbox_intersects(a: Sequence[float], b: Sequence[float]) -> bool:
    return not (
        a[2] < b[0] or b[2] < a[0] or a[3] < b[1] or b[3] < a[1]
    )


def signed_area(polygon: Sequence[Point]) -> float:
    total = 0.0
    for index, current in enumerate(polygon):
        following = polygon[(index + 1) % len(polygon)]
        total += current[0] * following[1] - following[0] * current[1]
    return total * 0.5


def point_in_polygon(target: Point, polygon: Sequence[Point]) -> bool:
    inside = False
    x, y = target
    count = len(polygon)
    for index in range(count):
        a = polygon[index]
        b = polygon[(index + 1) % count]
        if point_on_segment(target, a, b, 1e-8):
            return True
        intersects = (a[1] > y) != (b[1] > y)
        if intersects:
            x_intersection = (b[0] - a[0]) * (y - a[1]) / (b[1] - a[1]) + a[0]
            if x_intersection > x:
                inside = not inside
    return inside


def point_strictly_in_polygon(target: Point, polygon: Sequence[Point]) -> bool:
    if not point_in_polygon(target, polygon):
        return False
    return not any(
        point_on_segment(target, polygon[index], polygon[(index + 1) % len(polygon)], 1e-7)
        for index in range(len(polygon))
    )


def point_on_segment(target: Point, a: Point, b: Point, tolerance: float = 1e-7) -> bool:
    ab = sub(b, a)
    ap = sub(target, a)
    if abs(cross(ab, ap)) > tolerance * max(1.0, length(ab)):
        return False
    projection = dot(ap, ab)
    if projection < -tolerance:
        return False
    return projection <= dot(ab, ab) + tolerance


def orientation(a: Point, b: Point, c: Point) -> float:
    return cross(sub(b, a), sub(c, a))


def segments_properly_intersect(a: Point, b: Point, c: Point, d: Point) -> bool:
    o1 = orientation(a, b, c)
    o2 = orientation(a, b, d)
    o3 = orientation(c, d, a)
    o4 = orientation(c, d, b)
    return (o1 > EPS and o2 < -EPS or o1 < -EPS and o2 > EPS) and (
        o3 > EPS and o4 < -EPS or o3 < -EPS and o4 > EPS
    )


def polygons_overlap(a: Sequence[Point], b: Sequence[Point]) -> bool:
    if abs(signed_area(a)) <= 1e-7 or abs(signed_area(b)) <= 1e-7:
        return False
    for index in range(len(a)):
        a0, a1 = a[index], a[(index + 1) % len(a)]
        for other in range(len(b)):
            b0, b1 = b[other], b[(other + 1) % len(b)]
            if segments_properly_intersect(a0, a1, b0, b1):
                return True
    for vertex in a:
        if point_strictly_in_polygon(vertex, b):
            return True
    for vertex in b:
        if point_strictly_in_polygon(vertex, a):
            return True
    return False


def point_segment_distance(target: Point, a: Point, b: Point) -> float:
    ab = sub(b, a)
    denominator = dot(ab, ab)
    if denominator <= EPS:
        return distance(target, a)
    t = max(0.0, min(1.0, dot(sub(target, a), ab) / denominator))
    return distance(target, add(a, mul(ab, t)))


def segment_segment_distance(a: Point, b: Point, c: Point, d: Point) -> float:
    if segments_properly_intersect(a, b, c, d):
        return 0.0
    return min(
        point_segment_distance(a, c, d),
        point_segment_distance(b, c, d),
        point_segment_distance(c, a, b),
        point_segment_distance(d, a, b),
    )


def line_polygon_intervals(
    origin: Point,
    direction: Point,
    polygon: Sequence[Point],
) -> list[tuple[float, float]]:
    hits: list[float] = []
    count = len(polygon)
    for index in range(count):
        a = polygon[index]
        b = polygon[(index + 1) % count]
        edge = sub(b, a)
        denominator = cross(direction, edge)
        if abs(denominator) <= EPS:
            continue
        delta = sub(a, origin)
        t = cross(delta, edge) / denominator
        u = cross(delta, direction) / denominator
        if -1e-8 <= u <= 1.0 + 1e-8:
            hits.append(t)
    if len(hits) < 2:
        return []
    hits.sort()
    merged: list[tuple[float, float]] = []
    for index in range(0, len(hits) - 1, 2):
        start, end = hits[index], hits[index + 1]
        if end - start <= 1e-7:
            continue
        midpoint = lerp(origin, add(origin, direction), (start + end) * 0.5)
        # The midpoint test removes tangent duplicate hits.
        if not point_in_polygon(midpoint, polygon):
            continue
        if merged and start <= merged[-1][1] + 1e-7:
            merged[-1] = (merged[-1][0], max(merged[-1][1], end))
        else:
            merged.append((start, end))
    return merged


def polyline_to_dict(points: Iterable[Point]) -> list[list[float]]:
    return [[value[0], value[1]] for value in points]


def rotate(vector: Point, radians: float) -> Point:
    cosine = math.cos(radians)
    sine = math.sin(radians)
    return (
        vector[0] * cosine - vector[1] * sine,
        vector[0] * sine + vector[1] * cosine,
    )
