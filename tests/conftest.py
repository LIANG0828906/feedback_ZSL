from __future__ import annotations

import os
from pathlib import Path

import numpy as np
import pytest

from game_audio_restorer.recipes import Recipe, recipe_from_mapping
from game_audio_restorer.wav_io import encode_wav

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")


def write_test_wav(
    path: Path,
    *,
    sample_rate: int = 16_000,
    duration: float = 2.4,
    channels: int = 2,
) -> np.ndarray:
    frames = int(round(sample_rate * duration))
    time = np.arange(frames, dtype=np.float64) / sample_rate
    envelope = np.where((time % 1.0) < 0.72, 1.0, 0.0)
    source = np.empty((frames, channels), dtype=np.float64)
    for channel in range(channels):
        dc = 0.045 if channel == 0 else -0.031
        frequency = 180.0 + 37.0 * channel
        voice = 0.22 * np.sin(2 * np.pi * frequency * time) * envelope
        harmonics = 0.05 * np.sin(2 * np.pi * frequency * 2.02 * time + 0.2) * envelope
        source[:, channel] = dc + voice + harmonics
    source += np.random.default_rng(42).normal(0.0, 0.003, size=source.shape)
    source = np.clip(source, -0.95, 0.95)
    path.write_bytes(encode_wav(source, sample_rate))
    return source


def recipe_v1() -> Recipe:
    return recipe_from_mapping(
        {"version": 1, "denoise_strength": 0.5, "gain_db": 2.0},
        source="<test-v1>",
    )


def recipe_v2() -> Recipe:
    return recipe_from_mapping(
        {
            "version": 2,
            "denoise_strength": 0.5,
            "gain_db": 2.0,
            "remove_dc": True,
            "silence_threshold_dbfs": -54.0,
        },
        source="<test-v2>",
    )


def recipe_v3(mode: str = "preserve") -> Recipe:
    return recipe_from_mapping(
        {
            "version": 3,
            "denoise_strength": 0.5,
            "gain_db": 2.0,
            "remove_dc": True,
            "silence_threshold_dbfs": -54.0,
            "channel_merge_mode": mode,
            "spectral_subtraction": {
                "segment_ms": 32,
                "oversubtraction": 1.8,
                "spectral_floor": 0.015,
            },
            "loudness_target_lufs": -20.0,
        },
        source="<test-v3>",
    )


@pytest.fixture
def stereo_wav(tmp_path: Path) -> Path:
    path = tmp_path / "stereo.wav"
    write_test_wav(path)
    return path
