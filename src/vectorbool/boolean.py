from __future__ import annotations

import itertools
import math
from collections import defaultdict
from collections.abc import Callable, Iterable
from dataclasses import dataclass

from .errors import BusinessError, ErrorCode, fail
from .geometry import (
    EPSILON,
    deduplicate_points,
    flatten_path,
    point_in_flattened_path,
    signed_area,
)
from .model import (
    BooleanResult,
    Contour,
    FlattenedPath,
    Operation,
    PathSpec,
    Point,
)

CancelCheck = Callable[[], bool] | None


@dataclass(frozen=True)
class _SourceEdge:
    start: Point
    end: Point
    path_index: int
    segment_index: int


@dataclass
class _AtomicEdge:
    start: Point
    end: Point


class _VertexSnapper:
    def __init__(self, tolerance: float) -> None:
        self.tolerance = max(float(tolerance), EPSILON)
        self.buckets: dict[tuple[int, int], list[Point]] = {}

    def _cell(self, point: Point) -> tuple[int, int]:
        return (
            math.floor(point.x / self.tolerance),
            math.floor(point.y / self.tolerance),
        )

    def snap(self, point: Point) -> Point:
        cell_x, cell_y = self._cell(point)
        candidates: list[Point] = []
        for dx in (-1, 0, 1):
            for dy in (-1, 0, 1):
                candidates.extend(self.buckets.get((cell_x + dx, cell_y + dy), ()))
        if candidates:
            nearest = min(
                candidates,
                key=lambda candidate: (
                    candidate.distance_to(point),
                    candidate.x,
                    candidate.y,
                ),
            )
            if nearest.distance_to(point) <= self.tolerance:
                return nearest
        self.buckets.setdefault((cell_x, cell_y), []).append(point)
        return point


def _line_hit(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
    tolerance: float,
) -> tuple[float, float, Point] | None:
    da = a1 - a0
    db = b1 - b0
    denominator = da.cross(db)
    if abs(denominator) <= EPSILON * max(da.length(), 1.0) * max(db.length(), 1.0):
        return None
    delta = b0 - a0
    t = delta.cross(db) / denominator
    u = delta.cross(da) / denominator
    margin_a = tolerance / max(da.length(), EPSILON)
    margin_b = tolerance / max(db.length(), EPSILON)
    if (
        -margin_a <= t <= 1.0 + margin_a
        and -margin_b <= u <= 1.0 + margin_b
    ):
        return (
            min(1.0, max(0.0, t)),
            min(1.0, max(0.0, u)),
            Point(a0.x + da.x * t, a0.y + da.y * t),
        )
    return None


def _collinear_parameters(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
    tolerance: float,
) -> tuple[list[float], list[float]] | None:
    da = a1 - a0
    db = b1 - b0
    length_a_squared = da.dot(da)
    length_b_squared = db.dot(db)
    if length_a_squared <= EPSILON or length_b_squared <= EPSILON:
        return None
    if abs(da.cross(b0 - a0)) / da.length() > tolerance:
        return None
    if abs(da.cross(b1 - a0)) / da.length() > tolerance:
        return None
    tb0 = (b0 - a0).dot(da) / length_a_squared
    tb1 = (b1 - a0).dot(da) / length_a_squared
    ua0 = (a0 - b0).dot(db) / length_b_squared
    ua1 = (a1 - b0).dot(db) / length_b_squared
    low = max(0.0, min(tb0, tb1))
    high = min(1.0, max(tb0, tb1))
    if high < low - tolerance / da.length():
        return None
    params_a = [min(1.0, max(0.0, low)), min(1.0, max(0.0, high))]
    params_b = [min(1.0, max(0.0, ua0)), min(1.0, max(0.0, ua1))]
    # The intersection's overlap endpoints in B are not necessarily ua0/ua1;
    # project the clipped overlap back into both parameter domains.
    for t in params_a:
        point = a0 + da * t
        params_b.append((point - b0).dot(db) / length_b_squared)
    for u in params_b[:2]:
        point = b0 + db * u
        params_a.append((point - a0).dot(da) / length_a_squared)
    return (
        sorted({min(1.0, max(0.0, value)) for value in params_a}),
        sorted({min(1.0, max(0.0, value)) for value in params_b}),
    )


def _flattened_sources(path: PathSpec, tolerance: float) -> tuple[FlattenedPath, list[_SourceEdge]]:
    flattened = flatten_path(path, tolerance)
    edges: list[_SourceEdge] = []
    for subpath_index, subpath in enumerate(flattened.subpaths):
        for segment_index, (start, end) in enumerate(subpath.segments()):
            if start.distance_to(end) > tolerance * 0.01:
                edges.append(_SourceEdge(start, end, subpath_index, segment_index))
    return flattened, edges


