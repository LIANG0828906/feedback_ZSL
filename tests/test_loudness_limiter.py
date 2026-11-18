from __future__ import annotations

import numpy as np

from mastering.limiter import limit_true_peak, true_peak_dbtp
from mastering.loudness import integrated_loudness


def test_bs1770_reference_sine_loudness():
    sample_rate = 48000
    time = np.arange(sample_rate * 2) / sample_rate
    signal = np.column_stack(
        (0.1 * np.sin(2.0 * np.pi * 997.0 * time),) * 2
    )
    loudness = integrated_loudness(signal, sample_rate)
    assert abs(loudness - (-20.0)) < 0.05


def test_true_peak_limiter_is_bounded_and_length_preserving():
    sample_rate = 48000
    rng = np.random.default_rng(42)
    time = np.arange(sample_rate * 3) / sample_rate
    signal = np.column_stack(
        (
            2.4 * np.sin(2.0 * np.pi * 731.0 * time)
            + 0.8 * rng.standard_normal(time.size),
            2.1 * np.sin(2.0 * np.pi * 977.0 * time + 0.4)
            + 0.8 * rng.standard_normal(time.size),
        )
    )
    result = limit_true_peak(signal, sample_rate, -1.0)
    assert result.samples.shape == signal.shape
    assert true_peak_dbtp(result.samples) <= -1.0
    assert result.triggered_samples > 0
    assert result.max_attenuation_db < 0
    report = result.report(signal.shape[0], result.samples.shape[0], -1.0)
    assert report["input_frames"] == report["output_frames"]
    assert report["net_latency_frames"] == 0
