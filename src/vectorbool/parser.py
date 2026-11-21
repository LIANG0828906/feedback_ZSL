from __future__ import annotations

import json
import math
from collections import defaultdict
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from .errors import BusinessError, ErrorCode, fail
from .geometry import (
    flatten_subpath,
    point_in_flattened_path,
    signed_area,
)
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


def _required(mapping: dict[str, Any], key: str, *, path_name: str, subpath: int, segment: int | None = None) -> Any:
    if key not in mapping:
        fail(
            ErrorCode.PATH_INVALID,
            f"missing required field '{key}'",
            path=path_name,
            subpath=subpath,
            segment=segment,
        )
    return mapping[key]


def _parse_point(
    value: Any,
    *,
    path_name: str,
    subpath: int,
    segment: int,
    point_index: int,
) -> Point:
    if not isinstance(value, (list, tuple)) or len(value) != 2:
        fail(
            ErrorCode.PATH_INVALID,
            f"point {point_index} must be a two-element array",
            path=path_name,
            subpath=subpath,
            segment=segment,
        )
    try:
        x = float(value[0])
        y = float(value[1])
    except (TypeError, ValueError):
        fail(
            ErrorCode.PATH_INVALID,
            f"point {point_index} contains a non-numeric coordinate",
            path=path_name,
            subpath=subpath,
            segment=segment,
        )
    if not math.isfinite(x) or not math.isfinite(y):
        fail(
            ErrorCode.NON_FINITE_COORDINATE,
            "coordinate is not finite",
            path=path_name,
            subpath=subpath,
            segment=segment,
            point=(x if math.isfinite(x) else 0.0, y if math.isfinite(y) else 0.0),
            parent_code=ErrorCode.PATH_INVALID.value,
        )
    return Point(x, y)


