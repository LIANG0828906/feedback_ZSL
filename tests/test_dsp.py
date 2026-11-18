from __future__ import annotations

import numpy as np

from orchestral_mastering.dsp import (
    apply_loudness_and_peak_limit,
    integrated_loudness,
    linked_true_peak_limiter,
    true_peak_dbtp,
)


def test_true_peak_limiter_preserves_length_and_ceiling() -> None:
    sample_rate = 48000
    time = np.arange(sample_rate, dtype=np.float64) / sample_rate
    mono = 1.8 * np.sin(2.0 * np.pi * 997.0 * time)
    stereo = np.stack((mono, 0.93 * mono), axis=1)
    limited = linked_true_peak_limiter(stereo, sample_rate, -1.0)
    assert len(limited.audio) == len(stereo)
    assert limited.latency_samples == 0
    assert limited.triggered is True
    assert true_peak_dbtp(limited.audio) <= -1.0 + 1e-6


def test_loudness_driver_hits_target_under_peak_constraint() -> None:
    sample_rate = 48000
    time = np.arange(2 * sample_rate, dtype=np.float64) / sample_rate
    carrier = 0.10 * np.sin(2.0 * np.pi * 440.0 * time)
    accent = np.zeros_like(carrier)
    accent[::4800] = 0.30
    mono = carrier + accent
    stereo = np.stack((mono, 0.8 * mono), axis=1)
    result = apply_loudness_and_peak_limit(
        stereo,
        sample_rate,
        -14.0,
        -1.0,
        max_iterations=8,
    )
    assert abs(integrated_loudness(result.audio, sample_rate) - (-14.0)) <= 0.5
    assert true_peak_dbtp(result.audio) <= -1.0 + 1e-6
    assert len(result.audio) == len(stereo)

