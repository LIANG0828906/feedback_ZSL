from __future__ import annotations

import numpy as np

from offline_master.dsp import circular_error_deg, frac_delay, wrap_deg
from offline_master.loudness import (
    integrated_loudness,
    true_peak_db,
    true_peak_limiter,
)


def test_wrap_and_circular_error():
    assert wrap_deg(370) == 10
    assert circular_error_deg(179, -179) == 2.0


def test_fractional_delay_shifts_impulse():
    impulse = np.zeros(8)
    impulse[3] = 1.0
    delayed = frac_delay(impulse, 0.5)
    assert delayed[3] == 0.5 and delayed[4] == 0.5
    assert np.allclose(frac_delay(impulse, 0.0), impulse)


def test_limiter_respects_ceiling_and_length():
    sr = 48000
    t = np.arange(sr) / sr
    sine = 0.95 * np.sin(2 * np.pi * 440 * t)
    stereo = np.stack([sine, sine], axis=1)
    limited, info = true_peak_limiter(stereo, sr, -1.0)
    assert limited.shape == stereo.shape
    assert true_peak_db(limited, sr) <= -1.0
    assert info["limiter_triggered_samples"] > 0


def test_integrated_loudness_finite_for_real_signal():
    sr = 48000
    t = np.arange(int(2 * sr)) / sr
    stereo = np.stack(
        [0.2 * np.sin(2 * np.pi * 300 * t)] * 2, axis=1
    )
    lufs = integrated_loudness(stereo, sr)
    assert np.isfinite(lufs) and lufs < 0