def _bbox_overlap(a: _SourceEdge, b: _SourceEdge, tolerance: float) -> bool:
    return not (
        min(a.start.x, a.end.x) > max(b.start.x, b.end.x) + tolerance
        or min(b.start.x, b.end.x) > max(a.start.x, a.end.x) + tolerance
        or min(a.start.y, a.end.y) > max(b.start.y, b.end.y) + tolerance
        or min(b.start.y, b.end.y) > max(a.start.y, a.end.y) + tolerance
    )


def _lerp_edge(edge: _SourceEdge, parameter: float) -> Point:
    delta = edge.end - edge.start
    return Point(
        edge.start.x + delta.x * parameter,
        edge.start.y + delta.y * parameter,
    )


def _desired_status(
    point: Point,
    flattened_a: FlattenedPath,
    flattened_b: FlattenedPath,
    operation: Operation,
) -> bool:
    in_a = point_in_flattened_path(point, flattened_a)
    in_b = point_in_flattened_path(point, flattened_b)
    if operation is Operation.UNION:
        return in_a or in_b
    if operation is Operation.INTERSECT:
        return in_a and in_b
    if operation is Operation.DIFFERENCE:
        return in_a and not in_b
    return in_a != in_b


def _probe_epsilon(length: float, tolerance: float) -> float:
    return min(max(tolerance * 0.25, 1.0e-10), length * 0.125)


def _kept_oriented_edges(
    atomic_edges: Iterable[_AtomicEdge],
    flattened_a: FlattenedPath,
    flattened_b: FlattenedPath,
    operation: Operation,
    tolerance: float,
    cancel_check: CancelCheck,
) -> list[_AtomicEdge]:
    kept: list[_AtomicEdge] = []
    for index, edge in enumerate(atomic_edges):
        if cancel_check is not None and index % 128 == 0 and cancel_check():
            raise BusinessError(ErrorCode.CANCELLED, "boolean task cancelled")
        delta = edge.end - edge.start
        length = delta.length()
        if length <= tolerance * 0.01:
            continue
        normal = delta.left_normal().normalized()
        epsilon = _probe_epsilon(length, tolerance)
        midpoint = (edge.start + edge.end) * 0.5
        left_point = midpoint + normal * epsilon
        right_point = midpoint - normal * epsilon
        left = _desired_status(
            left_point, flattened_a, flattened_b, operation
        )
        right = _desired_status(
            right_point, flattened_a, flattened_b, operation
        )
        if left == right:
            continue
        if left:
            kept.append(_AtomicEdge(edge.start, edge.end))
        else:
            kept.append(_AtomicEdge(edge.end, edge.start))
    return kept


def _edge_key(point: Point) -> tuple[float, float]:
    return (point.x, point.y)


def _chain_edges(
    edges: list[_AtomicEdge],
    tolerance: float,
) -> list[list[Point]]:
    outgoing: dict[tuple[float, float], list[int]] = defaultdict(list)
    for index, edge in enumerate(edges):
        outgoing[_edge_key(edge.start)].append(index)

    def turn(index: int, previous: _AtomicEdge) -> tuple[float, float, int]:
        edge = edges[index]
        incoming = previous.end - previous.start
        outgoing_vector = edge.end - edge.start
        angle = math.atan2(
            incoming.cross(outgoing_vector), incoming.dot(outgoing_vector)
        )
        if angle <= 0.0:
            angle += math.tau
        return (angle, edge.end.distance_to(edge.start), index)

    used = [False] * len(edges)
    loops: list[list[Point]] = []
    for initial_index in range(len(edges)):
        if used[initial_index]:
            continue
        initial = edges[initial_index]
        loop = [initial.start]
        current_index = initial_index
        current = initial
        closed = False
        for _ in range(len(edges) + 1):
            used[current_index] = True
            loop.append(current.end)
            if _edge_key(current.end) == _edge_key(initial.start):
                closed = True
                break
            candidates = [
                index
                for index in outgoing.get(_edge_key(current.end), ())
                if not used[index]
            ]
            if not candidates:
                break
            current_index = min(candidates, key=lambda index: turn(index, current))
            current = edges[current_index]
        if closed and len(loop) >= 3:
            loops.append(loop[:-1])
    return loops


def _interior_anchor(points: list[Point], tolerance: float) -> Point:
    x_min = min(point.x for point in points)
    x_max = max(point.x for point in points)
    y_min = min(point.y for point in points)
    y_max = max(point.y for point in points)
    scale = max(x_max - x_min, y_max - y_min, tolerance)
    area = signed_area(points)
    for fraction in (0.5, 0.25, 0.75, 0.125, 0.375, 0.625, 0.875):
        y = y_min + scale * fraction
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
                candidate = Point((left + right) * 0.5, y)
                if area > 0.0 or area < 0.0:
                    return candidate
    return Point((x_min + x_max) * 0.5, (y_min + y_max) * 0.5)


