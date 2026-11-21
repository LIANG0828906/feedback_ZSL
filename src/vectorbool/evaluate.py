from __future__ import annotations

import itertools
import math
from collections.abc import Callable
from dataclasses import dataclass, field

import numpy as np

from .geometry import (
    EPSILON,
    closest_point_on_segment,
    flatten_path,
    signed_area,
)
from .model import BooleanResult, FlattenedPath, Operation, PathSpec, Point

CancelCheck = Callable[[], bool] | None


@dataclass
class ErrorReport:
    accepted: bool
    status: str
    tolerance: float
    area_symmetric_difference: float
    expected_area: float
    area_relative_deviation: float
    max_boundary_deviation: float
    boundary_deviation_upper_bound: float
    area_relative_threshold: float
    boundary_threshold: float
    tolerance_amplification: float
    scanline_rows: int
    degradation_reasons: list[str] = field(default_factory=list)
    metadata: dict[str, object] = field(default_factory=dict)

    @property
    def render_allowed(self) -> bool:
        return self.accepted and self.status == "ok"

    def to_dict(self) -> dict[str, object]:
        return {
            "accepted": self.accepted,
            "status": self.status,
            "render_allowed": self.render_allowed,
            "tolerance": self.tolerance,
            "area_symmetric_difference": self.area_symmetric_difference,
            "expected_area": self.expected_area,
            "area_relative_deviation": self.area_relative_deviation,
            "max_boundary_deviation": self.max_boundary_deviation,
            "boundary_deviation_upper_bound": self.boundary_deviation_upper_bound,
            "area_relative_threshold": self.area_relative_threshold,
            "boundary_threshold": self.boundary_threshold,
            "tolerance_amplification": self.tolerance_amplification,
            "scanline_rows": self.scanline_rows,
            "degradation_reasons": self.degradation_reasons,
            "metadata": self.metadata,
        }


def _operation_contains(
    in_a: bool, in_b: bool, operation: Operation
) -> bool:
    if operation is Operation.UNION:
        return in_a or in_b
    if operation is Operation.INTERSECT:
        return in_a and in_b
    if operation is Operation.DIFFERENCE:
        return in_a and not in_b
    return in_a != in_b


def _path_contains(point: Point, flattened: FlattenedPath) -> bool:
    from .geometry import point_in_flattened_path

    return point_in_flattened_path(point, flattened)


def _path_contains_many(
    points: list[Point], flattened: FlattenedPath
) -> np.ndarray:
    if not points:
        return np.zeros((0,), dtype=bool)
    query = np.asarray([(point.x, point.y) for point in points], dtype=np.float64)
    px = query[:, 0]
    py = query[:, 1]
    if flattened.fill_rule.value == "evenodd":
        parity = np.zeros((len(points),), dtype=np.int64)
        for subpath in flattened.subpaths:
            previous = subpath.points[-1]
            for current in subpath.points:
                straddles = (previous.y > py) != (current.y > py)
                denominator = np.where(
                    straddles, current.y - previous.y, 1.0
                )
                x = previous.x + (py - previous.y) * (
                    (current.x - previous.x) / denominator
                )
                crossing = straddles & (x > px)
                parity ^= crossing.astype(np.int64)
                previous = current
        return (parity & 1) != 0
    winding = np.zeros((len(points),), dtype=np.int64)
    for subpath in flattened.subpaths:
        previous = subpath.points[-1]
        for current in subpath.points:
            cross = (current.x - previous.x) * (py - previous.y) - (
                px - previous.x
            ) * (current.y - previous.y)
            upward = (previous.y <= py) & (current.y > py) & (cross > 0.0)
            downward = (previous.y > py) & (current.y <= py) & (cross < 0.0)
            winding += upward.astype(np.int64)
            winding -= downward.astype(np.int64)
            previous = current
    return winding != 0


def _contours_contain(point: Point, contours: list[list[Point]]) -> bool:
    from .geometry import winding_number

    return sum(winding_number(point, points) for points in contours) != 0


def _all_edges(paths: list[FlattenedPath]) -> list[tuple[Point, Point]]:
    result: list[tuple[Point, Point]] = []
    for path in paths:
        for subpath in path.subpaths:
            result.extend(subpath.segments())
    return result


