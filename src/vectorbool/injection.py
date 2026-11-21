from __future__ import annotations

import copy
import math
from collections.abc import Sequence
from dataclasses import dataclass
from enum import Enum
from typing import Any

from .errors import BusinessError, ErrorCode, fail
from .geometry import flatten_path, point_in_flattened_path, signed_area
from .model import FillRule, FlattenedPath, Point
from .parser import _interior_anchor, parse_path


class InjectionKind(str, Enum):
    COINCIDENT_EDGE = "coincident_edge"
    DEGENERATE_SEGMENT = "degenerate_segment"
    ZERO_AREA_CONTOUR = "zero_area_contour"
    HOLE_REVERSED = "hole_reversed"
    NON_FINITE_COORDINATE = "non_finite_coordinate"


INJECTION_CODES = {
    InjectionKind.COINCIDENT_EDGE: ErrorCode.COINCIDENT_EDGE,
    InjectionKind.DEGENERATE_SEGMENT: ErrorCode.DEGENERATE_SEGMENT,
    InjectionKind.ZERO_AREA_CONTOUR: ErrorCode.ZERO_AREA_CONTOUR,
    InjectionKind.HOLE_REVERSED: ErrorCode.HOLE_REVERSED,
    InjectionKind.NON_FINITE_COORDINATE: ErrorCode.NON_FINITE_COORDINATE,
}


@dataclass(frozen=True)
class InjectionLocation:
    kind: InjectionKind
    path: str
    subpath: int
    segment: int

    def to_dict(self) -> dict[str, object]:
        return {
            "kind": self.kind.value,
            "path": self.path,
            "subpath": self.subpath,
            "segment": self.segment,
        }


def normalize_injection_kind(value: str | InjectionKind) -> InjectionKind:
    if isinstance(value, InjectionKind):
        return value
    aliases = {
        "inj_coincident_edge": "coincident_edge",
        "inj_degenerate": "degenerate_segment",
        "inj_zero_area": "zero_area_contour",
        "inj_hole_reversed": "hole_reversed",
        "inj_non_finite": "non_finite_coordinate",
    }
    value = aliases.get(str(value), str(value))
    try:
        return InjectionKind(value)
    except ValueError:
        fail(ErrorCode.PATH_INVALID, f"unknown injection kind: {value}")


def apply_injection(
    raw_a: Any,
    raw_b: Any,
    kind: str | InjectionKind,
    *,
    tolerance: float,
) -> tuple[dict[str, Any], dict[str, Any], InjectionLocation]:
    injection = normalize_injection_kind(kind)
    path_a = copy.deepcopy(raw_a)
    path_b = copy.deepcopy(raw_b)
    _ensure_rectangular_base(path_a)
    _ensure_rectangular_base(path_b)
    location = InjectionLocation(injection, "a", 0, 0)
    if injection is InjectionKind.NON_FINITE_COORDINATE:
        path_a["subpaths"][0]["segments"][0]["points"][0][0] = math.nan
        location = InjectionLocation(injection, "a", 0, 0)
    elif injection is InjectionKind.DEGENERATE_SEGMENT:
        segments = path_a["subpaths"][0]["segments"]
        point = segments[0]["points"][0]
        short_end = [
            float(point[0]) + tolerance * 0.1,
            float(point[1]),
        ]
        segments[0]["points"][1] = short_end
        if len(segments) > 1:
            segments[1]["points"][0] = copy.deepcopy(short_end)
        location = InjectionLocation(injection, "a", 0, 0)
    elif injection is InjectionKind.ZERO_AREA_CONTOUR:
        path_a["subpaths"][0]["segments"] = [
            {"kind": "L", "points": [[0.0, 0.0], [1.0, 0.0]]},
            {"kind": "L", "points": [[1.0, 0.0], [2.0, 0.0]]},
            {"kind": "L", "points": [[2.0, 0.0], [0.0, 0.0]]},
        ]
        location = InjectionLocation(injection, "a", 0, 0)
    elif injection is InjectionKind.HOLE_REVERSED:
        outer = _rectangle_subpath(0.0, 0.0, 4.0, 4.0, clockwise=False)
        inner = _rectangle_subpath(1.0, 1.0, 3.0, 3.0, clockwise=False)
        path_a = {"fill_rule": "nonzero", "subpaths": [outer, inner]}
        location = InjectionLocation(injection, "a", 1, 0)
    elif injection is InjectionKind.COINCIDENT_EDGE:
        path_b = {
            "fill_rule": "nonzero",
            "subpaths": [_rectangle_subpath(2.0, 0.0, 4.0, 2.0, clockwise=False)],
        }
        location = InjectionLocation(injection, "b", 0, 3)
    return path_a, path_b, location


