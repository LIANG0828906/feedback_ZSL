from __future__ import annotations

import math
from collections import defaultdict
from dataclasses import dataclass, field
from typing import Any

from .geometry import (
    flatten_path,
    point_to_polyline_distance,
    prepare_path,
)
from .model import PathData, Point
from .raster import (
    RasterFrame,
    RasterSpec,
    rasterize_operation_truth,
    rasterize_path,
)


@dataclass(frozen=True, slots=True)
class ErrorThresholds:
    max_area_relative_error: float = 0.02
    max_boundary_error: float = 1.0

    def to_dict(self) -> dict[str, float]:
        return {
            "max_area_relative_error": self.max_area_relative_error,
            "max_boundary_error": self.max_boundary_error,
        }


@dataclass
class ErrorReport:
    tolerance: float
    area_relative_error: float
    area_symmetric_difference: float
    truth_area: float
    output_area: float
    max_boundary_error_upper: float
    directional_boundary: dict[str, float]
    amplification: dict[str, float]
    thresholds: ErrorThresholds
    degraded: bool
    reasons: list[str] = field(default_factory=list)
    raster_spec: dict[str, Any] = field(default_factory=dict)
    raster_hash: str | None = None
    truth_raster_hash: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    @property
    def exportable(self) -> bool:
        return not self.degraded

    def to_dict(self) -> dict[str, Any]:
        return {
            "tolerance": self.tolerance,
            "area_relative_error": self.area_relative_error,
            "area_symmetric_difference": self.area_symmetric_difference,
            "truth_area": self.truth_area,
            "output_area": self.output_area,
            "max_boundary_error_upper": self.max_boundary_error_upper,
            "directional_boundary": self.directional_boundary,
            "amplification": self.amplification,
            "thresholds": self.thresholds.to_dict(),
            "degraded": self.degraded,
            "exportable": self.exportable,
            "reasons": self.reasons,
            "raster_spec": self.raster_spec,
            "raster_hash": self.raster_hash,
            "truth_raster_hash": self.truth_raster_hash,
            "details": self.details,
        }

    @staticmethod
    def from_dict(data: dict[str, Any]) -> "ErrorReport":
        thresholds_data = dict(data.get("thresholds", {}))
        return ErrorReport(
            tolerance=float(data["tolerance"]),
            area_relative_error=float(data["area_relative_error"]),
            area_symmetric_difference=float(data["area_symmetric_difference"]),
            truth_area=float(data["truth_area"]),
            output_area=float(data["output_area"]),
            max_boundary_error_upper=float(data["max_boundary_error_upper"]),
            directional_boundary={
                key: float(value)
                for key, value in dict(data.get("directional_boundary", {})).items()
            },
            amplification={
                key: float(value)
                for key, value in dict(data.get("amplification", {})).items()
            },
            thresholds=ErrorThresholds(
                max_area_relative_error=float(
                    thresholds_data.get("max_area_relative_error", 0.02)
                ),
                max_boundary_error=float(
                    thresholds_data.get("max_boundary_error", 0.5)
                ),
            ),
            degraded=bool(data.get("degraded", False)),
            reasons=list(data.get("reasons", [])),
            raster_spec=dict(data.get("raster_spec", {})),
            raster_hash=data.get("raster_hash"),
            truth_raster_hash=data.get("truth_raster_hash"),
            details=dict(data.get("details", {})),
        )


def _operation_boundary_samples(
    path_a: PathData,
    path_b: PathData,
    operation: str,
    tolerance: float,
    flatten_error: float,
    desired_step: float,
) -> tuple[list[Point], list[tuple[Point, Point]], float]:
    from .boolean import _operation_value

    prepared_a = prepare_path(path_a, tolerance)
    prepared_b = prepare_path(path_b, tolerance)
    boundary_tolerance = max(1e-12, tolerance * 0.02)
    probe = max(tolerance * 0.35, 1e-9)
    edges = flatten_path(path_a, flatten_error, "a") + flatten_path(
        path_b,
        flatten_error,
        "b",
    )
    samples: list[Point] = []
    segments: list[tuple[Point, Point]] = []
    max_edge_length = 0.0
    for edge in edges:
        if edge.length <= 1e-14:
            continue
        subdivisions = max(1, math.ceil(edge.length / desired_step))
        normal = edge.left_normal()
        for index in range(subdivisions):
            f0 = index / subdivisions
            f1 = (index + 1) / subdivisions
            midpoint = edge.a.lerp(edge.b, (f0 + f1) * 0.5)
            left = midpoint + normal * probe
            right = midpoint - normal * probe
            left_truth = _operation_value(
                operation,
                prepared_a.contains(left, boundary_tolerance),
                prepared_b.contains(left, boundary_tolerance),
            )
            right_truth = _operation_value(
                operation,
                prepared_a.contains(right, boundary_tolerance),
                prepared_b.contains(right, boundary_tolerance),
            )
            if left_truth == right_truth:
                continue
            a = edge.a.lerp(edge.b, f0)
            b = edge.a.lerp(edge.b, f1)
            samples.append(midpoint)
            segments.append((a, b))
            max_edge_length = max(max_edge_length, a.distance_to(b))
    return samples, segments, max_edge_length


