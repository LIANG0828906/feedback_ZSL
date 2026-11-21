from __future__ import annotations

import copy
from typing import Any

import pytest

from haptic_studio.models import DeviceConfig, WaveSequence


def wave_payload() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "sequence_id": "test-sequence",
        "total_duration_ms": 240.0,
        "segments": [
            {
                "start_ms": 0.0,
                "duration_ms": 90.0,
                "frequency_hz": 40.0,
                "amplitude": 0.7,
                "envelope": "rectangular",
            },
            {
                "start_ms": 50.0,
                "duration_ms": 110.0,
                "frequency_hz": 55.0,
                "amplitude": 0.8,
                "envelope": "linear_decay",
            },
            {
                "start_ms": 170.0,
                "duration_ms": 60.0,
                "frequency_hz": 35.0,
                "amplitude": 0.55,
                "envelope": "sine",
            },
        ],
    }


def config_payload() -> dict[str, Any]:
    return {
        "sample_rate": 1000,
        "bit_depth": 8,
        "allocation_ratio": {"left": 0.48, "right": 0.52},
        "polarity": {"left": 1, "right": -1},
        "limiter": "hard",
    }


@pytest.fixture
def sample_wave() -> dict[str, Any]:
    return copy.deepcopy(wave_payload())


@pytest.fixture
def sample_config() -> dict[str, Any]:
    return copy.deepcopy(config_payload())


@pytest.fixture
def sequence(sample_wave: dict[str, Any]) -> WaveSequence:
    return WaveSequence.from_mapping(sample_wave)


@pytest.fixture
def config(sample_config: dict[str, Any]) -> DeviceConfig:
    return DeviceConfig.from_mapping(sample_config)
