"""Deterministic geometry helpers shared by parsing and rasterization."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Iterable, Sequence


@dataclass(frozen=True, order=True)
class Point:
    x: float
    y: float

    def __add__(self, other: "Point") -> "Point":
        return Point(self.x + other.x, self.y + other.y)

    def __sub__(self, other: "Point") -> "Point":
        return Point(self.x - other.x, self.y - other.y)

    def __mul__(self, value: float) -> "Point":
        return Point(self.x * value, self.y * value)

    def almost_equals(self, other: "Point", epsilon: float = 1e-12) -> bool:
        return abs(self.x - other.x) <= epsilon and abs(self.y - other.y) <= epsilon


@dataclass(frozen=True)
class LineContour:
    """A closed flattened contour, recorded in traversal order."""

    points: tuple[Point, ...]
    origin_glyph: str
    origin_contour: int
    declared_direction: str | None
    signed_area: float

    @property
    def orientation(self) -> str:
        if self.signed_area > 0:
            return "counterclockwise"
        if self.signed_area < 0:
            return "clockwise"
        return "degenerate"


def signed_area(points: Sequence[Point]) -> float:
    if len(points) < 2:
        return 0.0
    total = 0.0
    for index, point in enumerate(points):
        following = points[(index + 1) % len(points)]
        total += point.x * following.y - following.x * point.y
    return 0.5 * total


def distance_to_line(point: Point, start: Point, end: Point) -> float:
    dx = end.x - start.x
    dy = end.y - start.y
    length = math.hypot(dx, dy)
    if length <= 1e-15:
        return math.hypot(point.x - start.x, point.y - start.y)
    return abs(dy * point.x - dx * point.y + end.x * start.y - end.y * start.x) / length


def flatten_quadratic(
    start: Point,
    control: Point,
    end: Point,
    tolerance: float,
    depth: int = 0,
) -> list[Point]:
    if depth >= 12 or distance_to_line(control, start, end) <= tolerance:
        return [end]
    start_control = midpoint(start, control)
    control_end = midpoint(control, end)
    middle = midpoint(start_control, control_end)
    first = flatten_quadratic(start, start_control, middle, tolerance, depth + 1)
    second = flatten_quadratic(middle, control_end, end, tolerance, depth + 1)
    return first + second


def flatten_cubic(
    start: Point,
    control1: Point,
    control2: Point,
    end: Point,
    tolerance: float,
    depth: int = 0,
) -> list[Point]:
    flatness = max(distance_to_line(control1, start, end), distance_to_line(control2, start, end))
    if depth >= 12 or flatness <= tolerance:
        return [end]
    p01 = midpoint(start, control1)
    p12 = midpoint(control1, control2)
    p23 = midpoint(control2, end)
    p012 = midpoint(p01, p12)
    p123 = midpoint(p12, p23)
    middle = midpoint(p012, p123)
    first = flatten_cubic(start, p01, p012, middle, tolerance, depth + 1)
    second = flatten_cubic(middle, p123, p23, end, tolerance, depth + 1)
    return first + second


def midpoint(first: Point, second: Point) -> Point:
    return Point((first.x + second.x) * 0.5, (first.y + second.y) * 0.5)


def transform_point(point: Point, matrix: Sequence[float]) -> Point:
    a, b, c, d, e, f = matrix
    return Point(a * point.x + c * point.y + e, b * point.x + d * point.y + f)


def transform_contour(contour: LineContour, matrix: Sequence[float]) -> LineContour:
    points = tuple(transform_point(point, matrix) for point in contour.points)
    return LineContour(
        points=points,
        origin_glyph=contour.origin_glyph,
        origin_contour=contour.origin_contour,
        declared_direction=contour.declared_direction,
        signed_area=signed_area(points),
    )


def bounds(points: Iterable[Point]) -> tuple[float, float, float, float]:
    iterator = iter(points)
    try:
        first = next(iterator)
    except StopIteration as exc:
        raise ValueError("bounds require at least one point") from exc
    min_x = max_x = first.x
    min_y = max_y = first.y
    for point in iterator:
        min_x = min(min_x, point.x)
        max_x = max(max_x, point.x)
        min_y = min(min_y, point.y)
        max_y = max(max_y, point.y)
    return min_x, min_y, max_x, max_y