def parse_path(
    raw: Any,
    *,
    tolerance: float,
    path_name: str = "path",
    normalize: bool = True,
) -> PathSpec:
    if not isinstance(raw, dict):
        fail(ErrorCode.PATH_INVALID, "path must be a JSON object", path=path_name)
    tolerance = _validate_tolerance(tolerance)
    fill_rule_value = _required(raw, "fill_rule", path_name=path_name, subpath=None)
    try:
        fill_rule = FillRule(fill_rule_value)
    except (TypeError, ValueError):
        fail(
            ErrorCode.PATH_INVALID,
            "fill_rule must be 'nonzero' or 'evenodd'",
            path=path_name,
        )
    raw_subpaths = _required(raw, "subpaths", path_name=path_name, subpath=None)
    if not isinstance(raw_subpaths, list) or not raw_subpaths:
        fail(ErrorCode.PATH_INVALID, "subpaths must be a non-empty array", path=path_name)

    parsed_subpaths: list[Subpath] = []
    for subpath_index, raw_subpath in enumerate(raw_subpaths):
        if not isinstance(raw_subpath, dict):
            fail(
                ErrorCode.PATH_INVALID,
                "subpath must be an object",
                path=path_name,
                subpath=subpath_index,
            )
        closed = _required(
            raw_subpath, "closed", path_name=path_name, subpath=subpath_index
        )
        if not isinstance(closed, bool):
            fail(
                ErrorCode.PATH_INVALID,
                "closed must be boolean",
                path=path_name,
                subpath=subpath_index,
            )
        raw_segments = _required(
            raw_subpath, "segments", path_name=path_name, subpath=subpath_index
        )
        if not isinstance(raw_segments, list) or not raw_segments:
            fail(
                ErrorCode.PATH_INVALID,
                "segments must be a non-empty array",
                path=path_name,
                subpath=subpath_index,
            )

        segments: list[Segment] = []
        previous_end: Point | None = None
        for segment_index, raw_segment in enumerate(raw_segments):
            if not isinstance(raw_segment, dict):
                fail(
                    ErrorCode.PATH_INVALID,
                    "segment must be an object",
                    path=path_name,
                    subpath=subpath_index,
                    segment=segment_index,
                )
            kind_value = _required(
                raw_segment,
                "kind",
                path_name=path_name,
                subpath=subpath_index,
                segment=segment_index,
            )
            try:
                kind = SegmentKind(kind_value)
            except (TypeError, ValueError):
                fail(
                    ErrorCode.PATH_INVALID,
                    "segment kind must be L, Q, or C",
                    path=path_name,
                    subpath=subpath_index,
                    segment=segment_index,
                )
            raw_points = _required(
                raw_segment,
                "points",
                path_name=path_name,
                subpath=subpath_index,
                segment=segment_index,
            )
            expected_count = {
                SegmentKind.LINE: 2,
                SegmentKind.QUADRATIC: 3,
                SegmentKind.CUBIC: 4,
            }[kind]
            if not isinstance(raw_points, list) or len(raw_points) != expected_count:
                fail(
                    ErrorCode.PATH_INVALID,
                    f"{kind.value} segment requires {expected_count} points",
                    path=path_name,
                    subpath=subpath_index,
                    segment=segment_index,
                )
            points = tuple(
                _parse_point(
                    value,
                    path_name=path_name,
                    subpath=subpath_index,
                    segment=segment_index,
                    point_index=point_index,
                )
                for point_index, value in enumerate(raw_points)
            )
            if previous_end is not None:
                gap = previous_end.distance_to(points[0])
                if gap > tolerance:
                    fail(
                        ErrorCode.PATH_INVALID,
                        "segment start does not meet previous segment end within tolerance",
                        path=path_name,
                        subpath=subpath_index,
                        segment=segment_index,
                        point=points[0].as_list(),
                    )
                if gap > 0.0:
                    points = (previous_end,) + points[1:]
            segment = Segment(
                kind=kind,
                points=points,
                source_subpath=subpath_index,
                source_segment=segment_index,
            )
            if _segment_span(segment) <= tolerance:
                fail(
                    ErrorCode.DEGENERATE_SEGMENT,
                    "segment is shorter than the geometry tolerance",
                    path=path_name,
                    subpath=subpath_index,
                    segment=segment_index,
                    point=segment.start.as_list(),
                )
            segments.append(segment)
            previous_end = segment.end

        if closed and segments[0].start.distance_to(segments[-1].end) > tolerance:
            closing = Segment(
                kind=SegmentKind.LINE,
                points=(segments[-1].end, segments[0].start),
                source_subpath=subpath_index,
                source_segment=len(segments),
            )
            if _segment_span(closing) <= tolerance:
                fail(
                    ErrorCode.DEGENERATE_SEGMENT,
                    "closing segment is shorter than the geometry tolerance",
                    path=path_name,
                    subpath=subpath_index,
                    segment=len(segments),
                    point=closing.start.as_list(),
                )
            segments.append(closing)
        parsed_subpaths.append(
            Subpath(
                segments=tuple(segments),
                closed=closed,
                source_index=subpath_index,
            )
        )

    parsed_subpaths = _merge_coincident_vertices(
        parsed_subpaths, tolerance, path_name
    )
    path = PathSpec(subpaths=tuple(parsed_subpaths), fill_rule=fill_rule)
    if normalize:
        return normalize_path(path, tolerance=tolerance, path_name=path_name)
    return path


def parse_segment(
    raw: Any,
    *,
    tolerance: float,
    path_name: str = "segment",
) -> Segment:
    tolerance = _validate_tolerance(tolerance)
    if not isinstance(raw, dict):
        fail(ErrorCode.PATH_INVALID, "segment must be a JSON object", path=path_name)
    kind_value = _required(raw, "kind", path_name=path_name, subpath=0, segment=0)
    try:
        kind = SegmentKind(kind_value)
    except (TypeError, ValueError):
        fail(
            ErrorCode.PATH_INVALID,
            "segment kind must be L, Q, or C",
            path=path_name,
            subpath=0,
            segment=0,
        )
    raw_points = _required(
        raw, "points", path_name=path_name, subpath=0, segment=0
    )
    expected_count = {
        SegmentKind.LINE: 2,
        SegmentKind.QUADRATIC: 3,
        SegmentKind.CUBIC: 4,
    }[kind]
    if not isinstance(raw_points, list) or len(raw_points) != expected_count:
        fail(
            ErrorCode.PATH_INVALID,
            f"{kind.value} segment requires {expected_count} points",
            path=path_name,
            subpath=0,
            segment=0,
        )
    points = tuple(
        _parse_point(
            value,
            path_name=path_name,
            subpath=0,
            segment=0,
            point_index=index,
        )
        for index, value in enumerate(raw_points)
    )
    segment = Segment(kind=kind, points=points, source_subpath=0, source_segment=0)
    if _segment_span(segment) <= tolerance:
        fail(
            ErrorCode.DEGENERATE_SEGMENT,
            "segment is shorter than the geometry tolerance",
            path=path_name,
            subpath=0,
            segment=0,
            point=segment.start.as_list(),
        )
    return segment