def _ensure_rectangular_base(raw: Any) -> None:
    if (
        not isinstance(raw, dict)
        or not isinstance(raw.get("subpaths"), list)
        or not raw["subpaths"]
        or not isinstance(raw["subpaths"][0], dict)
        or not isinstance(raw["subpaths"][0].get("segments"), list)
        or not raw["subpaths"][0]["segments"]
        or not isinstance(raw["subpaths"][0]["segments"][0], dict)
        or not isinstance(raw["subpaths"][0]["segments"][0].get("points"), list)
        or len(raw["subpaths"][0]["segments"][0]["points"]) < 2
    ):
        fail(ErrorCode.PATH_INVALID, "base path cannot accept the requested injection")


def _rectangle_subpath(
    x0: float, y0: float, x1: float, y1: float, *, clockwise: bool
) -> dict[str, Any]:
    points = (
        [(x0, y0), (x0, y1), (x1, y1), (x1, y0)]
        if clockwise
        else [(x0, y0), (x1, y0), (x1, y1), (x0, y1)]
    )
    return {
        "closed": True,
        "segments": [
            {
                "kind": "L",
                "points": [list(points[index]), list(points[(index + 1) % 4])],
            }
            for index in range(4)
        ],
    }


def validate_entry(
    raw_a: Any,
    raw_b: Any,
    *,
    tolerance: float,
    injections: Sequence[str | InjectionKind | dict[str, Any]] = (),
    injection_metadata: Sequence[dict[str, Any]] = (),
) -> None:
    """Validate entry-level anomalies before any boolean output can exist."""

    kinds: list[InjectionKind] = []
    for value in injections:
        if isinstance(value, dict):
            kinds.append(normalize_injection_kind(value.get("kind", "")))
        else:
            kinds.append(normalize_injection_kind(value))
    metadata = list(injection_metadata)
    metadata.extend(item for item in injections if isinstance(item, dict))
    for item in metadata:
        kind = normalize_injection_kind(item.get("kind", ""))
        if kind not in kinds:
            kinds.append(kind)

    for kind in kinds:
        if kind is InjectionKind.COINCIDENT_EDGE:
            _validate_coincident_edges(raw_a, raw_b, tolerance, metadata)
        elif kind is InjectionKind.HOLE_REVERSED:
            _validate_reversed_holes(raw_a, tolerance)
        elif kind is InjectionKind.NON_FINITE_COORDINATE:
            _validate_finite(raw_a, "a")
            _validate_finite(raw_b, "b")
        elif kind is InjectionKind.DEGENERATE_SEGMENT or kind is InjectionKind.ZERO_AREA_CONTOUR:
            _parse_with_injection_marker(
                raw_a, tolerance, "a", kind
            )
            _parse_with_injection_marker(
                raw_b, tolerance, "b", kind
            )


def _parse_with_injection_marker(
    raw: Any,
    tolerance: float,
    path_name: str,
    kind: InjectionKind,
) -> None:
    try:
        parse_path(raw, tolerance=tolerance, path_name=path_name)
    except BusinessError as error:
        expected = INJECTION_CODES[kind]
        if error.code == expected or error.code == expected.value:
            raise BusinessError(
                code=error.code,
                message=error.message,
                location=error.location.__class__(
                    path=error.location.path,
                    subpath=error.location.subpath,
                    segment=error.location.segment,
                    point=error.location.point,
                    injection=kind.value,
                ),
                details=error.details,
                parent_code=error.parent_code,
            ) from error
        raise


