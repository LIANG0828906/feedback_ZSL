"""Exact output fingerprints, first-difference localization and loudness stats."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from typing import Any

import numpy as np
from scipy.signal import butter, sosfilt


def _as_2d(samples: np.ndarray) -> np.ndarray:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.ndim != 2:
        raise ValueError("audio arrays must be one- or two-dimensional")
    return data


def output_sha256(samples: np.ndarray) -> str:
    data = np.ascontiguousarray(_as_2d(samples).astype("<f8", copy=False))
    digest = hashlib.sha256()
    digest.update(
        json.dumps(
            {"shape": list(data.shape), "dtype": "float64-le"},
            sort_keys=True,
            separators=(",", ":"),
        ).encode("ascii")
    )
    digest.update(data.tobytes(order="C"))
    return digest.hexdigest()


def _db(value: float) -> float:
    return max(-120.0, 20.0 * float(np.log10(max(value, 1e-12))))


def _k_weight(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    nyquist = sample_rate * 0.5
    low = min(100.0, nyquist * 0.25)
    high = min(12000.0, nyquist * 0.90)
    if high <= low:
        return np.asarray(samples, dtype=np.float64)
    sos = butter(2, [low, high], btype="bandpass", fs=sample_rate, output="sos")
    return sosfilt(sos, np.asarray(samples, dtype=np.float64), axis=0)


def integrated_lufs(samples: np.ndarray, sample_rate: int) -> float:
    """A deterministic BS.1770-style gated loudness approximation.

    It is used as a comparison statistic, not as a broadcast compliance meter.
    The exact sampling and filter version are recorded with every report.
    """

    data = _as_2d(samples)
    weighted = _k_weight(data, sample_rate)
    block = max(1, int(round(sample_rate * 0.4)))
    step = max(1, int(round(sample_rate * 0.1)))
    if weighted.shape[0] < block:
        starts = [0]
    else:
        starts = list(range(0, weighted.shape[0] - block + 1, step))
    powers = []
    for start in starts:
        chunk = weighted[start : start + block]
        if chunk.shape[0] == 0:
            continue
        powers.append(float(np.mean(np.sum(chunk * chunk, axis=1))))
    if not powers:
        return -120.0
    power_array = np.asarray(powers, dtype=np.float64)
    loudness = -0.691 + 10.0 * np.log10(np.maximum(power_array, 1e-12))
    absolute = loudness > -70.0
    if not np.any(absolute):
        return -120.0
    relative_threshold = (
        -0.691
        + 10.0 * np.log10(float(np.mean(power_array[absolute])))
        - 10.0
    )
    gated = absolute & (loudness >= relative_threshold)
    if not np.any(gated):
        gated = absolute
    return float(
        -0.691 + 10.0 * np.log10(max(float(np.mean(power_array[gated])), 1e-12))
    )


@dataclass(frozen=True)
class AudioStats:
    sample_count: int
    channel_count: int
    peak_abs: float
    peak_dbfs: float
    rms: float
    rms_dbfs: float
    integrated_lufs: float
    sha256: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "sample_count": self.sample_count,
            "channel_count": self.channel_count,
            "peak_abs": self.peak_abs,
            "peak_dbfs": self.peak_dbfs,
            "rms": self.rms,
            "rms_dbfs": self.rms_dbfs,
            "integrated_lufs": self.integrated_lufs,
            "sha256": self.sha256,
        }


def audio_stats(samples: np.ndarray, sample_rate: int) -> AudioStats:
    data = _as_2d(samples)
    peak = float(np.max(np.abs(data))) if data.size else 0.0
    rms = float(np.sqrt(np.mean(data * data))) if data.size else 0.0
    return AudioStats(
        sample_count=int(data.shape[0]),
        channel_count=int(data.shape[1]),
        peak_abs=peak,
        peak_dbfs=_db(peak),
        rms=rms,
        rms_dbfs=_db(rms),
        integrated_lufs=integrated_lufs(data, sample_rate),
        sha256=output_sha256(data),
    )


@dataclass(frozen=True)
class Difference:
    equal: bool
    first_sample: int | None = None
    channel: int | None = None
    segment_index: int | None = None
    segment_size: int | None = None
    left_value: float | None = None
    right_value: float | None = None
    length_difference: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "equal": self.equal,
            "first_sample": self.first_sample,
            "channel": self.channel,
            "segment_index": self.segment_index,
            "segment_size": self.segment_size,
            "left_value": self.left_value,
            "right_value": self.right_value,
            "length_difference": self.length_difference,
        }


def locate_first_difference(
    left: np.ndarray, right: np.ndarray, segment_samples: int
) -> Difference:
    """Locate the first differing sample frame and its deterministic segment."""

    if segment_samples <= 0:
        raise ValueError("segment_samples must be positive")
    a = _as_2d(left)
    b = _as_2d(right)
    length_difference = int(a.shape[0] - b.shape[0])
    common = min(a.shape[0], b.shape[0])
    if a.shape[1] != b.shape[1]:
        return Difference(
            equal=False,
            first_sample=0,
            channel=0,
            segment_index=0,
            segment_size=segment_samples,
            left_value=None,
            right_value=None,
            length_difference=length_difference,
        )
    differing = np.any(a[:common] != b[:common], axis=1)
    indexes = np.flatnonzero(differing)
    if indexes.size:
        sample = int(indexes[0])
        channel = int(np.flatnonzero(a[sample] != b[sample])[0])
        return Difference(
            equal=False,
            first_sample=sample,
            channel=channel,
            segment_index=sample // segment_samples,
            segment_size=segment_samples,
            left_value=float(a[sample, channel]),
            right_value=float(b[sample, channel]),
            length_difference=length_difference,
        )
    if length_difference:
        sample = common
        return Difference(
            equal=False,
            first_sample=sample,
            channel=0,
            segment_index=sample // segment_samples,
            segment_size=segment_samples,
            left_value=None,
            right_value=None,
            length_difference=length_difference,
        )
    return Difference(equal=True, segment_size=segment_samples)