def _orient_contours(contours: list[list[Point]], tolerance: float) -> list[Contour]:
    # Boundary tracing orients the filled region to the left, so positive
    # signed area is an outer contour and negative signed area is a hole.
    # Recomputing nesting here is both redundant and vulnerable to anchors that
    # fall inside a nested hole.
    return [
        Contour(
            points=tuple(points),
            role="outer" if signed_area(points) > 0.0 else "hole",
        )
        for points in contours
    ]


def _flattened_subpath(points: list[Point]):
    from .model import FlattenedSubpath

    return FlattenedSubpath(points=list(points) + [points[0]])


def _proper_segment_intersection(
    a0: Point, a1: Point, b0: Point, b1: Point, tolerance: float
) -> Point | None:
    hit = _line_hit(a0, a1, b0, b1, tolerance * 0.01)
    if hit is None:
        return None
    t, u, point = hit
    if tolerance / max(a1.distance_to(a0), EPSILON) < t < 1.0 - tolerance / max(
        a1.distance_to(a0), EPSILON
    ) and tolerance / max(b1.distance_to(b0), EPSILON) < u < 1.0 - tolerance / max(
        b1.distance_to(b0), EPSILON
    ):
        return point
    return None


def detect_self_intersections(
    contours: list[Contour], tolerance: float
) -> list[dict[str, object]]:
    results: list[dict[str, object]] = []
    for contour_index, contour in enumerate(contours):
        edges = list(contour.segments())
        count = len(edges)
        for first in range(count):
            a0, a1 = edges[first]
            for second in range(first + 1, count):
                if second == first + 1 or (first == 0 and second == count - 1):
                    continue
                b0, b1 = edges[second]
                point = _proper_segment_intersection(
                    a0, a1, b0, b1, tolerance
                )
                if point is not None:
                    results.append(
                        {
                            "contour": contour_index,
                            "segment_a": first,
                            "segment_b": second,
                            "point": point.as_list(),
                        }
                    )
    return results


def boolean_paths(
    path_a: PathSpec,
    path_b: PathSpec,
    operation: Operation | str,
    *,
    tolerance: float,
    cancel_check: CancelCheck = None,
    reject_self_intersections: bool = True,
    evaluate_result: bool = True,
) -> BooleanResult:
    try:
        operation = Operation(operation)
    except ValueError:
        fail(ErrorCode.PATH_INVALID, "operation must be union, intersect, difference, or xor")
    if not math.isfinite(tolerance) or tolerance <= 0.0:
        fail(ErrorCode.PATH_INVALID, "geometry tolerance must be finite and positive")

    flattened_a, edges_a = _flattened_sources(path_a, tolerance)
    flattened_b, edges_b = _flattened_sources(path_b, tolerance)
    if cancel_check is not None and cancel_check():
        raise BusinessError(ErrorCode.CANCELLED, "boolean task cancelled")
    _reject_ambiguous_near_coincidence(edges_a, edges_b, tolerance)

    edges = edges_a + edges_b
    splits: list[set[float]] = [{0.0, 1.0} for _ in edges]
    intersection_points: list[Point] = []
    for first in range(len(edges)):
        if cancel_check is not None and first % 128 == 0 and cancel_check():
            raise BusinessError(ErrorCode.CANCELLED, "boolean task cancelled")
        edge_a = edges[first]
        for second in range(first + 1, len(edges)):
            edge_b = edges[second]
            if not _bbox_overlap(edge_a, edge_b, tolerance):
                continue
            hit = _line_hit(
                edge_a.start,
                edge_a.end,
                edge_b.start,
                edge_b.end,
                tolerance,
            )
            if hit is not None:
                t, u, point = hit
                splits[first].add(t)
                splits[second].add(u)
                intersection_points.append(point)
                continue
            collinear = _collinear_parameters(
                edge_a.start,
                edge_a.end,
                edge_b.start,
                edge_b.end,
                tolerance,
            )
            if collinear is not None:
                params_a, params_b = collinear
                splits[first].update(params_a)
                splits[second].update(params_b)
                for t in params_a:
                    intersection_points.append(_lerp_edge(edge_a, t))

    snapper = _VertexSnapper(tolerance)
    unique_edges: dict[tuple[tuple[float, float], tuple[float, float]], _AtomicEdge] = {}
    for edge, parameters in zip(edges, splits):
        ordered = sorted(parameters)
        for left, right in itertools.pairwise(ordered):
            if right - left <= 1.0e-14:
                continue
            start = snapper.snap(_lerp_edge(edge, left))
            end = snapper.snap(_lerp_edge(edge, right))
            if start.distance_to(end) <= tolerance * 0.01:
                continue
            key_a = _edge_key(start)
            key_b = _edge_key(end)
            key = (key_a, key_b) if key_a <= key_b else (key_b, key_a)
            if key not in unique_edges:
                unique_edges[key] = _AtomicEdge(start, end)

    atomic_edges = list(unique_edges.values())
    kept = _kept_oriented_edges(
        atomic_edges,
        flattened_a,
        flattened_b,
        operation,
        tolerance,
        cancel_check,
    )
    if cancel_check is not None and cancel_check():
        raise BusinessError(ErrorCode.CANCELLED, "boolean task cancelled")
    loops = _chain_edges(kept, tolerance)
    cleaned: list[list[Point]] = []
    for loop in loops:
        simplified = deduplicate_points(loop, tolerance * 0.01)
        if len(simplified) < 3:
            continue
        area = abs(signed_area(simplified))
        perimeter = sum(
            start.distance_to(end)
            for start, end in zip(simplified, simplified[1:] + simplified[:1])
        )
        if area <= max(tolerance * perimeter * 0.01, tolerance * tolerance, 1e-14):
            continue
        cleaned.append(simplified)
    contours = _orient_contours(cleaned, tolerance)
    self_hits = detect_self_intersections(contours, tolerance)
    if self_hits and reject_self_intersections:
        fail(
            ErrorCode.SELF_INTERSECT,
            "boolean result contains an irreducible self-intersection",
            subpath=int(self_hits[0]["contour"]),
            segment=int(self_hits[0]["segment_a"]),
            point=tuple(self_hits[0]["point"]),
            self_intersections=self_hits,
        )

    unique_intersection_count = _count_unique_points(intersection_points, tolerance)
    result = BooleanResult(
        operation=operation,
        tolerance=tolerance,
        contours=contours,
        intersection_count=unique_intersection_count,
        self_intersections=self_hits,
        preview_allowed=False,
    )
    if evaluate_result:
        from .evaluate import evaluate_boolean_result

        report = evaluate_boolean_result(
            path_a, path_b, result, tolerance=tolerance, cancel_check=cancel_check
        )
        result.report = report
        result.preview_allowed = report.accepted
    return result


