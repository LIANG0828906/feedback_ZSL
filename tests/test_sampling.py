from __future__ import annotations

from typing import Any

import pytest

from haptic_studio.models import DeviceConfig, WaveSequence
from haptic_studio.sampler import compute_full_signal, ms_to_sample


@pytest.mark.parametrize("sample_rate", [137, 500, 1000, 2000, 4800])
@pytest.mark.parametrize(
    ("envelope", "duration"),
    [
        ("rectangular", 37.0),
        ("linear_decay", 83.0),
        ("sine", 121.5),
    ],
)
def test_envelope_interval_is_rate_independent(
    envelope: str, duration: float, sample_rate: int
) -> None:
    sequence = WaveSequence.from_mapping(
        {
            "sequence_id": f"interval-{sample_rate}",
            "total_duration_ms": 200,
            "segments": [
                {
                    "start_ms": 17.25,
                    "duration_ms": duration,
                    "frequency_hz": 20,
                    "amplitude": 0.6,
                    "envelope": envelope,
                }
            ],
        }
    )
    config = DeviceConfig.from_mapping(
        {
            "sample_rate": sample_rate,
            "bit_depth": 16,
            "allocation_ratio": [0.5, 0.5],
            "polarity": [1, 1],
            "limiter": "hard",
        }
    )
    result = compute_full_signal(sequence, config)
    signal = result.segment_signals[0]
    expected_start = ms_to_sample(17.25, sample_rate)
    expected_end = ms_to_sample(17.25 + duration, sample_rate)
    assert signal.start_sample == expected_start
    assert signal.end_sample == expected_end
    assert signal.sample_count == expected_end - expected_start
    assert len(signal.left) == signal.sample_count
    assert len(signal.content_hash) == 64


def test_mapping_allocation_and_polarity(sequence: WaveSequence) -> None:
    normal = DeviceConfig.from_mapping(
        {
            "sample_rate": 1000,
            "bit_depth": 16,
            "allocation_ratio": [0.75, 0.25],
            "polarity": [1, 1],
            "limiter": "hard",
        }
    )
    inverted = DeviceConfig.from_mapping(
        {
            "sample_rate": 1000,
            "bit_depth": 16,
            "allocation_ratio": [0.75, 0.25],
            "polarity": [-1, 1],
            "limiter": "hard",
        }
    )
    normal_result = compute_full_signal(sequence, normal)
    inverted_result = compute_full_signal(sequence, inverted)
    normal_signal = normal_result.segment_signals[0]
    inverted_signal = inverted_result.segment_signals[0]
    assert (inverted_signal.left == -normal_signal.left).all()
    assert (inverted_signal.right == normal_signal.right).all()
    assert int(abs(normal_signal.left).max()) > int(abs(normal_signal.right).max())


def test_repeat_compute_is_byte_identical(
    sequence: WaveSequence, config: DeviceConfig
) -> None:
    first = compute_full_signal(sequence, config)
    second = compute_full_signal(sequence, config)
    assert first.signal_hash == second.signal_hash
    assert first.left_bytes == second.left_bytes
    assert first.right_bytes == second.right_bytes
    assert first.limited_bytes == second.limited_bytes
    assert [item.content_hash for item in first.segment_signals] == [
        item.content_hash for item in second.segment_signals
    ]


def _clipping_fixture(limiter: str) -> Any:
    segment = {
        "start_ms": 10,
        "duration_ms": 80,
        "frequency_hz": 25,
        "amplitude": 1.0,
        "envelope": "rectangular",
    }
    sequence = WaveSequence.from_mapping(
        {
            "sequence_id": f"clip-{limiter}",
            "total_duration_ms": 100,
            "segments": [segment, dict(segment)],
        }
    )
    config = DeviceConfig.from_mapping(
        {
            "sample_rate": 1000,
            "bit_depth": 8,
            "allocation_ratio": [0.8, 0.2],
            "polarity": [1, 1],
            "limiter": limiter,
        }
    )
    return compute_full_signal(sequence, config), config


def test_hard_limiter_statistics_and_range() -> None:
    result, config = _clipping_fixture("hard")
    assert result.limit_stats.limited_sample_count > 0
    assert result.limit_stats.left_limited_count > 0
    assert result.limit_stats.first_limited_sample is not None
    assert int(result.left_array.min()) >= config.min_code
    assert int(result.left_array.max()) <= config.max_code
    assert result.limited_mask.sum() > 0
    assert result.limited_mask[result.limit_stats.first_limited_sample] == 1


def test_soft_limiter_is_bounded_and_different_from_hard() -> None:
    hard, _ = _clipping_fixture("hard")
    soft, config = _clipping_fixture("soft")
    assert int(soft.left_array.min()) >= config.min_code
    assert int(soft.left_array.max()) <= config.max_code
    assert (
        soft.limit_stats.limited_sample_count == hard.limit_stats.limited_sample_count
    )
    assert soft.left_bytes != hard.left_bytes


def test_sorted_order_uses_start_then_index() -> None:
    sequence = WaveSequence.from_mapping(
        {
            "sequence_id": "order",
            "total_duration_ms": 100,
            "segments": [
                {
                    "start_ms": 50,
                    "duration_ms": 10,
                    "frequency_hz": 20,
                    "amplitude": 0.5,
                    "envelope": "rectangular",
                },
                {
                    "start_ms": 5,
                    "duration_ms": 10,
                    "frequency_hz": 20,
                    "amplitude": 0.5,
                    "envelope": "rectangular",
                },
                {
                    "start_ms": 50,
                    "duration_ms": 10,
                    "frequency_hz": 30,
                    "amplitude": 0.5,
                    "envelope": "rectangular",
                },
            ],
        }
    )
    config = DeviceConfig.from_mapping({"sample_rate": 1000, "bit_depth": 16})
    result = compute_full_signal(sequence, config)
    assert [item.index for item in result.segment_signals] == [1, 0, 2]
