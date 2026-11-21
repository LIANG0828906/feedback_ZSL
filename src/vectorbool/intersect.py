from __future__ import annotations

import math
from collections.abc import Sequence

from .errors import ErrorCode, fail
from .geometry import (
    EPSILON,
    bezier_tangent,
    closest_point_on_segment,
    flatten_segment,
    point_segment_distance,
)
from .model import Intersection, IntersectionDiff, Point, Segment, SegmentKind


def _line_intersection(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
    tolerance: float,
) -> tuple[float, float, Point] | None:
    da = a1 - a0
    db = b1 - b0
    denominator = da.cross(db)
    delta = b0 - a0
    length_a = max(da.length(), EPSILON)
    length_b = max(db.length(), EPSILON)
    if abs(denominator) <= tolerance * max(length_a, length_b) * 1.0e-9:
        return None
    t = delta.cross(db) / denominator
    u = delta.cross(da) / denominator
    margin_t = tolerance / length_a
    margin_u = tolerance / length_b
    if -margin_t <= t <= 1.0 + margin_t and -margin_u <= u <= 1.0 + margin_u:
        return (
            min(1.0, max(0.0, t)),
            min(1.0, max(0.0, u)),
            Point(a0.x + da.x * t, a0.y + da.y * t),
        )
    return None


def _collinear_candidates(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
    tolerance: float,
) -> list[tuple[float, float, Point, str]]:
    da = a1 - a0
    length_squared = da.dot(da)
    if length_squared <= EPSILON:
        return []
    exact_band = max(tolerance * 1.0e-6, 1.0e-12)
    if point_segment_distance(b0, a0, a1) > exact_band:
        return []
    if point_segment_distance(b1, a0, a1) > exact_band:
        return []
    tb0 = (b0 - a0).dot(da) / length_squared
    tb1 = (b1 - a0).dot(da) / length_squared
    db = b1 - b0
    low = max(0.0, min(tb0, tb1))
    high = min(1.0, max(tb0, tb1))
    if high < low - tolerance / max(da.length(), EPSILON):
        return []
    if high - low <= 1.0e-14:
        t = min(1.0, max(0.0, (low + high) * 0.5))
        point = a0 + da * t
        u = (point - b0).dot(db) / max(db.dot(db), EPSILON)
        return [(t, min(1.0, max(0.0, u)), point, "tangent")]
    result: list[tuple[float, float, Point, str]] = []
    for t in (low, high):
        point = a0 + da * t
        u = (point - b0).dot(db) / max(db.dot(db), EPSILON)
        result.append(
            (min(1.0, max(0.0, t)), min(1.0, max(0.0, u)), point, "coincident")
        )
    return result


def _deduplicate_intersections(
    intersections: Sequence[Intersection], tolerance: float
) -> list[Intersection]:
    result: list[Intersection] = []
    for candidate in sorted(
        intersections,
        key=lambda item: (
            item.parameter_a,
            item.parameter_b,
            item.point.x,
            item.point.y,
            item.kind,
        ),
    ):
        duplicate = False
        for existing in result:
            position_delta = existing.point.distance_to(candidate.point)
            parameter_delta = max(
                abs(existing.parameter_a - candidate.parameter_a),
                abs(existing.parameter_b - candidate.parameter_b),
            )
            if position_delta <= tolerance and parameter_delta <= 1.0e-7:
                duplicate = True
                if candidate.kind == "coincident" and existing.kind != "coincident":
                    result[result.index(existing)] = candidate
                break
        if not duplicate:
            result.append(candidate)
    return result


