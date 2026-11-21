from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Callable, Iterable, Sequence

from .compat import derive_vertex_shares
from .errors import E_BOOLEAN_FAILED, E_SELF_INTERSECT, PipelineError
from .geometry import (
    FlatEdge,
    PreparedPath,
    contour_polyline,
    flatten_path,
    point_on_segment,
    polygon_signed_area,
    prepare_path,
)
from .model import Contour, EdgeTrace, PathData, Point, Segment


OPERATIONS = {"union", "intersection", "difference", "xor"}


@dataclass(frozen=True, slots=True)
class BooleanOptions:
    operation: str
    tolerance: float
    flatten_ratio: float = 0.25
    cancel_check: Callable[[], None] | None = None

    def __post_init__(self) -> None:
        if self.operation not in OPERATIONS:
            raise ValueError(f"unknown boolean operation: {self.operation}")
        if not math.isfinite(self.tolerance) or self.tolerance <= 0.0:
            raise ValueError("tolerance must be finite and positive")


@dataclass
class WorkEdge:
    a_index: int
    b_index: int
    a: Point
    b: Point
    t_start: float
    t_end: float
    traces: tuple[EdgeTrace, ...]
    confidence: float = 1.0

    @property
    def key(self) -> tuple[int, int]:
        return (self.a_index, self.b_index)

    @property
    def undirected_key(self) -> tuple[int, int]:
        return (
            min(self.a_index, self.b_index),
            max(self.a_index, self.b_index),
        )

    @property
    def orientation(self) -> int:
        return 1 if self.a_index < self.b_index else -1

    @property
    def midpoint(self) -> Point:
        return self.a.lerp(self.b, 0.5)

    def reversed(self) -> "WorkEdge":
        return WorkEdge(
            self.b_index,
            self.a_index,
            self.b,
            self.a,
            self.t_end,
            self.t_start,
            self.traces,
            self.confidence,
        )


@dataclass
class BooleanResult:
    path: PathData
    report: dict[str, Any]


class _PointRegistry:
    def __init__(self, tolerance: float) -> None:
        self.tolerance = max(tolerance, 1e-10)
        self.points: list[Point] = []
        self._cells: dict[tuple[int, int], list[int]] = {}

    def _cell(self, point: Point) -> tuple[int, int]:
        return (
            math.floor(point.x / self.tolerance),
            math.floor(point.y / self.tolerance),
        )

    def add(self, point: Point) -> int:
        cx, cy = self._cell(point)
        for dx in (-1, 0, 1):
            for dy in (-1, 0, 1):
                for index in self._cells.get((cx + dx, cy + dy), ()):
                    if self.points[index].distance_to(point) <= self.tolerance:
                        return index
        index = len(self.points)
        self.points.append(point)
        self._cells.setdefault((cx, cy), []).append(index)
        return index


def _operation_value(
    operation: str,
    in_a: bool,
    in_b: bool,
) -> bool:
    if operation == "union":
        return in_a or in_b
    if operation == "intersection":
        return in_a and in_b
    if operation == "difference":
        return in_a and not in_b
    return in_a != in_b


