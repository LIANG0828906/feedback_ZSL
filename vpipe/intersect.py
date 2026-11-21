from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any

from .geometry import closest_points_between_segments, flatten_segment, segment_tangent
from .model import Point, Segment


ANGLE_EPS = 1e-3
TANGENT_ANGLE_LIMIT = 0.035


@dataclass(frozen=True, slots=True)
class IntersectionHit:
    point: Point
    t1: float
    t2: float
    tangent1: Point
    tangent2: Point
    angle: float
    kind: str
    confidence: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "point": self.point.as_list(),
            "t1": self.t1,
            "t2": self.t2,
            "tangent1": self.tangent1.as_list(),
            "tangent2": self.tangent2.as_list(),
            "angle_radians": self.angle,
            "kind": self.kind,
            "confidence": self.confidence,
        }


@dataclass
class IntersectionResult:
    status: str
    tolerance: float
    hits: list[IntersectionHit] = field(default_factory=list)
    min_distance: float = math.inf
    reason: str = ""
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "status": self.status,
            "tolerance": self.tolerance,
            "hits": [hit.to_dict() for hit in self.hits],
            "hit_count": len(self.hits),
            "min_distance": self.min_distance,
            "reason": self.reason,
            "details": self.details,
        }


def _line_intersection(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
) -> tuple[float, float, Point] | None:
    da = a1 - a0
    db = b1 - b0
    denominator = da.cross(db)
    scale = max(da.norm(), db.norm(), 0.0)
    if abs(denominator) <= 1e-15 * max(scale, 1.0):
        return None
    offset = b0 - a0
    t = offset.cross(db) / denominator
    u = offset.cross(da) / denominator
    if -1e-12 <= t <= 1.0 + 1e-12 and -1e-12 <= u <= 1.0 + 1e-12:
        t = min(1.0, max(0.0, t))
        u = min(1.0, max(0.0, u))
        return t, u, a0.lerp(a1, t)
    return None


def _projection_parameter(point: Point, a: Point, b: Point) -> float:
    direction = b - a
    length_sq = direction.dot(direction)
    if length_sq <= 1e-24:
        return 0.0
    return min(1.0, max(0.0, (point - a).dot(direction) / length_sq))


def _point_segment_distance(point: Point, a: Point, b: Point) -> float:
    t = _projection_parameter(point, a, b)
    return point.distance_to(a.lerp(b, t))


def _is_collinear_near(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
    tolerance: float,
) -> bool:
    da = a1 - a0
    db = b1 - b0
    if da.norm() <= 1e-14 or db.norm() <= 1e-14:
        return False
    sin_angle = abs(da.normalized().cross(db.normalized()))
    if sin_angle > max(ANGLE_EPS, tolerance / max(da.norm(), db.norm(), 1e-12)):
        return False
    return (
        _point_segment_distance(b0, a0, a1) <= tolerance
        and _point_segment_distance(b1, a0, a1) <= tolerance
    )


def _overlap_parameters(
    a0: Point,
    a1: Point,
    b0: Point,
    b1: Point,
) -> tuple[float, float] | None:
    direction = a1 - a0
    if direction.dot(direction) <= 1e-24:
        return None
    u0 = (b0 - a0).dot(direction) / direction.dot(direction)
    u1 = (b1 - a0).dot(direction) / direction.dot(direction)
    low = max(0.0, min(u0, u1))
    high = min(1.0, max(u0, u1))
    if high + 1e-12 < low:
        return None
    return low, high


def _tangent_confidence(distance: float, tolerance: float, angle: float) -> float:
    if tolerance <= 0.0:
        return 1.0
    distance_score = max(0.0, 1.0 - distance / tolerance)
    angle_score = max(0.0, 1.0 - angle / TANGENT_ANGLE_LIMIT)
    return min(1.0, 0.55 * distance_score + 0.45 * angle_score)