def _count_unique_points(points: list[Point], tolerance: float) -> int:
    unique: list[Point] = []
    for point in points:
        if not any(point.distance_to(existing) <= tolerance for existing in unique):
            unique.append(point)
    return len(unique)


def _segment_distance(
    a0: Point, a1: Point, b0: Point, b1: Point
) -> float:
    from .geometry import point_segment_distance

    return min(
        point_segment_distance(a0, b0, b1),
        point_segment_distance(a1, b0, b1),
        point_segment_distance(b0, a0, a1),
        point_segment_distance(b1, a0, a1),
    )


def _reject_ambiguous_near_coincidence(
    edges_a: list[_SourceEdge], edges_b: list[_SourceEdge], tolerance: float
) -> None:
    for edge_a in edges_a:
        for edge_b in edges_b:
            if not _bbox_overlap(edge_a, edge_b, tolerance):
                continue
            if _line_hit(
                edge_a.start,
                edge_a.end,
                edge_b.start,
                edge_b.end,
                tolerance,
            ) is not None:
                continue
            delta_a = edge_a.end - edge_a.start
            delta_b = edge_b.end - edge_b.start
            length_a = delta_a.length()
            length_b = delta_b.length()
            if length_a <= tolerance or length_b <= tolerance:
                continue
            if abs(delta_a.normalized().cross(delta_b.normalized())) > 1.0e-5:
                continue
            unit = delta_a / length_a
            projection = sorted(
                (
                    (edge_b.start - edge_a.start).dot(unit),
                    (edge_b.end - edge_a.start).dot(unit),
                )
            )
            overlap = max(0.0, min(length_a, projection[1]) - max(0.0, projection[0]))
            if overlap <= tolerance * 2.0:
                continue
            distance = _segment_distance(
                edge_a.start, edge_a.end, edge_b.start, edge_b.end
            )
            if tolerance * 1.0e-6 < distance <= tolerance:
                fail(
                    ErrorCode.INTERSECT_AMBIGUOUS,
                    "near-coincident edges fall inside the decision band",
                    path="b",
                    subpath=edge_b.path_index,
                    segment=edge_b.segment_index,
                    point=(
                        (edge_a.start.x + edge_b.start.x) * 0.5,
                        (edge_a.start.y + edge_b.start.y) * 0.5,
                    ),
                    distance=distance,
                    tolerance=tolerance,
                )