def _candidate_edge_pairs(
    edges: Sequence[FlatEdge],
    tolerance: float,
) -> list[tuple[int, int]]:
    if len(edges) < 2:
        return []
    min_x = min(min(edge.a.x, edge.b.x) for edge in edges)
    max_x = max(max(edge.a.x, edge.b.x) for edge in edges)
    min_y = min(min(edge.a.y, edge.b.y) for edge in edges)
    max_y = max(max(edge.a.y, edge.b.y) for edge in edges)
    extent = math.hypot(max_x - min_x, max_y - min_y)
    cell_size = max(tolerance * 4.0, extent / 128.0, 1e-9)
    buckets: dict[tuple[int, int], list[int]] = {}
    for index, edge in enumerate(edges):
        low_x = math.floor((min(edge.a.x, edge.b.x) - tolerance) / cell_size)
        high_x = math.floor((max(edge.a.x, edge.b.x) + tolerance) / cell_size)
        low_y = math.floor((min(edge.a.y, edge.b.y) - tolerance) / cell_size)
        high_y = math.floor((max(edge.a.y, edge.b.y) + tolerance) / cell_size)
        for cx in range(low_x, high_x + 1):
            for cy in range(low_y, high_y + 1):
                buckets.setdefault((cx, cy), []).append(index)
    count = len(edges)
    pair_keys: set[int] = set()
    for bucket_indices in buckets.values():
        for position, first in enumerate(bucket_indices):
            for second in bucket_indices[position + 1 :]:
                if first > second:
                    first, second = second, first
                pair_keys.add(first * count + second)
    return sorted((key // count, key % count) for key in pair_keys)


def _line_intersection_parameters(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
) -> tuple[float, float] | None:
    da = a1 - a0
    db = b1 - b0
    denominator = da.cross(db)
    if abs(denominator) <= 1e-15 * max(da.norm(), db.norm(), 1.0):
        return None
    offset = b0 - a0
    ta = offset.cross(db) / denominator
    tb = offset.cross(da) / denominator
    if -1e-12 <= ta <= 1.0 + 1e-12 and -1e-12 <= tb <= 1.0 + 1e-12:
        return min(1.0, max(0.0, ta)), min(1.0, max(0.0, tb))
    return None


def _project_parameter(point: Point, a: Point, b: Point) -> float:
    direction = b - a
    length_sq = direction.dot(direction)
    if length_sq <= 1e-24:
        return 0.0
    return min(1.0, max(0.0, (point - a).dot(direction) / length_sq))


def _near_collinear(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
    tolerance: float,
) -> bool:
    da = a1 - a0
    db = b1 - b0
    len_a = da.norm()
    len_b = db.norm()
    if len_a <= 1e-14 or len_b <= 1e-14:
        return False
    if abs(da.normalized().cross(db.normalized())) > 1e-5:
        return False
    for point in (b0, b1):
        t = _project_parameter(point, a0, a1)
        projected = a0.lerp(a1, t)
        if projected.distance_to(point) > tolerance:
            return False
    return True


def _collinear_split_parameters(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
    tolerance: float,
) -> tuple[float, ...]:
    if not _near_collinear(a0, a1, b0, b1, tolerance):
        return ()
    da = a1 - a0
    length_sq = da.dot(da)
    if length_sq <= 1e-24:
        return ()
    b_parameters = [
        (b0 - a0).dot(da) / length_sq,
        (b1 - a0).dot(da) / length_sq,
    ]
    low = max(0.0, min(b_parameters))
    high = min(1.0, max(b_parameters))
    if high < low - 1e-12:
        return ()
    result: list[float] = []
    for value in (low, high):
        if 1e-10 < value < 1.0 - 1e-10:
            result.append(value)
    return tuple(result)


def _map_trace_fraction(
    trace: EdgeTrace,
    start_fraction: float,
    end_fraction: float,
) -> EdgeTrace:
    span = trace.t_end - trace.t_start
    return EdgeTrace(
        operand=trace.operand,
        contour_index=trace.contour_index,
        contour_id=trace.contour_id,
        segment_index=trace.segment_index,
        subsegment_index=trace.subsegment_index,
        t_start=trace.t_start + span * start_fraction,
        t_end=trace.t_start + span * end_fraction,
        source_kind=trace.source_kind,
    )


def _probe_inside(
    prepared: PreparedPath,
    point: Point,
    tolerance: float,
) -> tuple[bool, bool, float]:
    boundary_tolerance = max(1e-12, tolerance * 0.02)
    inside, boundary = prepared.membership(point, boundary_tolerance)
    return inside, boundary, boundary_tolerance


def _truth_at(
    prepared_a: PreparedPath,
    prepared_b: PreparedPath,
    operation: str,
    point: Point,
    tolerance: float,
) -> tuple[bool, bool]:
    in_a, boundary_a, _ = _probe_inside(prepared_a, point, tolerance)
    in_b, boundary_b, _ = _probe_inside(prepared_b, point, tolerance)
    unresolved = boundary_a or boundary_b
    return _operation_value(operation, in_a, in_b), unresolved


def _probe_side_truth(
    prepared_a: PreparedPath,
    prepared_b: PreparedPath,
    operation: str,
    midpoint: Point,
    normal: Point,
    tolerance: float,
    edge_length: float,
) -> tuple[bool, bool, float]:
    base = max(tolerance * 0.35, 1e-9)
    maximum = min(max(tolerance * 8.0, base), max(edge_length * 0.2, base))
    unresolved = False
    last_truth = False
    last_distance = 0.0
    probes = 0
    distance = base
    while True:
        probes += 1
        point = midpoint + normal * distance
        truth, boundary = _truth_at(
            prepared_a,
            prepared_b,
            operation,
            point,
            tolerance,
        )
        last_truth = truth
        last_distance = distance
        if not boundary:
            return truth, False, min(1.0, distance / max(tolerance, 1e-12))
        if distance >= maximum or probes >= 8:
            unresolved = True
            break
        distance = min(maximum, distance * 1.8)
    return last_truth, unresolved, last_distance


def _choose_next_edge(
    candidates: Sequence[WorkEdge],
    incoming: WorkEdge | None,
) -> WorkEdge:
    if len(candidates) == 1 or incoming is None:
        return candidates[0]
    incoming_direction = (incoming.b - incoming.a).normalized()

    def score(edge: WorkEdge) -> tuple[float, float]:
        direction = (edge.b - edge.a).normalized()
        turn = math.atan2(
            incoming_direction.cross(direction),
            incoming_direction.dot(direction),
        )
        return (turn, edge.b_index)

    return min(candidates, key=score)


def _assemble_loops(
    edges: Sequence[WorkEdge],
    points: Sequence[Point] | None = None,
) -> tuple[list[list[WorkEdge]], list[WorkEdge], dict[str, Any]]:
    from collections import Counter

    outgoing: dict[int, list[int]] = {}
    out_degree = Counter(edge.a_index for edge in edges)
    in_degree = Counter(edge.b_index for edge in edges)
    degree_nodes = set(out_degree) | set(in_degree)
    diagnostics = {
        "out_degree_histogram": dict(Counter(out_degree.values())),
        "in_degree_histogram": dict(Counter(in_degree.values())),
        "degree_mismatch_nodes": sum(
            1 for node in degree_nodes if out_degree[node] != in_degree[node]
        ),
        "multi_out_nodes": sum(1 for value in out_degree.values() if value > 1),
    }
    if points is not None:
        diagnostics["mismatch_nodes"] = [
            {
                "node": node,
                "point": points[node].as_list(),
                "out_degree": out_degree[node],
                "in_degree": in_degree[node],
                "incoming": [
                    {
                        "from": edge.a_index,
                        "from_point": edge.a.as_list(),
                        "traces": [trace.to_dict() for trace in edge.traces],
                    }
                    for edge in edges
                    if edge.b_index == node
                ][:3],
                "outgoing": [
                    {
                        "to": edge.b_index,
                        "to_point": edge.b.as_list(),
                        "traces": [trace.to_dict() for trace in edge.traces],
                    }
                    for edge in edges
                    if edge.a_index == node
                ][:3],
            }
            for node in sorted(degree_nodes)
            if out_degree[node] != in_degree[node]
        ][:12]
    for index, edge in enumerate(edges):
        outgoing.setdefault(edge.a_index, []).append(index)
    used = [False] * len(edges)
    loops: list[list[WorkEdge]] = []
    leftovers: list[WorkEdge] = []

    for start_index in range(len(edges)):
        if used[start_index]:
            continue
        loop: list[WorkEdge] = []
        current_index = start_index
        start_node = edges[start_index].a_index
        guard = 0
        while True:
            if used[current_index]:
                leftovers.extend(edge for i, edge in enumerate(edges) if not used[i])
                return loops, leftovers, diagnostics
            current = edges[current_index]
            used[current_index] = True
            loop.append(current)
            if current.b_index == start_node:
                break
            candidate_indices = [
                index
                for index in outgoing.get(current.b_index, ())
                if not used[index]
            ]
            if not candidate_indices:
                leftovers.extend(edge for i, edge in enumerate(edges) if not used[i])
                return loops, leftovers, diagnostics
            candidates = [edges[index] for index in candidate_indices]
            chosen = _choose_next_edge(candidates, current)
            current_index = candidate_indices[candidates.index(chosen)]
            guard += 1
            if guard > len(edges) + 1:
                leftovers.extend(edge for i, edge in enumerate(edges) if not used[i])
                return loops, leftovers, diagnostics
        loops.append(loop)
    return loops, leftovers, diagnostics


def _loop_polyline(loop: Sequence[WorkEdge]) -> list[Point]:
    if not loop:
        return []
    points = [loop[0].a]
    points.extend(edge.b for edge in loop)
    return points


def _loop_depth(
    polyline: Sequence[Point],
    all_loops: Sequence[Sequence[WorkEdge]],
    target_index: int,
    tolerance: float,
) -> int:
    from .geometry import crossing_number, polygon_signed_area

    if len(polyline) < 4:
        return 0
    area = polygon_signed_area(polyline)
    diagonal = math.hypot(
        max(p.x for p in polyline) - min(p.x for p in polyline),
        max(p.y for p in polyline) - min(p.y for p in polyline),
    )
    sample = None
    for a, b in zip(polyline, polyline[1:]):
        direction = (b - a).normalized()
        normal = Point(-direction.y, direction.x)
        if area < 0:
            normal = Point(-normal.x, -normal.y)
        candidate = a.lerp(b, 0.5) + normal * max(diagonal * 1e-4, tolerance * 2.0)
        if not any(
            point_on_segment(candidate, p, q, tolerance * 0.01)
            for p, q in zip(polyline, polyline[1:])
        ):
            sample = candidate
            break
    sample = sample or polyline[0]
    depth = 0
    for other_index, other in enumerate(all_loops):
        if other_index == target_index:
            continue
        other_polyline = _loop_polyline(other)
        if crossing_number(other_polyline, sample) == 1:
            depth += 1
    return depth


def check_self_intersections(path: PathData, tolerance: float) -> list[dict[str, Any]]:
    from .geometry import flatten_path

    edges = flatten_path(path, max(1e-9, tolerance * 0.25), "result")
    by_contour: dict[int, list[FlatEdge]] = {}
    for edge in edges:
        by_contour.setdefault(edge.trace.contour_index, []).append(edge)
    intersections: list[dict[str, Any]] = []
    flat_list = list(edges)
    for first_index in range(len(flat_list)):
        first = flat_list[first_index]
        for second_index in range(first_index + 1, len(flat_list)):
            second = flat_list[second_index]
            same_contour = (
                first.trace.contour_index == second.trace.contour_index
            )
            if same_contour:
                adjacent = abs(
                    first.trace.segment_index - second.trace.segment_index
                ) <= 1
                if adjacent and (
                    first.a.distance_to(second.a) <= tolerance
                    or first.a.distance_to(second.b) <= tolerance
                    or first.b.distance_to(second.a) <= tolerance
                    or first.b.distance_to(second.b) <= tolerance
                ):
                    continue
            pair = _line_intersection_parameters(
                first.a,
                first.b,
                second.a,
                second.b,
            )
            if pair is None:
                if _near_collinear(
                    first.a,
                    first.b,
                    second.a,
                    second.b,
                    tolerance,
                ):
                    parameters = _collinear_split_parameters(
                        first.a,
                        first.b,
                        second.a,
                        second.b,
                        tolerance,
                    )
                    if parameters:
                        intersections.append(
                            {
                                "point": first.a.lerp(first.b, parameters[0]).as_list(),
                                "kind": "overlap",
                                "first": first.trace.to_dict(),
                                "second": second.trace.to_dict(),
                            }
                        )
                continue
            t1, t2 = pair
            endpoint_tolerance = 1e-8
            if (
                endpoint_tolerance < t1 < 1.0 - endpoint_tolerance
                and endpoint_tolerance < t2 < 1.0 - endpoint_tolerance
            ):
                point = first.a.lerp(first.b, t1)
                intersections.append(
                    {
                        "point": point.as_list(),
                        "first": first.trace.to_dict(),
                        "second": second.trace.to_dict(),
                    }
                )
    return intersections


def boolean_paths(
    path_a: PathData,
    path_b: PathData,
    options: BooleanOptions,
) -> BooleanResult:
    tolerance = options.tolerance
    cancel = options.cancel_check or (lambda: None)
    flatten_error = max(1e-10, tolerance * options.flatten_ratio)
    prepared_a = prepare_path(path_a, tolerance)
    prepared_b = prepare_path(path_b, tolerance)
    flat_a = flatten_path(
        path_a,
        flatten_error,
        "a",
        min_edge_length=max(1e-13, tolerance * 1e-6),
    )
    flat_b = flatten_path(
        path_b,
        flatten_error,
        "b",
        min_edge_length=max(1e-13, tolerance * 1e-6),
    )
    initial = list(flat_a) + list(flat_b)
    registry = _PointRegistry(tolerance)
    edge_nodes: list[tuple[int, int]] = []
    for edge in initial:
        edge_nodes.append((registry.add(edge.a), registry.add(edge.b)))

    split_points: list[list[tuple[float, int]]] = [[] for _ in initial]
    pair_checks = 0
    intersection_count = 0
    for pair_index, (first_index, second_index) in enumerate(
        _candidate_edge_pairs(initial, tolerance)
    ):
        first = initial[first_index]
        second = initial[second_index]
        pair_checks += 1
        if pair_index % 64 == 0:
            cancel()
        pair = _line_intersection_parameters(
            first.a,
            first.b,
            second.a,
            second.b,
        )
        if pair is not None:
            local_first, local_second = pair
            t_first = first.t_start + (
                first.t_end - first.t_start
            ) * local_first
            t_second = second.t_start + (
                second.t_end - second.t_start
            ) * local_second
            if (
                first.t_start + 1e-10
                < t_first
                < first.t_end - 1e-10
            ):
                point = first.a.lerp(first.b, local_first)
                node = registry.add(point)
                split_points[first_index].append((t_first, node))
                intersection_count += 1
            if (
                second.t_start + 1e-10
                < t_second
                < second.t_end - 1e-10
            ):
                point = second.a.lerp(second.b, local_second)
                node = registry.add(point)
                split_points[second_index].append((t_second, node))
                intersection_count += 1
            continue
        for local_first in _collinear_split_parameters(
            first.a,
            first.b,
            second.a,
            second.b,
            tolerance,
        ):
            global_first = first.t_start + (
                first.t_end - first.t_start
            ) * local_first
            point = first.a.lerp(first.b, local_first)
            node = registry.add(point)
            split_points[first_index].append((global_first, node))
        for local_second in _collinear_split_parameters(
            second.a,
            second.b,
            first.a,
            first.b,
            tolerance,
        ):
            global_second = second.t_start + (
                second.t_end - second.t_start
            ) * local_second
            point = second.a.lerp(second.b, local_second)
            node = registry.add(point)
            split_points[second_index].append((global_second, node))

    split_edges: list[WorkEdge] = []
    for edge_index, flat_edge in enumerate(initial):
        start_node, end_node = edge_nodes[edge_index]
        points = [(flat_edge.t_start, start_node), (flat_edge.t_end, end_node)]
        points.extend(split_points[edge_index])
        points.sort(key=lambda item: item[0])
        filtered: list[tuple[float, int]] = []
        for t, node in points:
            if not filtered:
                filtered.append((t, node))
                continue
            previous_t, previous_node = filtered[-1]
            if previous_node == node and abs(t - previous_t) <= 1e-12:
                continue
            if t - previous_t <= 1e-12:
                continue
            filtered.append((t, node))
        span = flat_edge.t_end - flat_edge.t_start
        for (t0, node0), (t1, node1) in zip(filtered, filtered[1:]):
            if node0 == node1 or t1 <= t0:
                continue
            fraction0 = (t0 - flat_edge.t_start) / span if span > 0 else 0.0
            fraction1 = (t1 - flat_edge.t_start) / span if span > 0 else 1.0
            traces = tuple(
                _map_trace_fraction(trace, fraction0, fraction1)
                for trace in (
                    (flat_edge.trace,) if isinstance(flat_edge.trace, EdgeTrace) else flat_edge.trace
                )
            )
            split_edges.append(
                WorkEdge(
                    node0,
                    node1,
                    registry.points[node0],
                    registry.points[node1],
                    t0,
                    t1,
                    traces,
                )
            )

    kept: list[WorkEdge] = []
    unresolved_probes = 0
    for index, edge in enumerate(split_edges):
        if index % 32 == 0:
            cancel()
        normal = edge.midpoint
        direction = (edge.b - edge.a).normalized()
        left_normal = Point(-direction.y, direction.x)
        left_truth, left_unresolved, left_confidence = _probe_side_truth(
            prepared_a,
            prepared_b,
            options.operation,
            normal,
            left_normal,
            tolerance,
            edge.a.distance_to(edge.b),
        )
        right_truth, right_unresolved, right_confidence = _probe_side_truth(
            prepared_a,
            prepared_b,
            options.operation,
            normal,
            Point(-left_normal.x, -left_normal.y),
            tolerance,
            edge.a.distance_to(edge.b),
        )
        if left_unresolved or right_unresolved:
            unresolved_probes += 1
        if left_truth == right_truth:
            continue
        confidence = min(left_confidence, right_confidence)
        if left_truth and not right_truth:
            kept.append(
                WorkEdge(
                    edge.a_index,
                    edge.b_index,
                    edge.a,
                    edge.b,
                    edge.t_start,
                    edge.t_end,
                    edge.traces,
                    confidence,
                )
            )
        else:
            kept.append(
                WorkEdge(
                    edge.b_index,
                    edge.a_index,
                    edge.b,
                    edge.a,
                    edge.t_end,
                    edge.t_start,
                    edge.traces,
                    confidence,
                )
            )

    deduplicated: list[WorkEdge] = []
    grouped: dict[tuple[int, int], list[WorkEdge]] = {}
    for edge in kept:
        grouped.setdefault(edge.undirected_key, []).append(edge)
    cancelled_duplicates = 0
    for group in grouped.values():
        orientations = {edge.orientation for edge in group}
        if len(orientations) > 1:
            cancelled_duplicates += len(group)
            continue
        representative = group[0]
        traces: list[EdgeTrace] = []
        seen: set[tuple[Any, ...]] = set()
        for edge in group:
            for trace in edge.traces:
                key = (
                    trace.operand,
                    trace.contour_index,
                    trace.segment_index,
                    trace.subsegment_index,
                    round(trace.t_start, 12),
                    round(trace.t_end, 12),
                )
                if key not in seen:
                    seen.add(key)
                    traces.append(trace)
        deduplicated.append(
            WorkEdge(
                representative.a_index,
                representative.b_index,
                representative.a,
                representative.b,
                representative.t_start,
                representative.t_end,
                tuple(traces),
                max(edge.confidence for edge in group),
            )
        )

    loops, leftovers, loop_diagnostics = _assemble_loops(
        deduplicated,
        registry.points,
    )
    if leftovers:
        mismatch_node_ids = {
            int(item["node"])
            for item in loop_diagnostics.get("mismatch_nodes", [])
        }
        raise PipelineError(
            E_BOOLEAN_FAILED,
            "kept edges could not be assembled into closed contours",
            leftover_edges=len(leftovers),
            kept_edges=len(kept),
            deduplicated_edges=len(deduplicated),
            split_edges=len(split_edges),
            split_intersections=intersection_count,
            unresolved_side_probes=unresolved_probes,
            loop_diagnostics=loop_diagnostics,
            debug_split_edges=[
                {
                    "a": edge.a_index,
                    "b": edge.b_index,
                    "a_point": edge.a.as_list(),
                    "b_point": edge.b.as_list(),
                }
                for edge in split_edges
                if edge.a_index in mismatch_node_ids or edge.b_index in mismatch_node_ids
            ][:24],
            debug_kept_edges=[
                {
                    "a": edge.a_index,
                    "b": edge.b_index,
                }
                for edge in deduplicated
                if edge.a_index in mismatch_node_ids or edge.b_index in mismatch_node_ids
            ][:24],
            operation=options.operation,
        )

    loop_polylines = [_loop_polyline(loop) for loop in loops]
    real_indices = [
        index
        for index, polyline in enumerate(loop_polylines)
        if abs(polygon_signed_area(polyline)) > max(tolerance * tolerance * 4.0, 1e-14)
    ]
    real_loops = [loops[index] for index in real_indices]
    real_polylines = [loop_polylines[index] for index in real_indices]

    contours: list[Contour] = []
    groups: list[str] = ["boolean-result"]
    for loop_index, loop in enumerate(real_loops):
        polyline = real_polylines[loop_index]
        depth = _loop_depth(polyline, real_loops, loop_index, tolerance)
        expected_direction = "ccw" if depth % 2 == 0 else "cw"
        area = polygon_signed_area(polyline)
        oriented_loop = loop
        if (area >= 0.0) != (expected_direction == "ccw"):
            oriented_loop = [
                WorkEdge(
                    edge.b_index,
                    edge.a_index,
                    edge.b,
                    edge.a,
                    edge.t_end,
                    edge.t_start,
                    edge.traces,
                    edge.confidence,
                )
                for edge in reversed(loop)
            ]
            polyline = list(reversed(polyline))
        segments = [
            Segment(
                "line",
                edge.a,
                edge.b,
                trace=edge.traces,
            )
            for edge in oriented_loop
        ]
        contour = Contour(
            segments=segments,
            contour_id=f"result-{loop_index}",
            direction=expected_direction,
            group="boolean-result",
            direction_source="boolean_depth",
            metadata={
                "nesting_depth": depth,
                "loop_edge_count": len(oriented_loop),
                "min_edge_confidence": min(
                    (edge.confidence for edge in oriented_loop),
                    default=1.0,
                ),
            },
        )
        contours.append(contour)

    result_path = PathData(
        version=2,
        fill_rule="nonzero",
        tolerance=tolerance,
        contours=contours,
        tolerance_source="boolean_operation",
        groups=groups,
        vertex_shares=[],
        metadata={
            "operation": options.operation,
            "empty_result": not contours,
            "source_tolerances": [path_a.tolerance, path_b.tolerance],
        },
    )
    result_path.vertex_shares = derive_vertex_shares(
        result_path,
        "boolean_coordinate_derivation",
    )
    self_intersections = check_self_intersections(result_path, tolerance)
    report = {
        "operation": options.operation,
        "tolerance": tolerance,
        "flatten_error": flatten_error,
        "input_edges": len(initial),
        "pair_checks": pair_checks,
        "split_intersections": intersection_count,
        "split_edges": len(split_edges),
        "kept_edges": len(kept),
        "deduplicated_edges": len(deduplicated),
        "cancelled_duplicate_edges": cancelled_duplicates,
        "unresolved_side_probes": unresolved_probes,
        "loop_count": len(contours),
        "leftover_edges": len(leftovers),
        "self_intersections": self_intersections,
        "area": sum(
            polygon_signed_area(polyline) for polyline in real_polylines
        ),
    }
    if leftovers:
        report["warning"] = "open leftover edges were excluded"
    if self_intersections:
        raise PipelineError(
            E_SELF_INTERSECT,
            "boolean output contains unresolved self-intersections",
            operation=options.operation,
            self_intersections=self_intersections,
            report=report,
        )
    return BooleanResult(result_path, report)