def _validate_finite(raw: Any, path_name: str) -> None:
    if not isinstance(raw, dict) or not isinstance(raw.get("subpaths"), list):
        return
    for subpath_index, subpath in enumerate(raw["subpaths"]):
        if not isinstance(subpath, dict) or not isinstance(subpath.get("segments"), list):
            continue
        for segment_index, segment in enumerate(subpath["segments"]):
            if not isinstance(segment, dict) or not isinstance(segment.get("points"), list):
                continue
            for value in segment["points"]:
                if (
                    not isinstance(value, list)
                    or len(value) != 2
                    or not all(
                        isinstance(coordinate, (int, float))
                        and math.isfinite(float(coordinate))
                        for coordinate in value
                    )
                ):
                    fail(
                        ErrorCode.NON_FINITE_COORDINATE,
                        "injected non-finite coordinate rejected at entry",
                        path=path_name,
                        subpath=subpath_index,
                        segment=segment_index,
                        injection=InjectionKind.NON_FINITE_COORDINATE.value,
                        parent_code=ErrorCode.PATH_INVALID.value,
                    )


def _validate_reversed_holes(raw: Any, tolerance: float) -> None:
    path = parse_path(raw, tolerance=tolerance, path_name="a", normalize=False)
    if path.fill_rule is FillRule.EVENODD:
        return
    flattened = flatten_path(path, tolerance)
    anchors = [_interior_anchor(subpath.points) for subpath in flattened.subpaths]
    for index, subpath in enumerate(flattened.subpaths):
        depth = 0
        for other_index, other in enumerate(flattened.subpaths):
            if index == other_index:
                continue
            if point_in_flattened_path(
                anchors[index],
                FlattenedPath([other], FillRule.EVENODD),
            ):
                depth += 1
        if depth % 2 == 1 and signed_area(subpath.points) > 0.0:
            fail(
                ErrorCode.HOLE_REVERSED,
                "injected hole direction is reversed under nonzero fill",
                path="a",
                subpath=index,
                segment=0,
                injection=InjectionKind.HOLE_REVERSED.value,
            )


def _validate_coincident_edges(
    raw_a: Any,
    raw_b: Any,
    tolerance: float,
    metadata: Sequence[dict[str, Any]],
) -> None:
    path_a = parse_path(raw_a, tolerance=tolerance, path_name="a", normalize=False)
    path_b = parse_path(raw_b, tolerance=tolerance, path_name="b", normalize=False)
    flat_a = flatten_path(path_a, tolerance)
    flat_b = flatten_path(path_b, tolerance)
    for subpath_a, flat_subpath_a in zip(path_a.subpaths, flat_a.subpaths):
        for subpath_b, flat_subpath_b in zip(path_b.subpaths, flat_b.subpaths):
            for segment_a, (a0, a1) in enumerate(flat_subpath_a.segments()):
                for segment_b, (b0, b1) in enumerate(flat_subpath_b.segments()):
                    if _overlap_length(a0, a1, b0, b1, tolerance) > tolerance * 2.0:
                        fail(
                            ErrorCode.COINCIDENT_EDGE,
                            "injected coincident edge rejected at entry",
                            path="b",
                            subpath=subpath_b.source_index,
                            segment=segment_b,
                            point=a0.as_list(),
                            injection=InjectionKind.COINCIDENT_EDGE.value,
                        )


def _overlap_length(
    a0: Point, a1: Point, b0: Point, b1: Point, tolerance: float
) -> float:
    delta_a = a1 - a0
    delta_b = b1 - b0
    length_a = delta_a.length()
    length_b = delta_b.length()
    if length_a <= tolerance or length_b <= tolerance:
        return 0.0
    if abs(delta_a.cross(b0 - a0)) / length_a > tolerance:
        return 0.0
    if abs(delta_a.cross(b1 - a0)) / length_a > tolerance:
        return 0.0
    unit = delta_a / length_a
    projections = sorted(((b0 - a0).dot(unit), (b1 - a0).dot(unit)))
    low = max(0.0, projections[0])
    high = min(length_a, projections[1])
    return max(0.0, high - low)

