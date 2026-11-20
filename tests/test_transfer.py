from __future__ import annotations

import numpy as np
import pytest

from volrender.errors import TransferFunctionError
from volrender.transfer import DEFAULT_CONTROL_POINTS, TransferFunction


def test_default_interval_bounds_match_quantization_model() -> None:
    tf = TransferFunction()
    bounds = tf.interval_error_bounds
    assert [item["interval"] for item in bounds] == [
        [0.0, 0.25],
        [0.25, 0.5],
        [0.5, 0.75],
        [0.75, 1.0],
    ]
    assert bounds[0]["opacity_error_bound"] == pytest.approx(0.05 * 2.0 / 255.0)
    assert bounds[2]["opacity_error_bound"] == pytest.approx(0.45 * 2.0 / 255.0)


def test_invalid_edit_preserves_previous_lut() -> None:
    tf = TransferFunction()
    before = tf.lut
    digest_before = tf.digest()
    with pytest.raises(TransferFunctionError) as raised:
        tf.replace([(0.0, 0.0, 0.0, 0.0, 0.0), (1.5, 1.0, 1.0, 1.0, 1.0)])
    assert raised.value.code == "E_TF_INVALID"
    assert raised.value.detail == "CONTROL_POINT_OUT_OF_RANGE"
    assert raised.value.field == "control_points[1]"
    assert np.array_equal(tf.lut, before)
    assert tf.digest() == digest_before


def test_monotonic_and_channel_ranges_are_validated() -> None:
    tf = TransferFunction()
    with pytest.raises(TransferFunctionError, match="严格递增"):
        tf.replace([(0.0, 0, 0, 0, 0), (0.5, 0, 0, 0, 0), (0.5, 1, 1, 1, 1)])
    with pytest.raises(TransferFunctionError) as raised:
        tf.replace([(0.0, 0, 0, 0, 0), (1.0, 1.2, 1, 1, 1)])
    assert raised.value.field == "control_points[1].r"
    assert raised.value.offset == 1


def test_lut_rebuild_is_continuous_and_opacity_ordered() -> None:
    tf = TransferFunction(DEFAULT_CONTROL_POINTS, resolution=512)
    lut = tf.lut_float
    jumps = np.abs(np.diff(lut[:, 3]))
    assert float(np.max(jumps)) < 0.01
    assert lut.shape == (512, 4)

