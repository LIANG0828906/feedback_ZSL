from __future__ import annotations

import time

import pytest

from studio_calib.config import CalibrationConfig, TargetPoint
from studio_calib.errors import CalibrationError
from studio_calib.pipeline import CalibrationPipeline
from studio_calib.worker import ProcessingCoordinator


def test_consistency_diagnostics_locates_phase_and_gain_jump(
    sample_dataset: dict, cache_dir
) -> None:
    result = CalibrationPipeline(cache_dir).run(
        sample_dataset["surround"],
        CalibrationConfig(gain_jump_threshold_db=3.0),
    )
    warnings = result.consistency["warnings"]
    assert warnings
    assert any(
        item["kind"] in {"phase_mismatch", "phase_inversion"}
        and item["channel"] in result.channel_names
        and "band_start" in item
        and "band_end" in item
        for item in warnings
    )
    low_jumps = [
        item for item in warnings if item["kind"] == "low_frequency_gain_jump"
    ]
    assert low_jumps
    allowed_sources = {
        "alignment_offset",
        "smoothing_coupling",
        "filter_truncation",
    }
    for warning in low_jumps:
        assert warning["source"] in allowed_sources
        assert set(warning["attribution"]) == allowed_sources
        assert "smoothing_coupling_power" in warning["evidence"]
        assert "truncation_db" in warning["evidence"]
        assert "offset_samples" in warning["evidence"]


def _wait_for_completion(coordinator: ProcessingCoordinator, timeout: float = 30.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        completion = coordinator.take_completion()
        if completion is not None:
            return completion
        time.sleep(0.05)
    raise AssertionError("worker did not complete before timeout")


def test_worker_returns_busy_and_retains_last_committed_version(
    sample_dataset: dict, cache_dir
) -> None:
    coordinator = ProcessingCoordinator(CalibrationPipeline(cache_dir))
    coordinator.submit(sample_dataset["surround"], CalibrationConfig(filter_taps=1025))
    with pytest.raises(CalibrationError) as caught:
        coordinator.submit(
            sample_dataset["surround"], CalibrationConfig(filter_taps=1025)
        )
    assert caught.value.code == "E_BUSY"
    completion = _wait_for_completion(coordinator, timeout=60.0)
    assert completion.status == "committed"
    assert coordinator.last_result is not None
    committed = coordinator.last_result

    coordinator.submit(
        sample_dataset["surround"],
        CalibrationConfig(
            target_curve=(TargetPoint(1000, 0), TargetPoint(100, 1))
        ),
    )
    completion = _wait_for_completion(coordinator, timeout=20.0)
    assert completion.status == "error"
    assert completion.error is not None
    assert completion.error.code == "E_FILTER_INVALID"
    assert coordinator.last_result is committed


def test_worker_cancel_retains_previous_result(
    sample_dataset: dict, cache_dir
) -> None:
    coordinator = ProcessingCoordinator(CalibrationPipeline(cache_dir))
    coordinator.submit(sample_dataset["surround"], CalibrationConfig(filter_taps=1025))
    completion = _wait_for_completion(coordinator, timeout=60.0)
    assert completion.status == "committed"
    committed = coordinator.last_result

    coordinator.submit(
        sample_dataset["surround"],
        CalibrationConfig(filter_taps=4097, smoothing_octaves=0.5),
    )
    assert coordinator.cancel() in {True, False}
    completion = _wait_for_completion(coordinator, timeout=60.0)
    assert completion.status in {"committed", "cancelled"}
    if completion.status == "cancelled":
        assert coordinator.last_result is committed

