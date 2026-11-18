from __future__ import annotations

import math

import numpy as np

from .errors import DetectionFailed


def detect_sync_pulse_start(
    samples: np.ndarray,
    sample_rate: int,
    expected_sample: float | None = None,
    search_radius_seconds: float = 0.15,
) -> tuple[float, float]:
    """Detect the first significant pulse onset in the original-rate track.

    Returns a fractional sample position and the detected peak amplitude.
    """
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim != 2 or data.shape[0] == 0:
        raise DetectionFailed("track has no samples")
    if expected_sample is None:
        search_start = 0
        search_end = min(data.shape[0], max(1, int(math.ceil(2.0 * sample_rate))))
    else:
        radius = max(1, int(math.ceil(search_radius_seconds * sample_rate)))
        expected = int(round(float(expected_sample)))
        search_start = max(0, expected - radius)
        search_end = min(data.shape[0], expected + radius + 1)
    if search_start >= data.shape[0] or search_end <= search_start:
        raise DetectionFailed("expected pulse window lies outside the available samples")
    magnitude = np.max(np.abs(data[search_start:search_end]), axis=1)
    if magnitude.size == 0:
        raise DetectionFailed("empty synchronization search window")

    peak = float(np.max(magnitude))
    if not math.isfinite(peak) or peak < 1e-8:
        raise DetectionFailed("no pulse energy found in the first two seconds")

    floor_frames = min(magnitude.size, max(1, int(round(0.050 * sample_rate))))
    noise_floor = float(np.median(magnitude[:floor_frames]))
    threshold = max(peak * 1e-4, noise_floor * 8.0, 1e-9)
    above = np.flatnonzero(magnitude >= threshold)
    if above.size == 0:
        raise DetectionFailed("pulse threshold was not crossed")
    index = int(above[0])
    if index == 0:
        return float(search_start), peak

    low = float(magnitude[index - 1])
    high = float(magnitude[index])
    if high <= low:
        return float(search_start + index), peak
    fraction = (threshold - low) / (high - low)
    fraction = min(max(fraction, 0.0), 1.0)
    return float(search_start + index - 1) + fraction, peak


def integer_alignment(
    detected_output_position: float,
    reference_output_position: int,
) -> tuple[int, float]:
    shift = int(round(float(reference_output_position) - detected_output_position))
    residual = detected_output_position + shift - float(reference_output_position)
    return shift, float(residual)
