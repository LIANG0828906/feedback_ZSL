from __future__ import annotations

import numpy as np

from offline_master.dsp import frac_delay
from offline_master.phase import correct_member, estimate_relative, low_band_energy
from offline_master.spatial import (
    StreamingBinauralPanner,
    params_for_angles,
    source_relative_pose,
    stability_report,
)


def test_binaural_params_deterministic_and_continuous():
    anchor = (0.2, 1.4, 0.0)
    rows_a = params_for_angles(anchor, list(range(0, 360, 10)), 48000)
    rows_b = params_for_angles(anchor, list(range(0, 360, 10)), 48000)
    for a, b in zip(rows_a, rows_b):
        assert a == b
    report = stability_report(rows_a, 0.5)
    assert report["within_threshold"]
    assert report["side_reversal_angles"] == []


def test_pose_rotates_full_circle():
    azimuth_front, _, _ = source_relative_pose((1.0, 1.0, 0.0), 0)
    azimuth_rear, _, _ = source_relative_pose((1.0, 1.0, 0.0), 180)
    assert abs(azimuth_front) < 1e-9
    assert abs(abs(azimuth_rear) - 180.0) < 1e-9


def test_streaming_panner_preserves_length():
    panner = StreamingBinauralPanner(48000)
    rows = params_for_angles((0.2, 1.4, 0.0), list(range(0, 360, 10)), 48000)
    block = np.random.default_rng(0).standard_normal(4800)
    out = panner.process_block(block, rows[0], rows[1])
    assert out.shape == (4800, 2)
    assert np.isfinite(out).all()


def test_phase_estimator_recovers_polarity_and_delay():
    sr = 48000
    duration = int(3.0 * sr)
    t = np.arange(duration) / sr
    reference = np.sin(2 * np.pi * 60 * t) * np.exp(-0.5 * t)
    member = -frac_delay(reference, 1.25)
    estimate = estimate_relative(
        reference, member, sr, (int(0.2 * sr), int(2.8 * sr)), center_lag=1
    )
    assert estimate["polarity"] == -1
    assert abs(estimate["relative_delay_samples"] - 1.25) < 0.35
    corrected = correct_member(member, estimate)
    # Polarity restored and transient displacement bounded by one sample.
    assert np.corrcoef(reference[100:-100], corrected[100:-100])[0, 1] > 0.95


def test_low_band_energy_gain_after_phase_correction():
    sr = 48000
    n = int(3.0 * sr)
    t = np.arange(n) / sr
    reference = np.sin(2 * np.pi * 60 * t)
    member = -0.9 * reference.copy()
    section = (int(0.5 * sr), int(2.5 * sr))
    mixed_before = reference + member
    before = low_band_energy(
        mixed_before, sr, (0, section[1] - section[0])
    ) + 1e-20
    estimate = estimate_relative(reference, member, sr, section)
    corrected = correct_member(member, estimate)
    after = low_band_energy(
        reference + corrected, sr, (0, section[1] - section[0])
    )
    gain_db = 10 * np.log10(after / before)
    assert gain_db > 6.0
