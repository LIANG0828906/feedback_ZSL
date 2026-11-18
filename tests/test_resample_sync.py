from __future__ import annotations

import numpy as np

from mastering.manifest import DriftAnchor
from mastering.resample import (
    output_to_source_positions,
    resample_to_rate,
    source_to_output_sample,
    warp_to_output_timeline,
)
from mastering.sync import detect_sync_pulse_start, integer_alignment


def test_drift_mapping_hits_every_anchor():
    anchors = (
        DriftAnchor(48000, 44130),
        DriftAnchor(96000, 88270),
        DriftAnchor(144000, 132410),
    )
    query = np.array([anchor.output_sample for anchor in anchors], dtype=np.float64)
    mapped = output_to_source_positions(query, anchors, 44100, 48000)
    assert np.array_equal(mapped, np.array([44130.0, 88270.0, 132410.0]))
    for anchor in anchors:
        inverse = source_to_output_sample(
            anchor.source_sample, anchors, 44100, 48000
        )
        assert abs(inverse - anchor.output_sample) < 1e-9


def test_drift_impulses_remain_within_two_output_samples_of_anchors():
    source_rate = 44100
    output_rate = 48000
    anchors = (
        DriftAnchor(12000, 11020),
        DriftAnchor(24000, 22050),
        DriftAnchor(36000, 33080),
    )
    source = np.zeros((46000, 1), dtype=np.float64)
    for anchor in anchors:
        source[anchor.source_sample, 0] = 0.8
    resampled = resample_to_rate(source, source_rate, output_rate)
    warped = warp_to_output_timeline(
        resampled, anchors, source_rate, output_rate, frames=42000
    )
    for anchor in anchors:
        region = warped[
            anchor.output_sample - 12 : anchor.output_sample + 13, 0
        ]
        peak_offset = int(np.argmax(np.abs(region))) - 12
        assert abs(peak_offset) <= 2


def test_sync_detection_and_integer_alignment():
    sample_rate = 48000
    time = np.arange(sample_rate * 2) / sample_rate
    signal = np.zeros((time.size, 1), dtype=np.float64)
    start = 5003
    burst = (time[:2000] < 0.03).astype(np.float64)
    burst *= np.sin(2.0 * np.pi * 1000.0 * time[:2000])
    signal[start : start + 2000, 0] = 0.4 * burst
    detected, _ = detect_sync_pulse_start(
        signal, sample_rate, expected_sample=5000
    )
    shift, residual = integer_alignment(detected, 5000)
    assert abs(detected - start) <= 1.0
    assert abs(residual) <= 0.5
    assert start + shift == 5000 + round(residual)