class _SegmentSpatialIndex:
    def __init__(
        self,
        segments: list[tuple[Point, Point]],
        cell_size: float,
    ) -> None:
        self.segments = segments
        self.cell_size = max(cell_size, 1e-9)
        self.buckets: dict[tuple[int, int], list[int]] = defaultdict(list)
        self.max_cell: tuple[int, int] = (0, 0)
        for index, (a, b) in enumerate(segments):
            min_x, max_x = sorted((a.x, b.x))
            min_y, max_y = sorted((a.y, b.y))
            min_cell = (
                math.floor(min_x / self.cell_size),
                math.floor(min_y / self.cell_size),
            )
            max_cell = (
                math.floor(max_x / self.cell_size),
                math.floor(max_y / self.cell_size),
            )
            self.max_cell = (
                max(self.max_cell[0], abs(min_cell[0]), abs(max_cell[0])),
                max(self.max_cell[1], abs(min_cell[1]), abs(max_cell[1])),
            )
            for cx in range(min_cell[0], max_cell[0] + 1):
                for cy in range(min_cell[1], max_cell[1] + 1):
                    self.buckets[(cx, cy)].append(index)

    def distance(self, point: Point) -> float:
        if not self.segments:
            return math.inf
        cx = math.floor(point.x / self.cell_size)
        cy = math.floor(point.y / self.cell_size)
        best = math.inf
        max_ring = max(self.max_cell) + 2
        for ring in range(max_ring + 1):
            for ix in range(cx - ring, cx + ring + 1):
                for iy in range(cy - ring, cy + ring + 1):
                    if ring > 0 and abs(ix - cx) != ring and abs(iy - cy) != ring:
                        continue
                    for index in self.buckets.get((ix, iy), ()):
                        a, b = self.segments[index]
                        distance = point_to_polyline_distance(point, [a, b])
                        if distance < best:
                            best = distance
            if math.isfinite(best) and best <= max(0, ring) * self.cell_size:
                return best
        return best


