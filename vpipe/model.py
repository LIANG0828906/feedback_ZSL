from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, field
from typing import Any, Iterable, Sequence


GEOMETRY_EPS = 1e-12


@dataclass(frozen=True, slots=True)
class Point:
    x: float
    y: float

    def __add__(self, other: "Point") -> "Point":
        return Point(self.x + other.x, self.y + other.y)

    def __sub__(self, other: "Point") -> "Point":
        return Point(self.x - other.x, self.y - other.y)

    def __mul__(self, scale: float) -> "Point":
        return Point(self.x * scale, self.y * scale)

    def __truediv__(self, scale: float) -> "Point":
        return Point(self.x / scale, self.y / scale)

    def dot(self, other: "Point") -> float:
        return self.x * other.x + self.y * other.y

    def cross(self, other: "Point") -> float:
        return self.x * other.y - self.y * other.x

    def norm(self) -> float:
        return math.hypot(self.x, self.y)

    def distance_to(self, other: "Point") -> float:
        return math.hypot(self.x - other.x, self.y - other.y)

    def normalized(self) -> "Point":
        n = self.norm()
        if n <= GEOMETRY_EPS:
            return Point(0.0, 0.0)
        return Point(self.x / n, self.y / n)

    def lerp(self, other: "Point", t: float) -> "Point":
        return Point(
            self.x + (other.x - self.x) * t,
            self.y + (other.y - self.y) * t,
        )

    def as_list(self) -> list[float]:
        return [self.x, self.y]

    @staticmethod
    def from_json(value: Any) -> "Point":
        from .errors import E_PARSE_INVALID, PipelineError

        if (
            not isinstance(value, (list, tuple))
            or len(value) != 2
            or not all(isinstance(v, (int, float)) and math.isfinite(v) for v in value)
        ):
            raise PipelineError(
                E_PARSE_INVALID,
                "point must be a finite [x, y] pair",
                value=value,
            )
        return Point(float(value[0]), float(value[1]))


@dataclass(frozen=True, slots=True)
class Segment:
    """A line, quadratic, or cubic Bezier segment with its start point."""

    kind: str
    start: Point
    end: Point
    control1: Point | None = None
    control2: Point | None = None
    trace: tuple["EdgeTrace", ...] = ()

    @staticmethod
    def line(start: Point, end: Point) -> "Segment":
        return Segment("line", start, end)

    @staticmethod
    def quadratic(start: Point, control: Point, end: Point) -> "Segment":
        return Segment("quad", start, end, control)

    @staticmethod
    def cubic(
        start: Point,
        control1: Point,
        control2: Point,
        end: Point,
    ) -> "Segment":
        return Segment("cubic", start, end, control1, control2)

    def validate(self) -> None:
        from .errors import E_PARSE_INVALID, PipelineError

        if self.kind == "line":
            return
        if self.kind == "quad" and self.control1 is not None:
            return
        if (
            self.kind == "cubic"
            and self.control1 is not None
            and self.control2 is not None
        ):
            return
        raise PipelineError(
            E_PARSE_INVALID,
            f"invalid {self.kind!r} segment control points",
        )

    def point_at(self, t: float) -> Point:
        t = min(1.0, max(0.0, t))
        if self.kind == "line":
            return self.start.lerp(self.end, t)
        assert self.control1 is not None
        if self.kind == "quad":
            one = 1.0 - t
            return (
                self.start * (one * one)
                + self.control1 * (2.0 * one * t)
                + self.end * (t * t)
            )
        assert self.control2 is not None
        one = 1.0 - t
        return (
            self.start * (one**3)
            + self.control1 * (3.0 * one * one * t)
            + self.control2 * (3.0 * one * t * t)
            + self.end * (t**3)
        )

    def derivative_at(self, t: float) -> Point:
        t = min(1.0, max(0.0, t))
        if self.kind == "line":
            return self.end - self.start
        assert self.control1 is not None
        if self.kind == "quad":
            one = 1.0 - t
            return (
                (self.control1 - self.start) * (2.0 * one)
                + (self.end - self.control1) * (2.0 * t)
            )
        assert self.control2 is not None
        one = 1.0 - t
        return (
            (self.control1 - self.start) * (3.0 * one * one)
            + (self.control2 - self.control1) * (6.0 * one * t)
            + (self.end - self.control2) * (3.0 * t * t)
        )

    def tangent_at(self, t: float) -> Point:
        d = self.derivative_at(t)
        if d.norm() <= GEOMETRY_EPS:
            d = self.end - self.start
        return d.normalized()

    def reversed(self) -> "Segment":
        return Segment(
            self.kind,
            self.end,
            self.start,
            self.control2 if self.kind == "cubic" else self.control1,
            self.control1 if self.kind == "cubic" else None,
            self.trace,
        )

    def with_trace(self, trace: Sequence["EdgeTrace"]) -> "Segment":
        return Segment(
            self.kind,
            self.start,
            self.end,
            self.control1,
            self.control2,
            tuple(trace),
        )

    def chord_length(self) -> float:
        return self.start.distance_to(self.end)

    def to_record(self) -> dict[str, Any]:
        if self.kind == "line":
            record: dict[str, Any] = {
                "type": "line",
                "to": self.end.as_list(),
            }
        elif self.kind == "quad":
            assert self.control1 is not None
            record = {
                "type": "quad",
                "control": self.control1.as_list(),
                "to": self.end.as_list(),
            }
        elif self.kind == "cubic":
            assert self.control1 is not None and self.control2 is not None
            record = {
                "type": "cubic",
                "control1": self.control1.as_list(),
                "control2": self.control2.as_list(),
                "to": self.end.as_list(),
            }
        else:
            raise ValueError(f"unknown segment kind: {self.kind}")
        if self.trace:
            record["trace"] = [entry.to_dict() for entry in self.trace]
        return record


