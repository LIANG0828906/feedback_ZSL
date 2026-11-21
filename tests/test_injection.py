from __future__ import annotations

import pytest

from vectorbool.errors import BusinessError, ErrorCode
from vectorbool.injection import apply_injection, validate_entry

from .helpers import rectangle


@pytest.mark.parametrize(
    ("kind", "expected_code"),
    [
        ("coincident_edge", ErrorCode.COINCIDENT_EDGE),
        ("degenerate_segment", ErrorCode.DEGENERATE_SEGMENT),
        ("zero_area_contour", ErrorCode.ZERO_AREA_CONTOUR),
        ("hole_reversed", ErrorCode.HOLE_REVERSED),
        ("non_finite_coordinate", ErrorCode.NON_FINITE_COORDINATE),
    ],
)
def test_each_injection_is_rejected_at_entry(kind: str, expected_code: ErrorCode) -> None:
    raw_a, raw_b, location = apply_injection(
        rectangle(0.0, 0.0, 2.0, 2.0),
        rectangle(3.0, 0.0, 5.0, 2.0),
        kind,
        tolerance=1.0e-6,
    )
    with pytest.raises(BusinessError) as captured:
        validate_entry(
            raw_a,
            raw_b,
            tolerance=1.0e-6,
            injections=[kind],
        )
    assert captured.value.code == expected_code
    assert captured.value.location.injection == kind
    assert location.kind.value == kind