def _row_crossings(
    edges: list[tuple[Point, Point]], y: float
) -> list[float]:
    crossings: list[float] = []
    for start, end in edges:
        if (start.y > y) == (end.y > y):
            continue
        x = start.x + (y - start.y) * (
            (end.x - start.x) / (end.y - start.y)
        )
        crossings.append(x)
    crossings.sort()
    filtered: list[float] = []
    for value in crossings:
        if not filtered or abs(value - filtered[-1]) > 1.0e-12:
            filtered.append(value)
    return filtered


def _scanline_area_metrics(
    flattened_a: FlattenedPath,
    flattened_b: FlattenedPath,
    output_contours: list[list[Point]],
    operation: Operation,
    *,
    rows: int,
    tolerance: float,
) -> tuple[float, float, float, int]:
    edges_a = _all_edges([flattened_a])
    edges_b = _all_edges([flattened_b])
    edges_output: list[tuple[Point, Point]] = []
    for points in output_contours:
        if len(points) >= 2:
            edges_output.extend(zip(points, points[1:] + points[:1]))
    all_points = [
        point
        for edges in (edges_a, edges_b, edges_output)
        for edge in edges
        for point in edge
    ]
    if not all_points:
        return (0.0, 0.0, 0.0, 0)
    x_min = min(point.x for point in all_points)
    x_max = max(point.x for point in all_points)
    y_min = min(point.y for point in all_points)
    y_max = max(point.y for point in all_points)
    if x_max - x_min <= tolerance:
        x_max = x_min + tolerance
    if y_max - y_min <= tolerance:
        y_max = y_min + tolerance
    y_events = sorted({point.y for point in all_points} | {y_min, y_max})
    y_samples: list[tuple[float, float]] = []
    for lower, upper in itertools.pairwise(y_events):
        height = upper - lower
        if height <= 1.0e-15:
            continue
        y_samples.append(((lower + upper) * 0.5, height))
    if not y_samples:
        height = max((y_max - y_min) / max(rows, 1), tolerance)
        y_samples = [(y_min + height * 0.5, height)]
    expected_area = 0.0
    symmetric_area = 0.0
    union_area = 0.0
    for y, slab_height in y_samples:
        cuts = sorted(
            set(
                [x_min, x_max]
                + _row_crossings(edges_a, y)
                + _row_crossings(edges_b, y)
                + _row_crossings(edges_output, y)
            )
        )
        for left, right in itertools.pairwise(cuts):
            width = right - left
            if width <= 1.0e-14:
                continue
            point = Point((left + right) * 0.5, y)
            in_a = _path_contains(point, flattened_a)
            in_b = _path_contains(point, flattened_b)
            expected = _operation_contains(in_a, in_b, operation)
            actual = _contours_contain(point, output_contours)
            cell_area = width * slab_height
            if expected:
                expected_area += cell_area
            if expected or actual:
                union_area += cell_area
            if expected != actual:
                symmetric_area += cell_area
    return expected_area, symmetric_area, max(union_area, expected_area), len(y_samples)


def _status_on_sides(
    midpoint: Point,
    normal: Point,
    epsilon: float,
    flattened_a: FlattenedPath,
    flattened_b: FlattenedPath,
    operation: Operation,
) -> tuple[bool, bool]:
    left = _operation_contains(
        _path_contains(midpoint + normal * epsilon, flattened_a),
        _path_contains(midpoint + normal * epsilon, flattened_b),
        operation,
    )
    right = _operation_contains(
        _path_contains(midpoint - normal * epsilon, flattened_a),
        _path_contains(midpoint - normal * epsilon, flattened_b),
        operation,
    )
    return left, right


def _expected_boundary_samples(
    flattened_a: FlattenedPath,
    flattened_b: FlattenedPath,
    operation: Operation,
    tolerance: float,
) -> list[Point]:
    candidate_points: list[Point] = []
    sample_points: list[Point] = []
    for edges in (_all_edges([flattened_a]), _all_edges([flattened_b])):
        for start, end in edges:
            delta = end - start
            length = delta.length()
            if length <= tolerance:
                continue
            normal = delta.left_normal().normalized()
            epsilon = min(
                max(tolerance * 0.25, 1.0e-10),
                length * 0.125,
            )
            step_count = max(1, min(4, math.ceil(length / max(tolerance * 32, 1e-9))))
            for index in range(step_count + 1):
                point = start + delta * (index / step_count)
                sample_points.append(point)
                candidate_points.append(point + normal * epsilon)
                candidate_points.append(point - normal * epsilon)
    if not sample_points:
        return []
    in_a = _path_contains_many(candidate_points, flattened_a)
    in_b = _path_contains_many(candidate_points, flattened_b)
    if operation is Operation.UNION:
        desired = in_a | in_b
    elif operation is Operation.INTERSECT:
        desired = in_a & in_b
    elif operation is Operation.DIFFERENCE:
        desired = in_a & ~in_b
    else:
        desired = in_a != in_b
    desired = desired.reshape((-1, 2))
    samples = [
        point
        for point, (left, right) in zip(sample_points, desired)
        if left != right
    ]
    return _deduplicate_samples(samples, tolerance * 0.25)


