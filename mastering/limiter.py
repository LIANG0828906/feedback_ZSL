from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from scipy.ndimage import minimum_filter1d, uniform_filter1d
from scipy.signal import resample_poly


@dataclass(frozen=True)
class LimiterResult:
    samples: np.ndarray
    triggered_samples: int
    max_attenuation_db: float
    input_true_peak_dbtp: float
    output_true_peak_dbtp: float
    lookahead_samples: int
    oversampling: int
    second_pass_correction: bool
    correction_db: float

    def report(self, input_frames: int, output_frames: int, ceiling_dbtp: float) -> dict:
        output_peak = (
            self.output_true_peak_dbtp
            if np.isfinite(self.output_true_peak_dbtp)
            else None
        )
        return {
            "ceiling_dbtp": ceiling_dbtp,
            "delay_compensated_frames": self.lookahead_samples,
            "input_frames": input_frames,
            "lookahead_samples": self.lookahead_samples,
            "max_attenuation_db": self.max_attenuation_db,
            "net_latency_frames": 0,
            "output_frames": output_frames,
            "output_true_peak_dbtp": output_peak,
            "oversampling": self.oversampling,
            "second_pass_correction": self.second_pass_correction,
            "correction_db": self.correction_db,
            "triggered_input_samples": self.triggered_samples,
        }


def _db(value: float) -> float:
    if value <= 0:
        return float("-inf")
    return 20.0 * float(np.log10(value))


def true_peak_envelope(samples: np.ndarray, oversampling: int = 4) -> np.ndarray:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim != 2:
        raise ValueError("samples must be two-dimensional")
    frames = data.shape[0]
    if frames == 0:
        return np.zeros((0, data.shape[1]), dtype=np.float64)
    envelope = np.zeros((frames, data.shape[1]), dtype=np.float64)
    for channel in range(data.shape[1]):
        for start, end, grouped in _oversampled_groups(
            data[:, channel], oversampling
        ):
            count = end - start
            if grouped.size > count:
                envelope[start:end, channel] = np.maximum(
                    grouped[:count], grouped[1 : count + 1]
                )
            else:
                current = grouped[:count].copy()
                if current.size > 1:
                    current[:-1] = np.maximum(current[:-1], current[1:])
                envelope[start:end, channel] = current
    return envelope


def _oversampled_groups(
    channel: np.ndarray,
    oversampling: int,
    chunk_size: int = 262_144,
):
    """Yield aligned groups of oversampled magnitudes without a full 4x allocation."""
    frames = channel.size
    pad = max(64, 10 * oversampling + 16)
    for start in range(0, frames, chunk_size):
        end = min(frames, start + chunk_size)
        left = max(0, start - pad)
        right = min(frames, end + pad)
        oversampled = resample_poly(channel[left:right], oversampling, 1)
        first_group = start - left
        available_groups = min(end - start + 1, right - left - first_group)
        octets = oversampled[
            first_group * oversampling : (first_group + available_groups)
            * oversampling
        ]
        grouped = np.max(
            np.abs(octets).reshape(available_groups, oversampling), axis=1
        )
        yield start, end, grouped


def true_peak_dbtp(samples: np.ndarray, oversampling: int = 4) -> float:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim != 2:
        raise ValueError("samples must be two-dimensional")
    if data.size == 0:
        return float("-inf")
    peak = 0.0
    for channel in range(data.shape[1]):
        for _, _, grouped in _oversampled_groups(data[:, channel], oversampling):
            peak = max(peak, float(np.max(grouped)))
    return _db(peak)


def limit_true_peak(
    samples: np.ndarray,
    sample_rate: int,
    ceiling_dbtp: float,
    *,
    oversampling: int = 4,
    lookahead_ms: float = 5.0,
    release_ms: float = 50.0,
    safety_margin_db: float = 0.02,
) -> LimiterResult:
    """Zero-net-latency look-ahead limiter driven by an oversampled peak envelope."""
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim != 2:
        raise ValueError("samples must be two-dimensional")
    frames = data.shape[0]
    if frames == 0:
        zeros = np.zeros_like(data)
        return LimiterResult(
            zeros,
            0,
            0.0,
            float("-inf"),
            float("-inf"),
            0,
            oversampling,
            False,
            0.0,
        )

    envelope = true_peak_envelope(data, oversampling=oversampling)
    peak_per_frame = np.max(envelope, axis=1)
    input_peak = float(np.max(peak_per_frame))
    design_ceiling = float(10.0 ** ((ceiling_dbtp - safety_margin_db) / 20.0))
    requested_ceiling = float(10.0 ** (ceiling_dbtp / 20.0))

    required = np.ones(frames, dtype=np.float64)
    over_ceiling = peak_per_frame > design_ceiling
    required[over_ceiling] = design_ceiling / peak_per_frame[over_ceiling]
    lookahead = max(1, int(round(lookahead_ms * 0.001 * sample_rate)))
    release = max(1, int(round(release_ms * 0.001 * sample_rate)))
    hold = minimum_filter1d(required, size=2 * lookahead + 1, mode="nearest")
    smoothed = uniform_filter1d(hold, size=release, mode="nearest")
    gain = np.minimum(np.minimum(smoothed, hold), required)
    gain = np.clip(gain, 0.0, 1.0)

    output = data * gain[:, None]
    second_pass = False
    correction_db = 0.0
    measured = true_peak_dbtp(output, oversampling=oversampling)
    output_peak = measured
    if measured > ceiling_dbtp - safety_margin_db * 0.5:
        current_peak = float(10.0 ** (measured / 20.0))
        correction = min(1.0, (requested_ceiling * 10.0 ** (-0.005 / 20.0)) / current_peak)
        if correction < 1.0:
            output *= correction
            gain *= correction
            second_pass = True
            correction_db = float(_db(correction))
            output_peak = true_peak_dbtp(output, oversampling=oversampling)

    triggered = int(np.count_nonzero(gain < 1.0 - 1e-12))
    max_attenuation = float(_db(float(np.min(gain)))) if triggered else 0.0
    return LimiterResult(
        samples=output,
        triggered_samples=triggered,
        max_attenuation_db=max_attenuation,
        input_true_peak_dbtp=_db(input_peak),
        output_true_peak_dbtp=output_peak,
        lookahead_samples=lookahead,
        oversampling=oversampling,
        second_pass_correction=second_pass,
        correction_db=correction_db,
    )
