from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Sequence

from .errors import Diagnostic, E_VERSION_LOSS
from .geometry import (
    actual_direction,
    contour_depth,
    contour_interior_point,
    contour_polyline,
    flatten_path,
    point_on_segment,
    point_to_polyline_distance,
    polygon_signed_area,
)
from .model import Contour, PathData, Point, VertexRef, VertexShare


@dataclass
class VersionReport:
    code: str | None = None
    diagnostics: list[Diagnostic] = field(default_factory=list)
    affected: list[dict[str, Any]] = field(default_factory=list)
    tolerance: float | None = None
    tolerance_source: str | None = None
    geometry: dict[str, Any] | None = None

    @property
    def lossless(self) -> bool:
        return self.code is None

    def to_dict(self) -> dict[str, Any]:
        return {
            "lossless": self.lossless,
            "code": self.code,
            "diagnostics": [item.to_dict() for item in self.diagnostics],
            "affected": self.affected,
            "tolerance": self.tolerance,
            "tolerance_source": self.tolerance_source,
            "geometry": self.geometry,
        }


def infer_v1_metadata(path: PathData) -> PathData:
    """Apply the documented v1 order/containment rule and derive shares."""

    max_error = max(1e-8, path.tolerance * 0.25)
    original_directions = [actual_direction(contour, max_error) for contour in path.contours]
    improved: list[Contour] = []
    for index, contour in enumerate(path.contours):
        depth = contour_depth(contour, path.contours, max_error)
        expected = "ccw" if depth % 2 == 0 else "cw"
        corrected = original_directions[index] != expected
        current = contour
        if corrected:
            current = contour.reversed()
        current.direction = expected
        current.direction_source = "v1_order_containment"
        current.orientation_corrected = corrected
        current.metadata = dict(current.metadata)
        current.metadata.update(
            {
                "v1_nesting_depth": depth,
                "v1_original_direction": original_directions[index],
            }
        )
        improved.append(current)
    path.contours = improved
    path.vertex_shares = derive_vertex_shares(path, "v1_coordinate_derivation")
    path.metadata = dict(path.metadata)
    path.metadata["v1_rule"] = "outer=ccw,odd_nesting_depth=cw"
    path.metadata["v1_share_rule"] = "coincident_vertices_within_tolerance"
    return path


def derive_vertex_shares(path: PathData, source: str) -> list[VertexShare]:
    tolerance = max(path.tolerance, 1e-12)
    registry: list[tuple[Point, list[VertexRef]]] = []
    for contour_index, contour in enumerate(path.contours):
        for vertex_index, vertex in enumerate(contour.vertices):
            target: list[VertexRef] | None = None
            for reference_point, refs in registry:
                if reference_point.distance_to(vertex) <= tolerance:
                    target = refs
                    break
            if target is None:
                registry.append((vertex, [VertexRef(contour_index, vertex_index)]))
            else:
                target.append(VertexRef(contour_index, vertex_index))
    shares: list[VertexShare] = []
    for point, refs in registry:
        if len(refs) > 1:
            shares.append(VertexShare(point, tuple(refs), source))
    return shares


def downgrade_to_v1(path: PathData) -> tuple[dict[str, Any], VersionReport]:
    from .io import serialize_v1

    payload, report = serialize_v1(path)
    return payload, report


