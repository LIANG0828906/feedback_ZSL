from __future__ import annotations

import math

import numpy as np
import pytest

from broadcast_loudness.analyzer import (
    BlockMeasurement,
    IntegratedTracker,
    integrated_series,
)
from broadcast_loudness.dsp import (
    K_PRE_A,
    K_PRE_B,
    K_RLB_A,
    K_RLB_B,
    _utc_resample_4x,
    block_bounds,
    direct_biquad_reference,
    k_weight_fft,
    lufs_from_energy,
)
from broadcast_loudness.params import AnalysisParameters
from broadcast_loudness.wavio import make_wav_bytes, read_wav


def test_fft_biquad_matches_direct_form(tmp_path) -> None:
    rng = np.random.default_rng(9)
    samples = rng.standard_normal(4096) * 0.1
    fast = k_weight_fft(samples[:, None])[:, 0]
    reference = direct_biquad_reference(samples, K_PRE_B, K_PRE_A)
    reference = direct_biquad_reference(reference, K_RLB_B, K_RLB_A)
    assert np.max(np.abs(fast - reference)) < 1e-11


def test_four_x_true_peak_estimator() -> None:
    time = np.arange(48_000) / 48_000.0
    sine = 0.5 * np.sin(2.0 * np.pi * 1_000.0 * time)
    resampled = _utc_resample_4x(sine, 4)
    assert math.isclose(float(np.max(np.abs(resampled))), 0.5, abs_tol=1e-9)


def test_observation_blocks_are_400ms_with_100ms_hop() -> None:
    bounds = block_bounds(48_000 * 2, 19_200, 4_800)
    assert bounds[0] == (0, 19_200)
    assert bounds[1] == (4_800, 24_000)
    assert bounds[-1] == (76_800, 96_000)
    assert all(end - start == 19_200 for start, end in bounds)


def test_integrated_two_stage_gating() -> None:
    measurements = []
    for index, loudness in enumerate((-80.0, -30.0, -20.0, -19.0)):
        energy = 10.0 ** ((loudness + 0.691) / 10.0)
        measurements.append(
            BlockMeasurement(
                index=index,
                start_sample=index * 4_800,
                end_sample=index * 4_800 + 19_200,
                sync_marker=f"s{index}",
                content_hash=f"c{index}",
                mean_square=energy,
                momentary_lufs=loudness,
                short_lufs=None,
                true_peak=None,  # type: ignore[arg-type]
                measurement_hash=f"m{index}",
            )
        )
    points = integrated_series(measurements, -70.0, -10.0)
    assert points[0].absolute_gated_count == 0
    assert points[1].absolute_gated_count == 1
    assert points[2].absolute_gated_count == 2
    assert points[3].absolute_gated_count == 3
    expected = lufs_from_energy(
        sum(item.mean_square for item in measurements[1:]) / 3.0
    )
    assert points[3].integrated_lufs == pytest.approx(expected, abs=1e-12)
    assert points[3].relative_gated_count == 3


def test_streaming_integrated_tracker_matches_naive_prefix() -> None:
    rng = np.random.default_rng(12)
    loudness_values = rng.uniform(-35.0, -5.0, 600)
    measurements = []
    for index, loudness in enumerate(loudness_values):
        energy = 10.0 ** ((loudness + 0.691) / 10.0)
        measurements.append(
            BlockMeasurement(
                index=index,
                start_sample=index * 4_800,
                end_sample=index * 4_800 + 19_200,
                sync_marker=f"s{index}",
                content_hash=f"c{index}",
                mean_square=energy,
                momentary_lufs=loudness,
                short_lufs=None,
                true_peak=None,  # type: ignore[arg-type]
                measurement_hash=f"m{index}",
            )
        )
    tracker = IntegratedTracker(-70.0, -10.0)
    streamed = [tracker.push(measurement) for measurement in measurements]
    absolute: list[float] = []
    for index, measurement in enumerate(measurements):
        if measurement.momentary_lufs >= -70.0:
            absolute.append(measurement.mean_square)
        if index in (0, 127, 128, 255, 599):
            threshold = lufs_from_energy(sum(absolute) / len(absolute)) - 10.0
            selected = [
                value for value in absolute if lufs_from_energy(value) > threshold
            ]
            expected = lufs_from_energy(sum(selected) / len(selected))
            assert streamed[index].integrated_lufs == pytest.approx(expected, abs=1e-12)


def test_true_peak_can_exceed_sample_peak(tmp_path) -> None:
    from broadcast_loudness.analyzer import analyze_audio

    samples = np.zeros((48_000, 1))
    samples[0, 0] = 0.5
    path = tmp_path / "impulse.wav"
    path.write_bytes(make_wav_bytes(samples, 24))
    audio = read_wav(path)
    params = AnalysisParameters.for_channels(1)
    result = analyze_audio(audio, params)
    assert result.blocks[0].true_peak_dbtp is not None
    assert result.blocks[0].true_peak_dbtp > 20.0 * math.log10(0.5)
