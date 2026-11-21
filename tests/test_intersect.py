from __future__ import annotations

from vectorbool.errors import BusinessError, ErrorCode
from vectorbool.intersect import (
    diff_intersections,
    intersect_segments,
    recompute_intersections,
)
from vectorbool.model import Point, Segment, SegmentKind


def line(a: tuple[float, float], b: tuple[float, float]) -> Segment:
    return Segment(SegmentKind.LINE, (Point(*a), Point(*b)))


def test_line_curve_and_tangent_intersections() -> None:
    horizontal = line((0.0, 0.5), (2.0, 0.5))
    quadratic = Segment(
        SegmentKind.QUADRATIC,
        (Point(0.0, 0.0), Point(1.0, 1.0), Point(2.0, 0.0)),
    )
    hits = intersect_segments(horizontal, quadratic, tolerance=1.0e-6)
    assert hits
    assert all(hit.kind in {"cross", "tangent", "near_coincident"} for hit in hits)
    assert hits[0].confidence in {"high", "medium", "low"}

    tangent = line((0.0, 0.5), (2.0, 0.5))
    tangent_hits = intersect_segments(tangent, quadratic, tolerance=1.0e-5)
    assert tangent_hits
    assert any(hit.kind == "tangent" for hit in tangent_hits)


def test_tolerance_recompute_reports_difference_summary() -> None:
    a = line((0.0, 0.0), (2.0, 2.0))
    b = line((0.0, 2.0), (2.0, 0.0))
    new, diff = recompute_intersections(
        a, b, old_tolerance=1.0e-6, new_tolerance=1.0e-4
    )
    assert len(new) == 1
    payload = diff.to_dict()
    assert payload["old_count"] == 1
    assert payload["new_count"] == 1
    assert payload["matched_count"] == 1
    assert payload["max_position_deviation"] <= 1.0e-12
    assert payload["summary"] == "unchanged"


def test_near_parallel_is_not_silently_dropped() -> None:
    a = line((0.0, 0.0), (2.0, 0.0))
    b = line((0.0, 5.0e-5), (2.0, 5.0e-5))
    hits = intersect_segments(a, b, tolerance=1.0e-4)
    assert hits
    assert hits[0].kind == "near_coincident"
    assert hits[0].confidence == "low"


def test_unstable_decision_band_raises_ambiguous() -> None:
    a = line((0.0, 0.0), (2.0, 0.0))
    b = line((0.5, 1.5e-4), (1.5, 1.5e-4))
    try:
        intersect_segments(a, b, tolerance=1.0e-4)
    except BusinessError as error:
        assert error.code == ErrorCode.INTERSECT_AMBIGUOUS
    else:
        raise AssertionError("expected E_INTERSECT_AMBIGUOUS")


def test_intersection_diff_detects_added_items() -> None:
    old = intersect_segments(
        line((0.0, 0.0), (1.0, 1.0)),
        line((0.0, 1.0), (1.0, 0.0)),
        tolerance=1.0e-6,
    )
    diff = diff_intersections(old, [], tolerance=1.0e-6)
    assert diff.old_count == 1
    assert diff.new_count == 0
    assert len(diff.removed) == 1