def _validate_tolerance(tolerance: float) -> float:
    try:
        value = float(tolerance)
    except (TypeError, ValueError):
        fail(ErrorCode.PATH_INVALID, "tolerance must be numeric")
    if not math.isfinite(value) or value <= 0.0:
        fail(ErrorCode.PATH_INVALID, "tolerance must be finite and positive")
    return value


def _segment_span(segment: Segment) -> float:
    start = segment.start
    return max(start.distance_to(point) for point in segment.points[1:])


def _merge_coincident_vertices(
    subpaths: Sequence[Subpath], tolerance: float, path_name: str
) -> list[Subpath]:
    buckets: dict[tuple[int, int], list[Point]] = defaultdict(list)

    def snap(point: Point) -> Point:
        cell = (
            math.floor(point.x / tolerance),
            math.floor(point.y / tolerance),
        )
        candidates = [
            candidate
            for dx in (-1, 0, 1)
            for dy in (-1, 0, 1)
            for candidate in buckets.get((cell[0] + dx, cell[1] + dy), ())
        ]
        if candidates:
            nearest = min(
                candidates,
                key=lambda candidate: (
                    candidate.distance_to(point),
                    candidate.x,
                    candidate.y,
                ),
            )
            if nearest.distance_to(point) <= tolerance:
                return nearest
        buckets[cell].append(point)
        return point

    merged: list[Subpath] = []
    for subpath in subpaths:
        segments: list[Segment] = []
        for segment in subpath.segments:
            merged_segment = Segment(
                kind=segment.kind,
                points=tuple(snap(point) for point in segment.points),
                source_subpath=segment.source_subpath,
                source_segment=segment.source_segment,
            )
            if _segment_span(merged_segment) <= tolerance:
                fail(
                    ErrorCode.DEGENERATE_SEGMENT,
                    "vertex merging produced a segment shorter than tolerance",
                    path=path_name,
                    subpath=segment.source_subpath,
                    segment=segment.source_segment,
                    point=merged_segment.start.as_list(),
                )
            segments.append(merged_segment)
        merged.append(
            Subpath(
                segments=tuple(segments),
                closed=subpath.closed,
                source_index=subpath.source_index,
                contour_role=subpath.contour_role,
            )
        )
    return merged


def _reverse_subpath(subpath: Subpath) -> Subpath:
    segments = tuple(segment.reversed() for segment in reversed(subpath.segments))
    return Subpath(
        segments=segments,
        closed=subpath.closed,
        source_index=subpath.source_index,
        contour_role=subpath.contour_role,
    )