def evaluate_error(
    path_a: PathData,
    path_b: PathData,
    operation: str,
    output: PathData,
    tolerance: float,
    thresholds: ErrorThresholds | None = None,
    raster_spec: RasterSpec | None = None,
) -> tuple[ErrorReport, RasterFrame, RasterFrame]:
    thresholds = thresholds or ErrorThresholds()
    spec = raster_spec or RasterSpec.auto([path_a, path_b, output], max_side=320)
    truth_frame = rasterize_operation_truth(
        path_a,
        path_b,
        operation,
        spec,
        tolerance,
    )
    output_frame = rasterize_path(output, spec, tolerance, output.content_hash())

    symmetric_cells = sum(
        1 for truth, rendered in zip(truth_frame.mask, output_frame.mask) if truth != rendered
    )
    symmetric_area = symmetric_cells * spec.cell_area
    truth_area = truth_frame.inside_cells * spec.cell_area
    output_area = output_frame.inside_cells * spec.cell_area
    if truth_area <= spec.cell_area * 0.5:
        area_relative = 0.0 if symmetric_area <= spec.cell_area * 0.5 else 1.0
    else:
        area_relative = symmetric_area / truth_area

    flatten_error = max(1e-10, tolerance * 0.25)
    extent = math.hypot(spec.bounds.width, spec.bounds.height)
    desired_step = max(tolerance * 8.0, extent / 256.0, 0.25)
    input_samples, input_segments, input_edge_length = _operation_boundary_samples(
        path_a,
        path_b,
        operation,
        tolerance,
        flatten_error,
        desired_step,
    )
    cell_size = max(tolerance * 4.0, extent / 64.0)
    input_index = _SegmentSpatialIndex(input_segments, cell_size)
    output_edges = flatten_path(output, flatten_error, "result")
    output_segments = [(edge.a, edge.b) for edge in output_edges]
    output_index = _SegmentSpatialIndex(output_segments, cell_size)
    output_samples: list[Point] = []
    max_output_sample_step = 0.0
    for edge in output_edges:
        subdivisions = max(1, math.ceil(edge.length / desired_step))
        for index in range(subdivisions):
            f0 = index / subdivisions
            f1 = (index + 1) / subdivisions
            output_samples.append(edge.a.lerp(edge.b, (f0 + f1) * 0.5))
            max_output_sample_step = max(
                max_output_sample_step,
                edge.length / subdivisions,
            )

    truth_nonempty = truth_area > spec.cell_area * 0.5
    if truth_nonempty and input_samples and output_segments:
        ideal_to_output = max(
            output_index.distance(point) for point in input_samples
        )
        ideal_to_output_upper = (
            ideal_to_output + input_edge_length * 0.5 + flatten_error
        )
    elif truth_nonempty and input_samples and not output_segments:
        ideal_to_output = math.inf
        ideal_to_output_upper = math.inf
    else:
        ideal_to_output = 0.0
        ideal_to_output_upper = 0.0

    if output_samples and input_segments:
        output_to_ideal = max(
            input_index.distance(point) for point in output_samples
        )
        output_to_ideal_upper = (
            output_to_ideal + max_output_sample_step * 0.5 + flatten_error
        )
    elif output_samples:
        output_to_ideal = math.inf
        output_to_ideal_upper = math.inf
    else:
        output_to_ideal = 0.0
        output_to_ideal_upper = 0.0

    max_boundary_upper = max(ideal_to_output_upper, output_to_ideal_upper)
    reported_boundary_upper = _finite_or_extent(max_boundary_upper, spec)
    reasons: list[str] = []
    if area_relative > thresholds.max_area_relative_error:
        reasons.append("area_relative_error_exceeds_threshold")
    if reported_boundary_upper > thresholds.max_boundary_error:
        reasons.append("boundary_error_exceeds_threshold")
    degraded = bool(reasons)
    safe_tolerance = max(tolerance, 1e-12)
    amplification = {
        "tolerance": tolerance,
        "boundary_upper_per_tolerance": reported_boundary_upper / safe_tolerance,
        "area_relative_per_tolerance": area_relative / safe_tolerance,
        "raster_cell_area": spec.cell_area,
        "boundary_upper_per_cell_diagonal": reported_boundary_upper
        / math.hypot(spec.bounds.width / spec.width, spec.bounds.height / spec.height),
    }
    report = ErrorReport(
        tolerance=tolerance,
        area_relative_error=area_relative,
        area_symmetric_difference=symmetric_area,
        truth_area=truth_area,
        output_area=output_area,
        max_boundary_error_upper=reported_boundary_upper,
        directional_boundary={
            "ideal_to_output": _finite_or_extent(ideal_to_output, spec),
            "output_to_ideal": _finite_or_extent(output_to_ideal, spec),
            "ideal_to_output_upper": _finite_or_extent(ideal_to_output_upper, spec),
            "output_to_ideal_upper": _finite_or_extent(output_to_ideal_upper, spec),
        },
        amplification=amplification,
        thresholds=thresholds,
        degraded=degraded,
        reasons=reasons,
        raster_spec=spec.to_dict(),
        raster_hash=output_frame.raster_hash,
        truth_raster_hash=truth_frame.raster_hash,
        details={
            "symmetric_cells": symmetric_cells,
            "total_cells": spec.width * spec.height,
            "truth_boundary_source": "participating_input_contours",
            "input_boundary_sample_count": len(input_samples),
            "output_boundary_sample_count": len(output_samples),
            "flatten_error": flatten_error,
        },
    )
    return report, output_frame, truth_frame


def _finite_or_extent(value: float, spec: RasterSpec) -> float:
    if math.isfinite(value):
        return value
    return math.hypot(spec.bounds.width, spec.bounds.height)

