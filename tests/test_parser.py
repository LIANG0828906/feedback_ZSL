from __future__ import annotations

import math

import pytest

from vectorbool.errors import BusinessError, ErrorCode
from vectorbool.geometry import flatten_subpath, signed_area
from vectorbool.parser import parse_path

from .helpers import rectangle, ring_with_hole


def test_normalize_is_deterministic_and_hole_direction_is_consistent() -> None:
    first = parse_path(ring_with_hole(), tolerance=1.0e-6, path_name="a")
    second = parse_path(ring_with_hole(), tolerance=1.0e-6, path_name="a")
    assert first.to_dict() == second.to_dict()
    assert [subpath.contour_role for subpath in first.subpaths] == ["outer", "hole"]
    areas = [signed_area(flatten_subpath(subpath, 1.0e-6).points) for subpath in first.subpaths]
    assert areas[0] > 0.0
    assert areas[1] < 0.0


def test_missing_field_has_subpath_and_segment_location() -> None:
    raw = rectangle(0.0, 0.0, 1.0, 1.0)
    del raw["subpaths"][0]["segments"][2]["kind"]
    with pytest.raises(BusinessError) as captured:
        parse_path(raw, tolerance=1.0e-6, path_name="broken")
    assert captured.value.code == ErrorCode.PATH_INVALID
    assert captured.value.location.path == "broken"
    assert captured.value.location.subpath == 0
    assert captured.value.location.segment == 2


def test_non_finite_coordinate_has_dedicated_code() -> None:
    raw = rectangle(0.0, 0.0, 1.0, 1.0)
    raw["subpaths"][0]["segments"][0]["points"][0][0] = math.inf
    with pytest.raises(BusinessError) as captured:
        parse_path(raw, tolerance=1.0e-6)
    assert captured.value.code == ErrorCode.NON_FINITE_COORDINATE
    assert captured.value.parent_code == ErrorCode.PATH_INVALID.value


def test_zero_area_contour_rejected_but_bowtie_is_allowed() -> None:
    zero = {
        "fill_rule": "nonzero",
        "subpaths": [
            {
                "closed": True,
                "segments": [
                    {"kind": "L", "points": [[0, 0], [1, 0]]},
                    {"kind": "L", "points": [[1, 0], [2, 0]]},
                    {"kind": "L", "points": [[2, 0], [0, 0]]},
                ],
            }
        ],
    }
    with pytest.raises(BusinessError) as captured:
        parse_path(zero, tolerance=1.0e-6)
    assert captured.value.code == ErrorCode.ZERO_AREA_CONTOUR

    bowtie = {
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
    parsed = parse_path(bowtie, tolerance=1.0e-6)
    assert parsed.fill_rule.value == "evenodd"


def test_degenerate_segment_rejected() -> None:
    raw = rectangle(0.0, 0.0, 1.0, 1.0)
    raw["subpaths"][0]["segments"][0]["points"][1] = [1.0e-9, 0.0]
    raw["subpaths"][0]["segments"][1]["points"][0] = [1.0e-9, 0.0]
    with pytest.raises(BusinessError) as captured:
        parse_path(raw, tolerance=1.0e-6)
    assert captured.value.code == ErrorCode.DEGENERATE_SEGMENT

