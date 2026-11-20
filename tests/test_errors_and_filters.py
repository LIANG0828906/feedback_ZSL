from __future__ import annotations

import math

import numpy as np
import pytest

from studio_calib.config import CalibrationConfig, TargetPoint
from studio_calib.errors import CalibrationError
from studio_calib.pipeline import CalibrationPipeline


def test_error_upper_bounds_are_independently_recomputable(
    sample_dataset: dict, cache_dir
) -> None:
    result = CalibrationPipeline(cache_dir).run(
        sample_dataset["surround"],
        CalibrationConfig(error_threshold_db=0.5),
    )
    saw_budget_error = False
    for channel in result.channel_names:
        for item in result.errors[channel]:
            recomputed = sum(
                10.0
                * math.log10(1.0 + max(fraction, 0.0))
                for fraction in item["terms_power_fraction"].values()
            )
            assert item["total_error_db"] == pytest.approx(recomputed, abs=1e-10)
            assert item["total_error_db"] == pytest.approx(
                sum(item["terms_db"].values()), abs=1e-10
            )
            assert item["dominant_node"] == max(
                item["terms_db"].items(), key=lambda kv: (kv[1], kv[0])
            )[0]
            expected_status = (
                "E_ERROR_BUDGET"
                if item["total_error_db"] > 0.5
                else "OK"
            )
            assert item["status"] == expected_status
            saw_budget_error |= item["status"] == "E_ERROR_BUDGET"
    assert saw_budget_error


def test_invalid_filter_order_returns_typed_error(
    sample_dataset: dict, cache_dir
) -> None:
    config = CalibrationConfig(filter_taps=100)
    with pytest.raises(CalibrationError) as caught:
        CalibrationPipeline(cache_dir).run(sample_dataset["surround"], config)
    assert caught.value.code == "E_FILTER_INVALID"
    assert caught.value.parameter == "filter_taps"


def test_invalid_target_curve_returns_typed_error(
    sample_dataset: dict, cache_dir
) -> None:
    config = CalibrationConfig(
        target_curve=(TargetPoint(1000, 0), TargetPoint(100, 1))
    )
    with pytest.raises(CalibrationError) as caught:
        CalibrationPipeline(cache_dir).run(sample_dataset["surround"], config)
    assert caught.value.code == "E_FILTER_INVALID"
    assert caught.value.parameter == "target_curve"


def test_filter_is_linear_phase_and_group_delay_is_reported(
    sample_dataset: dict, cache_dir
) -> None:
    config = CalibrationConfig(filter_taps=1025)
    result = CalibrationPipeline(cache_dir).run(sample_dataset["surround"], config)
    for channel in result.channel_names:
        coefficients = np.asarray(result.filters[channel]["coefficients"])
        assert coefficients.size == config.filter_taps
        assert np.max(np.abs(coefficients - coefficients[::-1])) < 1e-12
        assert result.filters[channel]["group_delay_samples"] == pytest.approx(
            (config.filter_taps - 1) / 2.0
        )
        assert result.filters[channel]["group_delay_seconds"] == pytest.approx(
            (config.filter_taps - 1) / (2.0 * 48000.0)
        )