def _output_boundary_samples(
    contours: list[list[Point]], tolerance: float
) -> list[Point]:
    samples: list[Point] = []
    for points in contours:
        if len(points) < 2:
            continue
        for start, end in zip(points, points[1:] + points[:1]):
            length = start.distance_to(end)
            step_count = max(1, min(4, math.ceil(length / max(tolerance * 16, 1e-9))))
            for index in range(step_count):
                samples.append(start + (end - start) * (index / step_count))
    return _deduplicate_samples(samples, tolerance * 0.25)


def _deduplicate_samples(points: list[Point], tolerance: float) -> list[Point]:
    result: list[Point] = []
    for point in points:
        if not result or point.distance_to(result[-1]) > tolerance:
            result.append(point)
    return result


def _distance_to_contours(
    point: Point, contours: list[list[Point]], tolerance: float
) -> float:
    best = math.inf
    for points in contours:
        if len(points) < 2:
            continue
        for start, end in zip(points, points[1:] + points[:1]):
            if (
                point.x < min(start.x, end.x) - best
                or point.x > max(start.x, end.x) + best
                or point.y < min(start.y, end.y) - best
                or point.y > max(start.y, end.y) + best
            ):
                continue
            _, _, distance = closest_point_on_segment(point, start, end)
            best = min(best, distance)
    return best


class _BoundaryIndex:
    def __init__(self, contours: list[list[Point]]) -> None:
        starts: list[tuple[float, float]] = []
        ends: list[tuple[float, float]] = []
        for points in contours:
            if len(points) < 2:
                continue
            for start, end in zip(points, points[1:] + points[:1]):
                if start.distance_to(end) <= 1.0e-15:
                    continue
                starts.append((start.x, start.y))
                ends.append((end.x, end.y))
        if starts:
            self.starts = np.asarray(starts, dtype=np.float64)
            self.ends = np.asarray(ends, dtype=np.float64)
            self.deltas = self.ends - self.starts
            self.length_squared = np.sum(self.deltas * self.deltas, axis=1)
        else:
            self.starts = np.empty((0, 2), dtype=np.float64)
            self.ends = np.empty((0, 2), dtype=np.float64)
            self.deltas = np.empty((0, 2), dtype=np.float64)
            self.length_squared = np.empty((0,), dtype=np.float64)

    def distances(self, points: list[Point], chunk_size: int = 512) -> np.ndarray:
        if not points or len(self.starts) == 0:
            return np.full((len(points),), math.inf, dtype=np.float64)
        query = np.asarray([(point.x, point.y) for point in points], dtype=np.float64)
        result = np.empty((len(points),), dtype=np.float64)
        for offset in range(0, len(points), chunk_size):
            batch = query[offset : offset + chunk_size]
            relative = batch[:, None, :] - self.starts[None, :, :]
            parameters = np.sum(relative * self.deltas[None, :, :], axis=2)
            parameters = np.divide(
                parameters,
                self.length_squared[None, :],
                out=np.zeros_like(parameters),
                where=self.length_squared[None, :] > 0.0,
            )
            np.clip(parameters, 0.0, 1.0, out=parameters)
            closest = self.starts[None, :, :] + parameters[:, :, None] * self.deltas[None, :, :]
            differences = batch[:, None, :] - closest
            distances = np.sqrt(np.sum(differences * differences, axis=2))
            result[offset : offset + len(batch)] = np.min(distances, axis=1)
        return result