def _polyline_intersections(
    segment_a: Segment,
    segment_b: Segment,
    tolerance: float,
) -> tuple[list[Intersection], list[tuple[Point, Point]]]:
    flat_a = flatten_segment(segment_a, tolerance)
    flat_b = flatten_segment(segment_b, tolerance)
    results: list[Intersection] = []
    near_pairs: list[tuple[Point, Point]] = []
    for a_index in range(len(flat_a) - 1):
        ta0, a0 = flat_a[a_index]
        ta1, a1 = flat_a[a_index + 1]
        for b_index in range(len(flat_b) - 1):
            tb0, b0 = flat_b[b_index]
            tb1, b1 = flat_b[b_index + 1]
            hit = _line_intersection(a0, a1, b0, b1, tolerance)
            if hit is not None:
                local_t, local_u, point = hit
                parameter_a = ta0 + (ta1 - ta0) * local_t
                parameter_b = tb0 + (tb1 - tb0) * local_u
                tangent_a = bezier_tangent(segment_a, parameter_a)
                tangent_b = bezier_tangent(segment_b, parameter_b)
                cross = abs(tangent_a.cross(tangent_b))
                kind = "tangent" if cross <= 1.0e-7 else "cross"
                results.append(
                    Intersection(
                        point=point,
                        parameter_a=parameter_a,
                        parameter_b=parameter_b,
                        tangent_a=tangent_a,
                        tangent_b=tangent_b,
                        kind=kind,
                        confidence="high" if segment_a.kind is SegmentKind.LINE and segment_b.kind is SegmentKind.LINE else "medium",
                        distance=0.0,
                    )
                )
                continue
            for point_a, point_b in ((a0, b0), (a0, b1), (a1, b0), (a1, b1)):
                distance = point_a.distance_to(point_b)
                if distance <= tolerance:
                    near_pairs.append((point_a, point_b))
    return _deduplicate_intersections(results, tolerance), near_pairs


def _minimum_polyline_distance(
    segment_a: Segment, segment_b: Segment, tolerance: float
) -> tuple[float, Point, Point]:
    flat_a = flatten_segment(segment_a, tolerance)
    flat_b = flatten_segment(segment_b, tolerance)
    best = (math.inf, flat_a[0][1], flat_b[0][1])
    for _, point_a in flat_a:
        for _, point_b in flat_b:
            distance = point_a.distance_to(point_b)
            if distance < best[0]:
                best = (distance, point_a, point_b)
    for _, point_a in flat_a:
        for b_index in range(len(flat_b) - 1):
            closest, _, distance = closest_point_on_segment(
                point_a, flat_b[b_index][1], flat_b[b_index + 1][1]
            )
            if distance < best[0]:
                best = (distance, point_a, closest)
    for _, point_b in flat_b:
        for a_index in range(len(flat_a) - 1):
            closest, _, distance = closest_point_on_segment(
                point_b, flat_a[a_index][1], flat_a[a_index + 1][1]
            )
            if distance < best[0]:
                best = (distance, closest, point_b)
    return best


