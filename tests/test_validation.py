from __future__ import annotations

import copy
from typing import Any

import pytest

from haptic_studio.errors import ErrorCode, HapticError
from haptic_studio.models import DeviceConfig, WaveSequence
from haptic_studio.sampler import compute_full_signal


@pytest.mark.parametrize(
    ("mutation", "field_name", "segment_index"),
    [
        (lambda data: data["segments"][1].pop("start_ms"), "start_ms", 1),
        (lambda data: data["segments"][2].pop("envelope"), "envelope", 2),
        (
            lambda data: data["segments"][0].__setitem__("duration_ms", -5),
            "duration_ms",
            0,
        ),
        (
            lambda data: data["segments"][1].__setitem__("frequency_hz", 0),
            "frequency_hz",
            1,
        ),
        (
            lambda data: data["segments"][1].__setitem__("frequency_hz", 100_001),
            "frequency_hz",
            1,
        ),
        (
            lambda data: data["segments"][2].__setitem__("duration_ms", 1000),
            "duration_ms",
            2,
        ),
        (
            lambda data: data["segments"][0].__setitem__("amplitude", 1.5),
            "amplitude",
            0,
        ),
    ],
)
def test_wave_validation_locates_segment_and_field(
    sample_wave: dict[str, Any],
    mutation: Any,
    field_name: str,
    segment_index: int,
) -> None:
    data = copy.deepcopy(sample_wave)
    mutation(data)
    with pytest.raises(HapticError) as caught:
        WaveSequence.from_mapping(data)
    error = caught.value
    assert error.code == ErrorCode.WAVE_INVALID
    assert error.field == field_name
    assert error.segment_index == segment_index


def test_missing_segments_root_is_wave_invalid(sample_wave: dict[str, Any]) -> None:
    sample_wave.pop("segments")
    with pytest.raises(HapticError) as caught:
        WaveSequence.from_mapping(sample_wave)
    assert caught.value.code == ErrorCode.WAVE_INVALID
    assert caught.value.field == "segments"


def test_nyquist_violation_is_wave_invalid(
    sample_wave: dict[str, Any], sample_config: dict[str, Any]
) -> None:
    config = DeviceConfig.from_mapping(
        {**sample_config, "sample_rate": 64, "bit_depth": 8}
    )
    beginner = WaveSequence.from_mapping(
        {
            **sample_wave,
            "segments": [
                {
                    **sample_wave["segments"][0],
                    "frequency_hz": 40,
                }
            ],
        }
    )
    with pytest.raises(HapticError) as caught:
        compute_full_signal(beginner, config)
    assert caught.value.code == ErrorCode.WAVE_INVALID
    assert caught.value.field == "frequency_hz"
    assert caught.value.segment_index == 0


def test_config_validation_has_stable_code() -> None:
    with pytest.raises(HapticError) as caught:
        DeviceConfig.from_mapping(
            {
                "sample_rate": 1000,
                "bit_depth": 16,
                "allocation_ratio": [0.8, 0.4],
                "polarity": [1, 1],
                "limiter": "hard",
            }
        )
    assert caught.value.code == ErrorCode.CONFIG_INVALID
    assert caught.value.field == "allocation_ratio"