def normalize_path(path: PathSpec, *, tolerance: float, path_name: str = "path") -> PathSpec:
    flattened = FlattenedPath(
        subpaths=[flatten_subpath(subpath, tolerance) for subpath in path.subpaths],
        fill_rule=path.fill_rule,
    )
    for index, subpath in enumerate(flattened.subpaths):
        if not _has_enclosed_area(subpath.points, tolerance):
            fail(
                ErrorCode.ZERO_AREA_CONTOUR,
                "contour has no finite enclosed area",
                path=path_name,
                subpath=index,
            )

    # Determine nesting from a point strictly inside each contour. Translating
    # toward the centroid avoids testing a vertex that lies on another boundary.
    depths: list[int] = []
    for index, subpath in enumerate(flattened.subpaths):
        anchor = _interior_anchor(subpath.points)
        depth = 0
        for other_index, other in enumerate(flattened.subpaths):
            if index == other_index or not other.points:
                continue
            if point_in_flattened_path(anchor, FlattenedPath([other], path.fill_rule)):
                depth += 1
        depths.append(depth)

    normalized: list[Subpath] = []
    for subpath, depth in zip(path.subpaths, depths):
        flat = flatten_subpath(subpath, tolerance)
        wants_ccw = depth % 2 == 0
        is_ccw = signed_area(flat.points) > 0.0
        if is_ccw != wants_ccw:
            subpath = _reverse_subpath(subpath)
        normalized.append(
            Subpath(
                segments=subpath.segments,
                closed=subpath.closed,
                source_index=subpath.source_index,
                contour_role="outer" if depth % 2 == 0 else "hole",
            )
        )
    return PathSpec(subpaths=tuple(normalized), fill_rule=path.fill_rule)


def _has_enclosed_area(points: Sequence[Point], tolerance: float) -> bool:
    if len(points) < 3:
        return False
    x_min = min(point.x for point in points)
    x_max = max(point.x for point in points)
    y_min = min(point.y for point in points)
    y_max = max(point.y for point in points)
    if (x_max - x_min) <= tolerance or (y_max - y_min) <= tolerance:
        return False
    rows = 64
    row_height = (y_max - y_min) / rows
    for row in range(rows):
        y = y_min + (row + 0.5) * row_height
        crossings: list[float] = []
        previous = points[-1]
        for current in points:
            if (previous.y > y) != (current.y > y):
                x = previous.x + (y - previous.y) * (
                    (current.x - previous.x) / (current.y - previous.y)
                )
                crossings.append(x)
            previous = current
        crossings.sort()
        for left, right in zip(crossings[::2], crossings[1::2]):
            if right - left > tolerance:
                return True
    return False


def _interior_anchor(points: Sequence[Point]) -> Point:
    if len(points) < 3:
        return points[0] if points else Point(0.0, 0.0)
    area = signed_area(points)
    scale = max(
        max(point.x for point in points) - min(point.x for point in points),
        max(point.y for point in points) - min(point.y for point in points),
        1.0,
    )
    # A near-boundary point is a safer nesting probe than the centroid: a
    # centroid of an outer ring can accidentally fall inside one of its holes.
    for a, b in zip(points, points[1:] + points[:1]):
        delta = b - a
        length = delta.length()
        if length <= 0.0:
            continue
        normal = delta.left_normal().normalized()
        if area < 0.0:
            normal = normal * -1.0
        offset = min(scale * 1.0e-4, length * 0.1)
        candidate = (a + b) * 0.5 + normal * offset
        if point_in_flattened_path(
            candidate,
            FlattenedPath([FlattenedSubpath(list(points))], FillRule.EVENODD),
        ):
            return candidate
    # Scanline candidates are deterministic and work for concave contours.
    for fraction in (0.5, 0.25, 0.75, 0.125, 0.375, 0.625, 0.875):
        y = min(point.y for point in points) + scale * fraction
        intersections: list[float] = []
        previous = points[-1]
        for current in points:
            if (previous.y > y) != (current.y > y):
                x = previous.x + (y - previous.y) * (
                    (current.x - previous.x) / (current.y - previous.y)
                )
                intersections.append(x)
            previous = current
        intersections.sort()
        for left, right in zip(intersections[::2], intersections[1::2]):
            if right > left:
                return Point((left + right) * 0.5, y)
    return points[0]


def load_path(
    filename: str | Path,
    *,
    tolerance: float,
    path_name: str | None = None,
) -> PathSpec:
    path = Path(filename)
    try:
        with path.open("r", encoding="utf-8") as stream:
            raw = json.load(stream)
    except (OSError, json.JSONDecodeError) as error:
        raise BusinessError(
            ErrorCode.PATH_INVALID,
            f"cannot read path JSON: {error}",
        ) from error
    return parse_path(raw, tolerance=tolerance, path_name=path_name or path.name)