def build_v1_loss_report(path: PathData) -> VersionReport:
    losses: list[dict[str, Any]] = []
    for index, contour in enumerate(path.contours):
        losses.append(
            {
                "contour_index": index,
                "contour_id": contour.contour_id,
                "field": "direction",
                "value": contour.direction,
                "reason": "v1 derives direction from contour order and nesting",
            }
        )
        if contour.group is not None:
            losses.append(
                {
                    "contour_index": index,
                    "contour_id": contour.contour_id,
                    "field": "group",
                    "value": contour.group,
                    "reason": "v1 has no group reference field",
                }
            )
    if path.vertex_shares:
        losses.append(
            {
                "contour_index": None,
                "contour_id": None,
                "field": "vertex_shares",
                "value": len(path.vertex_shares),
                "reason": "v1 only supports coordinate-derived sharing",
            }
        )
    if path.groups:
        losses.append(
            {
                "contour_index": None,
                "contour_id": None,
                "field": "groups",
                "value": list(path.groups),
                "reason": "v1 has no group table",
            }
        )
    if any(segment.trace for contour in path.contours for segment in contour.segments):
        losses.append(
            {
                "contour_index": None,
                "contour_id": None,
                "field": "edge_trace",
                "value": "present",
                "reason": "v1 has no standardized retained-edge provenance field",
            }
        )
    if path.metadata:
        losses.append(
            {
                "contour_index": None,
                "contour_id": None,
                "field": "metadata",
                "value": dict(path.metadata),
                "reason": "v1 has no path-level metadata field",
            }
        )
    tolerance_affected = {
        "contour_index": None,
        "contour_id": None,
        "field": "tolerance",
        "value": path.tolerance,
        "reason": "v1 has no tolerance declaration; recorded only in this compatibility report",
    }
    losses.append(tolerance_affected)
    diagnostics = [
        Diagnostic(
            E_VERSION_LOSS,
            "v2 metadata cannot be represented losslessly in v1",
            {
                "fields": sorted({str(item["field"]) for item in losses}),
                "affected_count": len(losses),
                "tolerance_retained_in_report_only": path.tolerance,
            },
        )
    ]
    return VersionReport(
        code=E_VERSION_LOSS,
        diagnostics=diagnostics,
        affected=losses,
        tolerance=path.tolerance,
        tolerance_source=path.tolerance_source,
    )


def _boundary_sample(path: PathData, tolerance: float) -> list[Point]:
    edges = flatten_path(path, max(1e-9, tolerance * 0.25), "compare")
    if not edges:
        return []
    samples = [edges[0].a]
    for edge in edges:
        samples.append(edge.midpoint)
        samples.append(edge.b)
    return samples


def compare_geometry(
    left: PathData,
    right: PathData,
    tolerance: float | None = None,
) -> dict[str, Any]:
    tol = max(
        1e-9,
        tolerance
        if tolerance is not None
        else max(left.tolerance, right.tolerance),
    )
    left_points = _boundary_sample(left, tol)
    right_points = _boundary_sample(right, tol)
    left_polylines = [
        contour_polyline(contour, max(1e-9, tol * 0.25))
        for contour in left.contours
    ]
    right_polylines = [
        contour_polyline(contour, max(1e-9, tol * 0.25))
        for contour in right.contours
    ]

    def directed_max(source: Sequence[Point], targets: Sequence[Sequence[Point]]) -> float:
        if not source or not targets:
            return math.inf
        maximum = 0.0
        for point in source:
            distance = min(point_to_polyline_distance(point, polyline) for polyline in targets)
            maximum = max(maximum, distance)
        return maximum

    forward = directed_max(left_points, right_polylines)
    backward = directed_max(right_points, left_polylines)
    max_boundary = max(forward, backward)
    left_area = sum(polygon_signed_area(p) for p in left_polylines)
    right_area = sum(polygon_signed_area(p) for p in right_polylines)
    agree = max_boundary <= tol + 1e-12
    return {
        "tolerance": tol,
        "max_boundary_deviation": max_boundary,
        "area_left": left_area,
        "area_right": right_area,
        "area_delta": abs(left_area - right_area),
        "consistent": agree,
        "directional": {
            "left_to_right": forward,
            "right_to_left": backward,
        },
    }


def canonical_geometry(path: PathData, tolerance: float | None = None) -> list[list[list[float]]]:
    """Deterministic boundary samples, normalized for closure and winding."""

    tol = max(1e-9, tolerance if tolerance is not None else path.tolerance)
    result: list[list[list[float]]] = []
    for contour in path.contours:
        polyline = contour_polyline(contour, max(1e-9, tol * 0.25))
        if len(polyline) > 1 and polyline[0].distance_to(polyline[-1]) <= tol:
            polyline = polyline[:-1]
        if len(polyline) < 3:
            continue
        if polygon_signed_area(polyline) < 0:
            polyline = list(reversed(polyline))
        minimum_index = min(
            range(len(polyline)),
            key=lambda index: (round(polyline[index].x / tol), round(polyline[index].y / tol)),
        )
        rotated = polyline[minimum_index:] + polyline[:minimum_index]
        result.append([[round(p.x, 9), round(p.y, 9)] for p in rotated])
    result.sort(key=lambda contour: (contour[0][0], contour[0][1], len(contour)))
    return result

