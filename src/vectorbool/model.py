from __future__ import annotations

import math
from collections.abc import Iterable, Sequence
from dataclasses import dataclass, field
from enum import Enum
from typing import Any


def canonical_float(value: float) -> float:
    value = float(value)
    if value == 0.0:
        return 0.0
    return value


@dataclass(frozen=True, order=True)
class Point:
    x: float
    y: float

    def __post_init__(self) -> None:
        object.__setattr__(self, "x", canonical_float(self.x))
        object.__setattr__(self, "y", canonical_float(self.y))

    def __add__(self, other: Point) -> Point:
        return Point(self.x + other.x, self.y + other.y)

    def __sub__(self, other: Point) -> Point:
        return Point(self.x - other.x, self.y - other.y)

    def __mul__(self, scalar: float) -> Point:
        return Point(self.x * scalar, self.y * scalar)

    def __truediv__(self, scalar: float) -> Point:
        return Point(self.x / scalar, self.y / scalar)

    def dot(self, other: Point) -> float:
        return self.x * other.x + self.y * other.y

    def cross(self, other: Point) -> float:
        return self.x * other.y - self.y * other.x

    def length(self) -> float:
        return math.hypot(self.x, self.y)

    def distance_to(self, other: Point) -> float:
        return math.hypot(self.x - other.x, self.y - other.y)

    def normalized(self) -> Point:
        length = self.length()
        if length == 0.0:
            return Point(0.0, 0.0)
        return Point(self.x / length, self.y / length)

    def left_normal(self) -> Point:
        return Point(-self.y, self.x)

    def as_list(self) -> list[float]:
        return [self.x, self.y]


class SegmentKind(str, Enum):
    LINE = "L"
    QUADRATIC = "Q"
    CUBIC = "C"


class FillRule(str, Enum):
    NONZERO = "nonzero"
    EVENODD = "evenodd"


class Operation(str, Enum):
    UNION = "union"
    INTERSECT = "intersect"
    DIFFERENCE = "difference"
    XOR = "xor"


@dataclass(frozen=True)
class Segment:
    kind: SegmentKind
    points: tuple[Point, ...]
    source_subpath: int | None = None
    source_segment: int | None = None

    @property
    def start(self) -> Point:
        return self.points[0]

    @property
    def end(self) -> Point:
        return self.points[-1]

    def expected_point_count(self) -> int:
        return {
            SegmentKind.LINE: 2,
            SegmentKind.QUADRATIC: 3,
            SegmentKind.CUBIC: 4,
        }[self.kind]

    def reversed(self) -> Segment:
        return Segment(
            kind=self.kind,
            points=tuple(reversed(self.points)),
            source_subpath=self.source_subpath,
            source_segment=self.source_segment,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind.value,
            "points": [point.as_list() for point in self.points],
        }


@dataclass(frozen=True)
class Subpath:
    segments: tuple[Segment, ...]
    closed: bool
    source_index: int | None = None
    contour_role: str = "auto"

    def to_dict(self) -> dict[str, Any]:
        vertices = [segment.start.as_list() for segment in self.segments]
        if self.segments:
            vertices.append(self.segments[-1].end.as_list())
        return {
            "closed": self.closed,
            "vertices": vertices,
            "segments": [segment.to_dict() for segment in self.segments],
        }


@dataclass(frozen=True)
class PathSpec:
    subpaths: tuple[Subpath, ...]
    fill_rule: FillRule

    def to_dict(self) -> dict[str, Any]:
        return {
            "fill_rule": self.fill_rule.value,
            "subpaths": [subpath.to_dict() for subpath in self.subpaths],
        }