def intersect_segments(
    segment_a: Segment,
    segment_b: Segment,
    *,
    tolerance: float,
    strict: bool = True,
) -> list[Intersection]:
    tolerance = float(tolerance)
    if not math.isfinite(tolerance) or tolerance <= 0.0:
        fail(ErrorCode.PATH_INVALID, "intersection tolerance must be positive")

    if segment_a.kind is SegmentKind.LINE and segment_b.kind is SegmentKind.LINE:
        a0, a1 = segment_a.points
        b0, b1 = segment_b.points
        hit = _line_intersection(a0, a1, b0, b1, tolerance)
        if hit is not None:
            t, u, point = hit
            tangent_a = (a1 - a0).normalized()
            tangent_b = (b1 - b0).normalized()
            cross = abs(tangent_a.cross(tangent_b))
            return [
                Intersection(
                    point=point,
                    parameter_a=t,
                    parameter_b=u,
                    tangent_a=tangent_a,
                    tangent_b=tangent_b,
                    kind="tangent" if cross <= 1.0e-7 else "cross",
                    confidence="high",
                )
            ]
        collinear = _collinear_candidates(a0, a1, b0, b1, tolerance)
        if collinear:
            return _deduplicate_intersections(
                [
                    Intersection(
                        point=point,
                        parameter_a=t,
                        parameter_b=u,
                        tangent_a=(a1 - a0).normalized(),
                        tangent_b=(b1 - b0).normalized(),
                        kind=kind,
                        confidence="high",
                    )
                    for t, u, point, kind in collinear
                ],
                tolerance,
            )
    else:
        results, near_pairs = _polyline_intersections(segment_a, segment_b, tolerance)
        if results:
            return results
        if near_pairs:
            point_a, point_b = min(
                near_pairs, key=lambda pair: pair[0].distance_to(pair[1])
            )
            point = (point_a + point_b) * 0.5
            distance = point_a.distance_to(point_b)
            if distance <= tolerance:
                return [
                    Intersection(
                        point=point,
                        parameter_a=_nearest_parameter(segment_a, point, tolerance),
                        parameter_b=_nearest_parameter(segment_b, point, tolerance),
                        tangent_a=bezier_tangent(
                            segment_a, _nearest_parameter(segment_a, point, tolerance)
                        ),
                        tangent_b=bezier_tangent(
                            segment_b, _nearest_parameter(segment_b, point, tolerance)
                        ),
                        kind="near_coincident",
                        confidence="low",
                        distance=distance,
                    )
                ]

    distance, point_a, point_b = _minimum_polyline_distance(
        segment_a, segment_b, tolerance
    )
    if distance <= tolerance:
        point = (point_a + point_b) * 0.5
        parameter_a = _nearest_parameter(segment_a, point, tolerance)
        parameter_b = _nearest_parameter(segment_b, point, tolerance)
        return [
            Intersection(
                point=point,
                parameter_a=parameter_a,
                parameter_b=parameter_b,
                tangent_a=bezier_tangent(segment_a, parameter_a),
                tangent_b=bezier_tangent(segment_b, parameter_b),
                kind="near_coincident",
                confidence="low",
                distance=distance,
            )
        ]
    if distance <= tolerance * 4.0 and strict:
        fail(
            ErrorCode.INTERSECT_AMBIGUOUS,
            "segments are closer than the decision band but no stable intersection exists",
            point=((point_a.x + point_b.x) * 0.5, (point_a.y + point_b.y) * 0.5),
            distance=distance,
            tolerance=tolerance,
        )
    return []


def _nearest_parameter(segment: Segment, point: Point, tolerance: float) -> float:
    flattened = flatten_segment(segment, tolerance)
    best_t = 0.0
    best_distance = math.inf
    for index in range(len(flattened) - 1):
        t0, start = flattened[index]
        t1, end = flattened[index + 1]
        _, local_t, distance = closest_point_on_segment(point, start, end)
        if distance < best_distance:
            best_distance = distance
            best_t = t0 + (t1 - t0) * local_t
    return best_t


def diff_intersections(
    old: Sequence[Intersection],
    new: Sequence[Intersection],
    *,
    tolerance: float,
) -> IntersectionDiff:
    unmatched_new = list(new)
    unmatched_old: list[Intersection] = []
    matched_count = 0
    max_deviation = 0.0
    match_radius = max(tolerance * 10.0, 1.0e-12)
    for old_item in old:
        if not unmatched_new:
            unmatched_old.append(old_item)
            continue
        nearest = min(
            unmatched_new,
            key=lambda item: old_item.point.distance_to(item.point),
        )
        distance = old_item.point.distance_to(nearest.point)
        if distance <= match_radius:
            unmatched_new.remove(nearest)
            matched_count += 1
            max_deviation = max(max_deviation, distance)
        else:
            unmatched_old.append(old_item)
    if not old and not new:
        summary = "unchanged-empty"
    elif not unmatched_old and not unmatched_new:
        summary = "unchanged"
    else:
        summary = (
            f"added={len(unmatched_new)} removed={len(unmatched_old)} "
            f"matched={matched_count}"
        )
    return IntersectionDiff(
        tolerance=float(tolerance),
        old_count=len(old),
        new_count=len(new),
        added=list(unmatched_new),
        removed=unmatched_old,
        matched_count=matched_count,
        max_position_deviation=max_deviation,
        summary=summary,
    )


def recompute_intersections(
    segment_a: Segment,
    segment_b: Segment,
    *,
    old_tolerance: float,
    new_tolerance: float,
) -> tuple[list[Intersection], IntersectionDiff]:
    old = intersect_segments(segment_a, segment_b, tolerance=old_tolerance)
    new = intersect_segments(segment_a, segment_b, tolerance=new_tolerance)
    return new, diff_intersections(old, new, tolerance=new_tolerance)

