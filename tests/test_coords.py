import pytest

from ocr_pipeline.coords import (
    DEFAULT_PRECISION,
    normalized_to_pixel,
    pixel_to_normalized,
    quantization_step,
    round_trip,
)
from ocr_pipeline.errors import CoordError

QUAD = ((10, 20), (110, 20), (110, 45), (10, 45))
W, H = 640, 480


@pytest.mark.parametrize("rounding", ["half_up", "half_even", "floor", "ceil"])
def test_round_trip_within_quantization_step(rounding):
    back = round_trip(QUAD, W, H, rounding=rounding)
    step = quantization_step(W, H, DEFAULT_PRECISION)
    for orig, conv in zip(QUAD, back):
        assert abs(orig[0] - conv[0]) <= max(1, step)
        assert abs(orig[1] - conv[1]) <= max(1, step)


def test_round_trip_exact_at_default_precision():
    assert round_trip(QUAD, W, H) == QUAD


def test_precision_is_declared_and_parameterizable():
    nq = pixel_to_normalized(QUAD, W, H, precision=3)
    for nx, ny in nq:
        assert abs(nx * 1000 - round(nx * 1000)) < 1e-9
        assert abs(ny * 1000 - round(ny * 1000)) < 1e-9


def test_normalized_out_of_range_raises_e_coord():
    bad = [(0.1, 0.1), (1.2, 0.1), (1.2, 0.3), (0.1, 0.3)]
    with pytest.raises(CoordError) as exc:
        normalized_to_pixel(bad, W, H, line_id=7)
    assert exc.value.code == "E_COORD"
    assert exc.value.line_id == 7
    assert exc.value.point_index == 1


def test_pixel_out_of_bounds_raises_e_coord():
    bad = ((0, 0), (W + 5, 0), (W + 5, 10), (0, 10))
    with pytest.raises(CoordError) as exc:
        pixel_to_normalized(bad, W, H, line_id=3)
    assert exc.value.line_id == 3
    assert exc.value.point_index == 1


def test_unknown_rounding_mode_rejected():
    with pytest.raises(ValueError):
        pixel_to_normalized(QUAD, W, H, rounding="bankers-plus")