@dataclass(frozen=True)
class Contour:
    points: tuple[Point, ...]
    role: str = "outer"

    def signed_area(self) -> float:
        points = self.points
        if len(points) < 3:
            return 0.0
        total = 0.0
        previous = points[-1]
        for point in points:
            total += previous.x * point.y - point.x * previous.y
            previous = point
        return total * 0.5

    def area(self) -> float:
        return abs(self.signed_area())

    def to_dict(self) -> dict[str, Any]:
        return {
            "closed": True,
            "role": self.role,
            "signed_area": self.signed_area(),
            "segments": [
                {
                    "kind": "L",
                    "points": [start.as_list(), end.as_list()],
                }
                for start, end in zip(self.points, self.points[1:] + self.points[:1])
            ],
        }

    def segments(self) -> Iterable[tuple[Point, Point]]:
        return zip(self.points, self.points[1:] + self.points[:1])


@dataclass
class FlattenedSubpath:
    points: list[Point]
    source_index: int | None = None

    def segments(self) -> Iterable[tuple[Point, Point]]:
        for index in range(len(self.points) - 1):
            yield self.points[index], self.points[index + 1]


@dataclass
class FlattenedPath:
    subpaths: list[FlattenedSubpath]
    fill_rule: FillRule


@dataclass(frozen=True)
class Intersection:
    point: Point
    parameter_a: float
    parameter_b: float
    tangent_a: Point
    tangent_b: Point
    kind: str
    confidence: str
    distance: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "point": self.point.as_list(),
            "parameter_a": self.parameter_a,
            "parameter_b": self.parameter_b,
            "tangent_a": self.tangent_a.as_list(),
            "tangent_b": self.tangent_b.as_list(),
            "kind": self.kind,
            "confidence": self.confidence,
            "distance": self.distance,
        }


@dataclass
class IntersectionDiff:
    tolerance: float
    old_count: int
    new_count: int
    added: list[Intersection]
    removed: list[Intersection]
    matched_count: int
    max_position_deviation: float
    summary: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "tolerance": self.tolerance,
            "old_count": self.old_count,
            "new_count": self.new_count,
            "added": [item.to_dict() for item in self.added],
            "removed": [item.to_dict() for item in self.removed],
            "matched_count": self.matched_count,
            "max_position_deviation": self.max_position_deviation,
            "summary": self.summary,
        }


@dataclass
class KeptEdge:
    start: Point
    end: Point
    source: str


@dataclass
class BooleanComputation:
    result: BooleanResult
    kept_edges: list[KeptEdge] = field(default_factory=list)


@dataclass
class BooleanResult:
    operation: Operation
    tolerance: float
    contours: list[Contour]
    intersection_count: int
    report: Any = None
    preview_allowed: bool = False
    self_intersections: list[dict[str, Any]] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "operation": self.operation.value,
            "tolerance": self.tolerance,
            "intersection_count": self.intersection_count,
            "contour_count": len(self.contours),
            "contours": [contour.to_dict() for contour in self.contours],
            "self_intersections": self.self_intersections,
            "preview_allowed": self.preview_allowed,
        }
        if self.report is not None:
            payload["report"] = (
                self.report.to_dict()
                if hasattr(self.report, "to_dict")
                else self.report
            )
        return payload


def path_bounds(path: PathSpec, tolerance: float) -> tuple[float, float, float, float]:
    from .geometry import flatten_path

    flattened = flatten_path(path, tolerance)
    points = [point for subpath in flattened.subpaths for point in subpath.points]
    if not points:
        return (0.0, 0.0, 0.0, 0.0)
    return (
        min(point.x for point in points),
        min(point.y for point in points),
        max(point.x for point in points),
        max(point.y for point in points),
    )


def combined_bounds(
    paths: Sequence[PathSpec], contours: Sequence[Contour], tolerance: float
) -> tuple[float, float, float, float]:
    bounds: list[tuple[float, float, float, float]] = [
        path_bounds(path, tolerance) for path in paths
    ]
    for contour in contours:
        if contour.points:
            bounds.append(
                (
                    min(point.x for point in contour.points),
                    min(point.y for point in contour.points),
                    max(point.x for point in contour.points),
                    max(point.y for point in contour.points),
                )
            )
    if not bounds:
        return (0.0, 0.0, 1.0, 1.0)
    return (
        min(item[0] for item in bounds),
        min(item[1] for item in bounds),
        max(item[2] for item in bounds),
        max(item[3] for item in bounds),
    )

