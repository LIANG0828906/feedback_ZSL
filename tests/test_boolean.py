from __future__ import annotations

import pytest

from vectorbool.boolean import boolean_paths, detect_self_intersections
from vectorbool.errors import BusinessError, ErrorCode
from vectorbool.evaluate import evaluate_boolean_result
from vectorbool.model import Contour, Point
from vectorbool.parser import parse_path

from .helpers import rectangle, ring_with_hole


@pytest.mark.parametrize(
    ("operation", "expected_area", "expected_contours"),
    [
        ("union", 7.0, 1),
        ("intersect", 1.0, 1),
        ("difference", 3.0, 1),
        ("xor", 6.0, 2),
    ],
)
def test_four_operations(operation: str, expected_area: float, expected_contours: int) -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)
    result = boolean_paths(path_a, path_b, operation, tolerance=1.0e-6)
    assert len(result.contours) == expected_contours
    assert sum(contour.signed_area() for contour in result.contours) == pytest.approx(
        expected_area
    )
    assert result.report.status == "ok"
    assert result.preview_allowed
    assert result.report.area_relative_deviation <= 1.0e-3


def test_hole_orientation_and_signed_area_match_fill_rule() -> None:
    path_a = parse_path(ring_with_hole(), tolerance=1.0e-6)
    path_b = parse_path(rectangle(2.0, 2.0, 5.0, 5.0), tolerance=1.0e-6)
    result = boolean_paths(path_a, path_b, "union", tolerance=1.0e-6)
    roles = {contour.role: contour.signed_area() for contour in result.contours}
    assert roles["hole"] < 0.0
    assert roles["outer"] > 0.0
    assert sum(contour.signed_area() for contour in result.contours) == pytest.approx(18.0)


def test_near_coincident_below_tolerance_is_ambiguous() -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-4)
    path_b = parse_path(
        rectangle(-1.0e-5, 1.0e-5, 2.00001, 2.00001), tolerance=1.0e-4
    )
    with pytest.raises(BusinessError) as captured:
        boolean_paths(path_a, path_b, "difference", tolerance=1.0e-4)
    assert captured.value.code == ErrorCode.INTERSECT_AMBIGUOUS
    assert captured.value.location.path == "b"


def test_narrow_but_resolvable_strip_is_not_degraded() -> None:
    tolerance = 1.0e-4
    path_a = parse_path(rectangle(0.0, 0.0, 16.0, 16.0), tolerance=tolerance)
    path_b = parse_path(
        rectangle(-0.00016, 0.00016, 16.00016, 16.00016), tolerance=tolerance
    )
    result = boolean_paths(path_a, path_b, "difference", tolerance=tolerance)
    assert len(result.contours) == 1
    assert result.contours[0].signed_area() == pytest.approx(0.00256)
    assert result.report.status == "ok"


def test_self_intersecting_input_is_processed_deterministically() -> None:
    raw = {
        "fill_rule": "evenodd",
        "subpaths": [
            {
                "closed": True,
                "segments": [
                    {"kind": "L", "points": [[0, 0], [2, 2]]},
                    {"kind": "L", "points": [[2, 2], [2, 0]]},
                    {"kind": "L", "points": [[2, 0], [0, 2]]},
                    {"kind": "L", "points": [[0, 2], [0, 0]]},
                ],
            }
        ],
    }
    path_a = parse_path(raw, tolerance=1.0e-6)
    path_b = parse_path(rectangle(0.5, 0.5, 2.5, 2.5), tolerance=1.0e-6)
    first = boolean_paths(path_a, path_b, "union", tolerance=1.0e-6)
    second = boolean_paths(path_a, path_b, "union", tolerance=1.0e-6)
    assert first.to_dict() == second.to_dict()
    assert first.report.status == "ok"


def test_self_intersection_detector_reports_segments() -> None:
    contour = Contour(
        points=(
            Point(0.0, 0.0),
            Point(2.0, 2.0),
            Point(2.0, 0.0),
            Point(0.0, 2.0),
        )
    )
    hits = detect_self_intersections([contour], tolerance=1.0e-6)
    assert hits
    assert hits[0]["contour"] == 0
    assert "segment_a" in hits[0]


def test_exceeded_threshold_produces_real_degraded_report() -> None:
    path_a = parse_path(rectangle(0.0, 0.0, 2.0, 2.0), tolerance=1.0e-6)
    path_b = parse_path(rectangle(1.0, 1.0, 3.0, 3.0), tolerance=1.0e-6)
    result = boolean_paths(path_a, path_b, "union", tolerance=1.0e-6)
    report = evaluate_boolean_result(
        path_a,
        path_b,
        result,
        tolerance=1.0e-6,
        boundary_threshold=0.0,
    )
    assert not report.accepted
    assert report.status == "degraded"
    assert "boundary_deviation_exceeded" in report.degradation_reasons


@pytest.mark.parametrize(
    ("raw_a", "raw_b", "expected"),
    [
        (
            rectangle(0.0, 0.0, 2.0, 2.0),
            rectangle(2.0, 0.0, 4.0, 2.0),
            {"union": 8.0, "intersect": 0.0, "difference": 4.0, "xor": 8.0},
        ),
        (
            rectangle(0.0, 0.0, 2.0, 2.0),
            rectangle(0.0, 0.0, 2.0, 2.0),
            {"union": 4.0, "intersect": 4.0, "difference": 0.0, "xor": 0.0},
        ),
    ],
)
def test_shared_and_identical_edges_are_stable(raw_a, raw_b, expected) -> None:
    path_a = parse_path(raw_a, tolerance=1.0e-6)
    path_b = parse_path(raw_b, tolerance=1.0e-6)
    for operation, area in expected.items():
        result = boolean_paths(path_a, path_b, operation, tolerance=1.0e-6)
        assert sum(contour.signed_area() for contour in result.contours) == pytest.approx(
            area
        )
        assert result.report.status == "ok"