def intersect_segments(
    first: Segment,
    second: Segment,
    tolerance: float,
) -> IntersectionResult:
    if not math.isfinite(tolerance) or tolerance <= 0.0:
        raise ValueError("tolerance must be finite and positive")

    flatten_error = max(1e-10, tolerance * 0.125)
    fa = flatten_segment(first, flatten_error)
    fb = flatten_segment(second, flatten_error)
    edges_a = list(zip(fa, fa[1:]))
    edges_b = list(zip(fb, fb[1:]))

    raw_hits: list[tuple[float, float, Point, str, float]] = []
    edge_min_distance = math.inf
    nearest_pair = (0.0, 0.0)
    coincident_overlaps: list[tuple[float, float, float, float]] = []

    for va0, va1 in edges_a:
        for vb0, vb1 in edges_b:
            dt_a, dt_b, distance = closest_points_between_segments(
                va0.point,
                va1.point,
                vb0.point,
                vb1.point,
            )
            if distance < edge_min_distance:
                edge_min_distance = distance
                t1 = va0.t + (va1.t - va0.t) * dt_a
                t2 = vb0.t + (vb1.t - vb0.t) * dt_b
                nearest_pair = (t1, t2)

            intersection = _line_intersection(
                va0.point,
                va1.point,
                vb0.point,
                vb1.point,
            )
            if intersection is not None:
                u1, u2, point = intersection
                t1 = va0.t + (va1.t - va0.t) * u1
                t2 = vb0.t + (vb1.t - vb0.t) * u2
                raw_hits.append((t1, t2, point, "intersect", 1.0))
                continue

            if _is_collinear_near(
                va0.point,
                va1.point,
                vb0.point,
                vb1.point,
                tolerance,
            ):
                overlap = _overlap_parameters(
                    va0.point,
                    va1.point,
                    vb0.point,
                    vb1.point,
                )
                if overlap is not None and overlap[1] - overlap[0] > 1e-14:
                    coincident_overlaps.append(
                        (
                            va0.t,
                            va1.t,
                            overlap[0],
                            overlap[1],
                        )
                    )

    hits: list[IntersectionHit] = []
    for t1, t2, point, kind, _ in raw_hits:
        tangent1 = segment_tangent(first, t1)
        tangent2 = segment_tangent(second, t2)
        angle = math.acos(max(-1.0, min(1.0, abs(tangent1.dot(tangent2)))))
        classification = "intersect" if angle > ANGLE_EPS else "tangent"
        confidence = max(0.2, min(1.0, angle / max(ANGLE_EPS, TANGENT_ANGLE_LIMIT)))
        if classification == "tangent":
            confidence = _tangent_confidence(0.0, tolerance, angle)
        hits.append(
            IntersectionHit(
                point,
                t1,
                t2,
                tangent1,
                tangent2,
                angle,
                classification,
                confidence,
            )
        )

    if coincident_overlaps:
        longest = max(
            coincident_overlaps,
            key=lambda item: (item[1] - item[0]) * (item[3] - item[2]),
        )
        ta0, ta1, u0, u1 = longest
        t1 = ta0 + (ta1 - ta0) * ((u0 + u1) * 0.5)
        point = first.point_at(t1)
        tangent1 = segment_tangent(first, t1)
        tangent2 = segment_tangent(second, min(1.0, max(0.0, (u0 + u1) * 0.5)))
        angle = math.acos(max(-1.0, min(1.0, abs(tangent1.dot(tangent2)))))
        overlap_length = (ta1 - ta0) * abs(u1 - u0)
        hits.append(
            IntersectionHit(
                point,
                t1,
                min(1.0, max(0.0, (u0 + u1) * 0.5)),
                tangent1,
                tangent2,
                angle,
                "coincident",
                min(1.0, max(0.0, overlap_length / max(tolerance, 1e-12))),
            )
        )

    hits = _deduplicate_hits(hits, tolerance)

    if any(hit.kind == "coincident" for hit in hits):
        status = "coincident"
        reason = "segments overlap collinearly within tolerance"
    elif any(hit.kind == "intersect" for hit in hits):
        status = "intersect"
        reason = "transversal intersection detected"
    elif any(hit.kind == "tangent" for hit in hits):
        status = "tangent"
        reason = "intersection with near-parallel tangents"
    elif edge_min_distance <= tolerance * 0.5:
        status = "uncertain"
        reason = "curves lie inside the tolerance band without a classified crossing"
        t1, t2 = nearest_pair
        hits = [
            IntersectionHit(
                first.point_at(t1).lerp(second.point_at(t2), 0.5),
                t1,
                t2,
                segment_tangent(first, t1),
                segment_tangent(second, t2),
                math.acos(
                    max(
                        -1.0,
                        min(
                            1.0,
                            abs(
                                segment_tangent(first, t1).dot(
                                    segment_tangent(second, t2)
                                )
                            ),
                        ),
                    )
                ),
                "uncertain",
                0.0,
            )
        ]
    elif edge_min_distance <= tolerance * 2.0:
        status = "uncertain"
        reason = "minimum distance is inside the two-tolerance decision band"
    else:
        status = "disjoint"
        reason = "minimum distance exceeds the tolerance band"

    if hits:
        edge_min_distance = min(
            edge_min_distance,
            min(
                hit.point.distance_to(first.point_at(hit.t1))
                + hit.point.distance_to(second.point_at(hit.t2))
                for hit in hits
            ),
        )

    return IntersectionResult(
        status=status,
        tolerance=tolerance,
        hits=hits,
        min_distance=edge_min_distance,
        reason=reason,
        details={
            "flatten_error": flatten_error,
            "coincident_overlap_count": len(coincident_overlaps),
            "hit_kinds": [hit.kind for hit in hits],
        },
    )


