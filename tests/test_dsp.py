from __future__ import annotations

import numpy as np
import pytest

from scene_audio.camera import CameraPath
from scene_audio.mastering import master_audio
from scene_audio.resample import (
    anchor_alignment_error_samples,
    resample_track,
)
from scene_audio.reverb import ReverbSchedule
from scene_audio.scene import CameraSample, ReverbRegion, SpatialLimits
from scene_audio.spatial import render_spatial_event, spatial_parameters


def test_anchor_resampling_is_sample_accurate() -> None:
    source_rate = 44100
    target_rate = 48000
    source = np.zeros((int(6.0 * source_rate), 1), dtype=np.float64)
    anchors = (
        (0, 0.0, 0),
        (44100, 0.999, 1),
        (88200, 1.998, 2),
        (132300, 2.994, 3),
        (176400, 3.990, 4),
        (220500, 4.995, 5),
        (264600, 6.0, 6),
    )
    from scene_audio.scene import SyncAnchor

    sync_anchors = tuple(SyncAnchor(item[0], item[1]) for item in anchors)
    for source_sample, _, index in anchors:
        if source_sample < source.shape[0]:
            source[source_sample, 0] = float(index + 1)
    rendered = resample_track(
        source,
        source_rate,
        target_rate,
        int(6.0 * target_rate),
        sync_anchors,
    )
    assert rendered.shape == (int(6.0 * target_rate), 1)
    target_times = [item[1] for item in anchors[1:-1]]
    errors = anchor_alignment_error_samples(rendered, target_rate, target_times)
    assert errors == [0.0, 0.0, 0.0, 0.0, 0.0]
    for target_time, (_, _, index) in zip(target_times, anchors[1:-1]):
        expected_index = int(round(target_time * target_rate))
        assert rendered[expected_index, 0] == pytest.approx(float(index + 1))


def test_reverb_boundary_is_continuous() -> None:
    schedule = ReverbSchedule(
        [
            ReverbRegion(0.0, 2.0, 0.7, 0.2),
            ReverbRegion(2.0, 4.0, 1.3, 0.35),
            ReverbRegion(4.0, 6.0, 0.9, 0.25),
        ],
        0.2,
    )
    times = np.linspace(1.89, 2.11, 2001)
    rt60, wet, _ = schedule.values_at(times)
    assert rt60[0] == pytest.approx(0.7)
    assert rt60[-1] == pytest.approx(1.3)
    assert wet[0] == pytest.approx(0.2)
    assert wet[-1] == pytest.approx(0.35)
    assert np.all(np.diff(rt60) >= -1e-14)
    assert np.all(np.diff(wet) >= -1e-14)
    assert float(np.max(np.abs(np.diff(rt60)))) < 0.005
    midpoint_rt = float(
        schedule.values_at(np.asarray([2.0]))[0][0]
    )
    assert midpoint_rt == pytest.approx(1.0)


def test_spatial_model_is_deterministic_and_rate_limited() -> None:
    sample_rate = 48000
    frame_count = 24000
    times = np.arange(frame_count, dtype=np.float64) / sample_rate
    camera = CameraPath(
        [
            CameraSample(
                0.0,
                np.array([0.0, 0.0, 1.7]),
                np.array([0.0, 1.0, 0.0]),
            ),
            CameraSample(
                0.5,
                np.array([0.2, 0.5, 1.7]),
                np.array([0.35, 1.0, 0.0]),
            ),
        ],
        np.array([0.0, 0.0, 1.0]),
    )
    limits = SpatialLimits(
        max_ild_db=10.0,
        max_itd_us=620.0,
        max_ild_rate_db_per_s=60.0,
        max_itd_rate_us_per_s=3500.0,
        head_radius_m=0.0875,
        speed_of_sound_m_s=343.0,
        ild_strength_db=8.0,
    )
    source = np.array([2.0, 3.0, 1.6])
    azimuth, elevation, ild, itd, distance = spatial_parameters(
        times, source, camera, limits
    )
    assert azimuth.shape == elevation.shape == ild.shape == itd.shape
    assert np.max(np.abs(np.diff(ild))) * sample_rate <= 60.0 + 1e-6
    assert np.max(np.abs(np.diff(itd))) * sample_rate <= 3500.0 + 1e-6
    assert np.all(np.abs(ild) <= 10.0)
    assert np.all(np.abs(itd) <= 620.0)
    assert np.all(distance > 0)

    mono = 0.1 * np.sin(
        2.0 * np.pi * 440.0 * times[:2400]
    )
    first = render_spatial_event(
        mono, times[:2400], source, camera, limits, sample_rate
    )
    second = render_spatial_event(
        mono, times[:2400], source, camera, limits, sample_rate
    )
    assert np.array_equal(first.stereo, second.stereo)
    assert np.array_equal(first.ild_db, second.ild_db)
    assert np.array_equal(first.itd_us, second.itd_us)


def test_mastering_holds_loudness_and_peak_with_limiting() -> None:
    sample_rate = 48000
    rng = np.random.default_rng(7)
    raw = rng.standard_normal((sample_rate * 2, 2)) * 0.2
    raw[::12000] *= 8.0
    result = master_audio(raw, sample_rate, -8.0, -1.0)
    assert abs(result.integrated_loudness_lufs - (-8.0)) <= 0.5
    assert result.true_peak_dbtp <= -1.0 + 1e-6
    assert float(np.max(np.abs(result.samples))) <= 10.0 ** (-1.0 / 20.0)
    assert result.suppressed_samples > 0

