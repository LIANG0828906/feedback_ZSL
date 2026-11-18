from __future__ import annotations

from dataclasses import dataclass

import numpy as np


@dataclass(frozen=True)
class DetectionResult:
    success: bool
    onset_sample: int | None
    peak: float
    threshold: float
    search_start: int
    search_stop: int


def detect_onset(
    mono: np.ndarray,
    sample_rate: int,
    expected_source_sample: float,
    threshold_fraction: float,
    search_ms: float,
) -> DetectionResult:
    search_samples = max(1, int(round(search_ms * 0.001 * sample_rate)))
    center = int(round(expected_source_sample))
    start = max(0, center - search_samples)
    stop = min(mono.shape[0], center + search_samples + 1)
    if stop <= start:
        return DetectionResult(False, None, 0.0, 0.0, start, stop)
    region = np.abs(mono[start:stop])
    peak = float(np.max(region)) if region.size else 0.0
    if peak < 1e-7:
        return DetectionResult(False, None, peak, 0.0, start, stop)
    threshold = peak * threshold_fraction
    above = np.flatnonzero(region >= threshold)
    if above.size == 0:
        return DetectionResult(False, None, peak, threshold, start, stop)
    local = int(above[0])
    # Move to the first local rising edge near the threshold crossing. This
    # avoids selecting a quiet pre-echo on a zero-phase anti-alias filter.
    while local > 0 and region[local - 1] >= threshold * 0.5:
        local -= 1
    return DetectionResult(
        True,
        start + local,
        peak,
        threshold,
        start,
        stop,
    )