def _deduplicate_hits(
    hits: list[IntersectionHit],
    tolerance: float,
) -> list[IntersectionHit]:
    priority = {
        "coincident": 4,
        "intersect": 3,
        "tangent": 2,
        "uncertain": 1,
    }
    unique: list[IntersectionHit] = []
    for hit in sorted(
        hits,
        key=lambda item: (priority.get(item.kind, 0), item.confidence),
        reverse=True,
    ):
        if any(
            existing.point.distance_to(hit.point) <= max(tolerance * 0.5, 1e-10)
            and min(
                abs(existing.t1 - hit.t1),
                abs(existing.t2 - hit.t2),
            )
            <= 0.02
            for existing in unique
        ):
            continue
        unique.append(hit)
    unique.sort(key=lambda item: (item.t1, item.t2, item.kind))
    return unique


def compare_intersection_results(
    previous: IntersectionResult,
    current: IntersectionResult,
) -> dict[str, Any]:
    matching_radius = max(
        previous.tolerance,
        current.tolerance,
        1e-10,
    ) * 8.0
    unmatched_previous = list(previous.hits)
    matched: list[tuple[IntersectionHit, IntersectionHit, float]] = []
    added: list[IntersectionHit] = []
    for hit in current.hits:
        if not unmatched_previous:
            added.append(hit)
            continue
        best = min(
            unmatched_previous,
            key=lambda candidate: candidate.point.distance_to(hit.point),
        )
        distance = best.point.distance_to(hit.point)
        if distance <= matching_radius:
            unmatched_previous.remove(best)
            matched.append((best, hit, distance))
        else:
            added.append(hit)
    removed = unmatched_previous
    max_delta = max((item[2] for item in matched), default=0.0)
    status_changes = [
        {
            "old_status": old.kind,
            "new_status": new.kind,
            "old_point": old.point.as_list(),
            "new_point": new.point.as_list(),
            "position_delta": delta,
        }
        for old, new, delta in matched
        if old.kind != new.kind
    ]
    return {
        "old_tolerance": previous.tolerance,
        "new_tolerance": current.tolerance,
        "old_status": previous.status,
        "new_status": current.status,
        "status_changed": previous.status != current.status,
        "matched_count": len(matched),
        "added_count": len(added),
        "removed_count": len(removed),
        "added_points": [hit.point.as_list() for hit in added],
        "removed_points": [hit.point.as_list() for hit in removed],
        "classification_changes": status_changes,
        "max_position_deviation": max_delta,
        "summary": (
            f"{previous.status}->{current.status}; +{len(added)} "
            f"-{len(removed)} matched={len(matched)}; "
            f"max_position_deviation={max_delta:.12g}"
        ),
    }