@dataclass(frozen=True, slots=True)
class EdgeTrace:
    operand: str
    contour_index: int
    contour_id: str
    segment_index: int
    subsegment_index: int
    t_start: float
    t_end: float
    source_kind: str = "input"

    def to_dict(self) -> dict[str, Any]:
        return {
            "operand": self.operand,
            "contour_index": self.contour_index,
            "contour_id": self.contour_id,
            "segment_index": self.segment_index,
            "subsegment_index": self.subsegment_index,
            "t": [round(self.t_start, 12), round(self.t_end, 12)],
            "source": self.source_kind,
        }


@dataclass
class Contour:
    segments: list[Segment]
    contour_id: str
    direction: str
    group: str | None = None
    direction_source: str = "explicit"
    orientation_corrected: bool = False
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def vertices(self) -> list[Point]:
        if not self.segments:
            return []
        return [self.segments[0].start] + [seg.end for seg in self.segments[:-1]]

    def reversed(self) -> "Contour":
        return Contour(
            [seg.reversed() for seg in reversed(self.segments)],
            self.contour_id,
            "cw" if self.direction == "ccw" else "ccw",
            self.group,
            self.direction_source,
            not self.orientation_corrected,
            dict(self.metadata),
        )

    def to_record(self, include_metadata: bool = True) -> dict[str, Any]:
        if not self.segments:
            from .errors import E_PARSE_INVALID, PipelineError

            raise PipelineError(
                E_PARSE_INVALID,
                "cannot serialize an empty contour",
                contour=self.contour_id,
            )
        record: dict[str, Any] = {
            "id": self.contour_id,
            "start": self.segments[0].start.as_list(),
            "segments": [seg.to_record() for seg in self.segments],
        }
        if include_metadata:
            record["direction"] = self.direction
            record["group"] = self.group
            record["direction_source"] = self.direction_source
            if self.orientation_corrected:
                record["orientation_corrected"] = True
            if self.metadata:
                record["metadata"] = self.metadata
        return record


@dataclass(frozen=True, slots=True)
class VertexRef:
    contour_index: int
    vertex_index: int

    def to_dict(self) -> dict[str, int]:
        return {"contour": self.contour_index, "vertex": self.vertex_index}


@dataclass(frozen=True, slots=True)
class VertexShare:
    point: Point
    refs: tuple[VertexRef, ...]
    source: str = "explicit"

    def to_dict(self) -> dict[str, Any]:
        return {
            "point": self.point.as_list(),
            "refs": [ref.to_dict() for ref in self.refs],
            "source": self.source,
        }


@dataclass
class PathData:
    version: int
    fill_rule: str
    tolerance: float
    contours: list[Contour]
    tolerance_source: str = "declared"
    groups: list[str] = field(default_factory=list)
    vertex_shares: list[VertexShare] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)

    def copy(self) -> "PathData":
        return PathData(
            version=self.version,
            fill_rule=self.fill_rule,
            tolerance=self.tolerance,
            contours=[
                Contour(
                    list(contour.segments),
                    contour.contour_id,
                    contour.direction,
                    contour.group,
                    contour.direction_source,
                    contour.orientation_corrected,
                    dict(contour.metadata),
                )
                for contour in self.contours
            ],
            tolerance_source=self.tolerance_source,
            groups=list(self.groups),
            vertex_shares=list(self.vertex_shares),
            metadata=dict(self.metadata),
        )

    def content_hash(self) -> str:
        from .io import path_to_dict

        payload = path_to_dict(self)
        encoded = json.dumps(
            payload,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        return "sha256:" + hashlib.sha256(encoded).hexdigest()

    def bounds(self) -> "Bounds":
        points: list[Point] = []
        for contour in self.contours:
            for segment in contour.segments:
                points.extend(_segment_control_points(segment))
        if not points:
            return Bounds(0.0, 0.0, 1.0, 1.0)
        return Bounds(
            min(p.x for p in points),
            min(p.y for p in points),
            max(p.x for p in points),
            max(p.y for p in points),
        )


@dataclass(frozen=True, slots=True)
class Bounds:
    min_x: float
    min_y: float
    max_x: float
    max_y: float

    @property
    def width(self) -> float:
        return self.max_x - self.min_x

    @property
    def height(self) -> float:
        return self.max_y - self.min_y

    def expanded(self, amount: float) -> "Bounds":
        return Bounds(
            self.min_x - amount,
            self.min_y - amount,
            self.max_x + amount,
            self.max_y + amount,
        )

    def union(self, other: "Bounds") -> "Bounds":
        return Bounds(
            min(self.min_x, other.min_x),
            min(self.min_y, other.min_y),
            max(self.max_x, other.max_x),
            max(self.max_y, other.max_y),
        )

    def contains(self, point: Point) -> bool:
        return (
            self.min_x <= point.x <= self.max_x
            and self.min_y <= point.y <= self.max_y
        )

    def to_dict(self) -> dict[str, float]:
        return {
            "min_x": self.min_x,
            "min_y": self.min_y,
            "max_x": self.max_x,
            "max_y": self.max_y,
        }


def _segment_control_points(segment: Segment) -> Iterable[Point]:
    yield segment.start
    if segment.control1 is not None:
        yield segment.control1
    if segment.control2 is not None:
        yield segment.control2
    yield segment.end


def bounds_for_paths(paths: Sequence[PathData]) -> Bounds:
    if not paths:
        return Bounds(0.0, 0.0, 1.0, 1.0)
    result = paths[0].bounds()
    for path in paths[1:]:
        result = result.union(path.bounds())
    return result

