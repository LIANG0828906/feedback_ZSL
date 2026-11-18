from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np
from scipy import signal

from .scene import SyncAnchor


@dataclass(frozen=True)
class AnchorMap:
    source_samples: np.ndarray
    target_times: np.ndarray

    @classmethod
    def from_anchors(
        cls,
        anchors: tuple[SyncAnchor, ...],
        source_sample_rate: int,
    ) -> "AnchorMap" | None:
        if not anchors:
            return None
        return cls(
            source_samples=np.asarray(
                [anchor.source_sample for anchor in anchors], dtype=np.float64
            ),
            target_times=np.asarray(
                [anchor.target_time for anchor in anchors], dtype=np.float64
            ),
        )

    def source_at_target_time(self, target_times: np.ndarray | float) -> np.ndarray:
        times = np.asarray(target_times, dtype=np.float64)
        xs = self.target_times
        ys = self.source_samples
        if xs.size == 1:
            return np.full_like(times, ys[0], dtype=np.float64)
        result = np.interp(times, xs, ys)
        first_slope = (ys[1] - ys[0]) / (xs[1] - xs[0])
        last_slope = (ys[-1] - ys[-2]) / (xs[-1] - xs[-2])
        before = times < xs[0]
        after = times > xs[-1]
        result = np.where(before, ys[0] + (times - xs[0]) * first_slope, result)
        result = np.where(after, ys[-1] + (times - xs[-1]) * last_slope, result)
        return result

    def target_time_at_source_sample(
        self, source_samples: np.ndarray | float
    ) -> np.ndarray:
        samples = np.asarray(source_samples, dtype=np.float64)
        xs = self.source_samples
        ys = self.target_times
        if xs.size == 1:
            return np.full_like(samples, ys[0], dtype=np.float64)
        result = np.interp(samples, xs, ys)
        first_slope = (ys[1] - ys[0]) / (xs[1] - xs[0])
        last_slope = (ys[-1] - ys[-2]) / (xs[-1] - xs[-2])
        before = samples < xs[0]
        after = samples > xs[-1]
        result = np.where(before, ys[0] + (samples - xs[0]) * first_slope, result)
        result = np.where(after, ys[-1] + (samples - xs[-1]) * last_slope, result)
        return result


def expected_source_sample(
    anchors: tuple[SyncAnchor, ...],
    target_time: float,
    source_sample_rate: int,
) -> float:
    anchor_map = AnchorMap.from_anchors(anchors, source_sample_rate)
    if anchor_map is None:
        return float(target_time) * float(source_sample_rate)
    return float(anchor_map.source_at_target_time(target_time))


def source_sample_to_target_time(
    anchors: tuple[SyncAnchor, ...],
    source_sample: float,
    source_sample_rate: int,
) -> float:
    anchor_map = AnchorMap.from_anchors(anchors, source_sample_rate)
    if anchor_map is None:
        return float(source_sample) / float(source_sample_rate)
    return float(anchor_map.target_time_at_source_sample(source_sample))


def _antialias_source(samples: np.ndarray, source_rate: int, target_rate: int) -> np.ndarray:
    if target_rate >= source_rate:
        return samples
    cutoff = 0.90 * (target_rate / source_rate)
    sos = signal.butter(8, cutoff, btype="lowpass", fs=1.0, output="sos")
    return signal.sosfiltfilt(sos, samples, axis=0)


def resample_track(
    samples: np.ndarray,
    source_rate: int,
    target_rate: int,
    total_samples: int,
    anchors: tuple[SyncAnchor, ...],
) -> np.ndarray:
    """Resample a complete track to a fixed output grid.

    Sync anchors define a piecewise-linear source-sample to scene-time map.
    Outside the outermost anchors the nearest segment slope is held constant.
    Every channel is processed in float64 and always returns exactly
    ``total_samples`` frames.
    """

    if samples.ndim != 2:
        raise ValueError("samples must be (frames, channels)")
    if samples.shape[0] == 0:
        return np.zeros((total_samples, samples.shape[1]), dtype=np.float64)
    filtered = _antialias_source(samples.astype(np.float64, copy=False), source_rate, target_rate)
    times = np.arange(total_samples, dtype=np.float64) / float(target_rate)
    anchor_map = AnchorMap.from_anchors(anchors, source_rate)
    if anchor_map is None:
        source_coordinates = times * float(source_rate)
    else:
        source_coordinates = anchor_map.source_at_target_time(times)

    base = np.arange(filtered.shape[0], dtype=np.float64)
    output = np.empty((total_samples, filtered.shape[1]), dtype=np.float64)
    for channel in range(filtered.shape[1]):
        output[:, channel] = np.interp(
            source_coordinates,
            base,
            filtered[:, channel],
            left=0.0,
            right=0.0,
        )
    return output


def anchor_alignment_error_samples(
    rendered: np.ndarray,
    target_rate: int,
    target_times_s: list[float],
) -> list[float]:
    """Return nearest-grid anchor error in output samples.

    This helper is intended for deterministic diagnostics and tests. It finds
    the strongest local sample around each expected anchor coordinate.
    """

    errors: list[float] = []
    mono = np.mean(np.abs(rendered), axis=1) if rendered.ndim == 2 else np.abs(rendered)
    for target_time in target_times_s:
        center = int(round(target_time * target_rate))
        left = max(0, center - 2)
        right = min(mono.shape[0], center + 3)
        if right <= left:
            errors.append(math.inf)
            continue
        peak = left + int(np.argmax(mono[left:right]))
        errors.append(abs(float(peak - center)))
    return errors