def _max_nearest_boundary_distance(
    source: list[Point],
    target_contours: list[list[Point]],
    tolerance: float,
    cancel_check: CancelCheck,
) -> float:
    if cancel_check is not None and cancel_check():
        from .errors import BusinessError, ErrorCode

        raise BusinessError(ErrorCode.CANCELLED, "evaluation cancelled")
    if not source:
        return 0.0
    distances = _BoundaryIndex(target_contours).distances(source)
    if distances.size == 0 or not np.any(np.isfinite(distances)):
        return math.inf
    return float(np.max(distances[np.isfinite(distances)]))


def evaluate_boolean_result(
    path_a: PathSpec,
    path_b: PathSpec,
    result: BooleanResult,
    *,
    tolerance: float,
    area_relative_threshold: float = 1.0e-3,
    boundary_threshold: float | None = None,
    scanline_rows: int = 256,
    cancel_check: CancelCheck = None,
) -> ErrorReport:
    flattened_a = flatten_path(path_a, tolerance)
    flattened_b = flatten_path(path_b, tolerance)
    output_contours = [list(contour.points) for contour in result.contours]
    expected_area, symmetric_area, union_area, actual_scanline_rows = _scanline_area_metrics(
        flattened_a,
        flattened_b,
        output_contours,
        result.operation,
        rows=max(32, int(scanline_rows)),
        tolerance=tolerance,
    )
    if expected_area > 0.0:
        relative_area = symmetric_area / expected_area
    else:
        relative_area = 0.0 if symmetric_area <= tolerance * tolerance else 1.0

    def boundary_for(path: FlattenedPath) -> list[list[Point]]:
        loops: list[list[Point]] = []
        for subpath in path.subpaths:
            loops.append(list(subpath.points))
        return loops

    expected_samples = _expected_boundary_samples(
        flattened_a, flattened_b, result.operation, tolerance
    )
    output_samples = _output_boundary_samples(output_contours, tolerance)
    expected_loops = boundary_for(flattened_a) + boundary_for(flattened_b)

    output_to_expected = _max_nearest_boundary_distance(
        output_samples, expected_loops, tolerance, cancel_check
    )
    expected_to_output = _max_nearest_boundary_distance(
        expected_samples, output_contours, tolerance, cancel_check
    )
    measured_boundary = max(output_to_expected, expected_to_output)
    undefined_empty_target = not math.isfinite(measured_boundary)
    if undefined_empty_target:
        all_boundary_points = [
            point
            for edge in expected_loops
            for point in edge
        ] + [point for points in output_contours for point in points]
        if all_boundary_points:
            span = math.hypot(
                max(point.x for point in all_boundary_points)
                - min(point.x for point in all_boundary_points),
                max(point.y for point in all_boundary_points)
                - min(point.y for point in all_boundary_points),
            )
        else:
            span = 1.0
        measured_boundary = max(span, 1.0)
    if not output_contours:
        measured_boundary = 0.0 if not expected_samples else measured_boundary
    flattening_bound = tolerance * 2.0
    boundary_upper = measured_boundary + flattening_bound

    if boundary_threshold is None:
        span = math.sqrt(max(union_area, tolerance * tolerance))
        boundary_threshold = max(tolerance * 50.0, span * 0.005)

    reasons: list[str] = []
    if undefined_empty_target:
        reasons.append("expected_boundary_without_output_boundary")
    if relative_area > area_relative_threshold:
        reasons.append("area_relative_deviation_exceeded")
    if boundary_upper > boundary_threshold:
        reasons.append("boundary_deviation_exceeded")
    accepted = not reasons
    report = ErrorReport(
        accepted=accepted,
        status="ok" if accepted else "degraded",
        tolerance=tolerance,
        area_symmetric_difference=symmetric_area,
        expected_area=expected_area,
        area_relative_deviation=relative_area,
        max_boundary_deviation=measured_boundary,
        boundary_deviation_upper_bound=boundary_upper,
        area_relative_threshold=area_relative_threshold,
        boundary_threshold=boundary_threshold,
        tolerance_amplification=boundary_upper / max(tolerance, EPSILON),
        scanline_rows=actual_scanline_rows,
        degradation_reasons=reasons,
        metadata={
            "expected_union_area": union_area,
            "output_signed_area": sum(
                signed_area(points) for points in output_contours
            ),
            "expected_boundary_samples": len(expected_samples),
            "output_boundary_samples": len(output_samples),
            "linearization_bound": flattening_bound,
        },
    )
    return report

