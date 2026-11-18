from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from scipy import ndimage, signal

from .errors import EXIT_RENDER, BusinessError


def _k_weighting_sos(sample_rate: int) -> np.ndarray:
    gain_db = 3.999843853973347
    quality = 0.7071752369554196
    center_hz = 1681.974450955533
    k = np.tan(np.pi * center_hz / sample_rate)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh ** 0.499666774155
    a0 = 1.0 + k / quality + k * k
    shelf = np.asarray(
        [
            (vh + vb * k / quality + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / quality + k * k) / a0,
            1.0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / quality + k * k) / a0,
        ]
    )

    center_hz = 38.13547087602444
    quality = 0.5003270373238773
    k = np.tan(np.pi * center_hz / sample_rate)
    a0 = 1.0 + k / quality + k * k
    highpass = np.asarray(
        [
            1.0,
            -2.0,
            1.0,
            1.0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / quality + k * k) / a0,
        ]
    )
    return np.vstack((shelf, highpass))


def _as_stereo(x: np.ndarray) -> np.ndarray:
    if x.ndim == 1:
        return x[:, None]
    return x


def integrated_loudness_lufs(x: np.ndarray, sample_rate: int) -> float:
    """ITU-R BS.1770-4 integrated loudness for L/R (and optional extra channels)."""

    data = _as_stereo(np.asarray(x, dtype=np.float64))
    if data.shape[0] == 0:
        return -200.0
    filtered = signal.sosfilt(_k_weighting_sos(sample_rate), data, axis=0)
    block = max(1, int(round(0.400 * sample_rate)))
    hop = max(1, int(round(0.100 * sample_rate)))
    if filtered.shape[0] < block:
        filtered = np.pad(filtered, ((0, block - filtered.shape[0]), (0, 0)))
    starts = range(0, filtered.shape[0] - block + 1, hop)
    block_energy = np.asarray(
        [np.mean(filtered[start : start + block] ** 2, axis=0) for start in starts],
        dtype=np.float64,
    )
    channel_energy = np.sum(block_energy, axis=1)
    if channel_energy.size == 0:
        return -200.0
    block_loudness = -0.691 + 10.0 * np.log10(channel_energy + 1.0e-30)
    absolute = block_loudness > -70.0
    if not np.any(absolute):
        return -200.0
    absolute_energy = float(np.mean(channel_energy[absolute]))
    absolute_loudness = -0.691 + 10.0 * np.log10(absolute_energy + 1.0e-30)
    relative = absolute & (block_loudness > absolute_loudness - 10.0)
    if not np.any(relative):
        return -200.0
    gated_energy = float(np.mean(channel_energy[relative]))
    return float(-0.691 + 10.0 * np.log10(gated_energy + 1.0e-30))


def true_peak_dbtp(x: np.ndarray, sample_rate: int, oversample: int = 4) -> float:
    data = _as_stereo(np.asarray(x, dtype=np.float64))
    if data.shape[0] == 0:
        return -200.0
    if oversample > 1:
        upsampled = signal.resample_poly(
            data,
            oversample,
            1,
            axis=0,
            window=("kaiser", 5.0),
            padtype="constant",
        )
    else:
        upsampled = data
    peak = float(np.max(np.abs(upsampled))) if upsampled.size else 0.0
    return 20.0 * np.log10(peak + 1.0e-30)


@dataclass
class LimiterResult:
    signal: np.ndarray
    triggered_samples: int
    lookahead_samples: int
    maximum_reduction_db: float


def lookahead_limiter(
    x: np.ndarray,
    sample_rate: int,
    ceiling_linear: float,
    lookahead_ms: float,
    release_ms: float,
) -> LimiterResult:
    data = _as_stereo(np.asarray(x, dtype=np.float64))
    lookahead = max(1, int(round(lookahead_ms * sample_rate / 1000.0)))
    padded = np.pad(data, ((lookahead, lookahead), (0, 0)))
    envelope = np.max(np.abs(padded), axis=1)
    desired = np.minimum(1.0, ceiling_linear / np.maximum(envelope, 1.0e-30))
    minimum = ndimage.minimum_filter1d(
        desired,
        size=2 * lookahead + 1,
        mode="nearest",
    )
    release_alpha = np.exp(-1.0 / max(1.0, release_ms * sample_rate / 1000.0))
    released = signal.lfilter(
        [1.0 - release_alpha],
        [1.0, -release_alpha],
        minimum,
        zi=[release_alpha * float(minimum[0])],
    )[0]
    gain = np.minimum(released, desired)
    gain = np.clip(gain, 0.0, 1.0)
    padded_output = padded * gain[:, None]
    output = padded_output[lookahead : lookahead + data.shape[0]]
    original_triggered = np.count_nonzero(desired[lookahead : lookahead + data.shape[0]] < 1.0 - 1.0e-12)
    maximum_reduction = -20.0 * np.log10(float(np.min(gain)) + 1.0e-30)
    return LimiterResult(
        signal=output,
        triggered_samples=int(original_triggered),
        lookahead_samples=lookahead,
        maximum_reduction_db=maximum_reduction,
    )


@dataclass
class MasterResult:
    signal: np.ndarray
    integrated_loudness_lufs: float
    true_peak_dbtp: float
    target_loudness_lufs: float
    loudness_deviation_lu: float
    limiter_applied: bool
    limiter_triggered_samples: int
    limiter_cumulative_triggered_samples: int
    limiter_passes: int
    limiter_lookahead_samples: int
    maximum_limiter_reduction_db: float


def master_bus(
    raw_mix: np.ndarray,
    sample_rate: int,
    target_loudness_lufs: float,
    true_peak_limit_dbtp: float,
    lookahead_ms: float,
    release_ms: float,
) -> MasterResult:
    data = np.asarray(raw_mix, dtype=np.float64).copy()
    if data.ndim == 1:
        data = data[:, None]
    if data.shape[0] == 0 or float(np.max(np.abs(data))) <= 1.0e-15:
        raise BusinessError(
            "E_MASTER_SILENT",
            "master",
            "the mixed bus is silent",
            EXIT_RENDER,
        )
    peak_limit = 10.0 ** (true_peak_limit_dbtp / 20.0)
    limiter_triggered = 0
    limiter_cumulative_triggered = 0
    limiter_passes = 0
    lookahead_samples = 0
    maximum_reduction = 0.0
    limiter_applied = False

    for _ in range(16):
        loudness = integrated_loudness_lufs(data, sample_rate)
        if loudness <= -100.0:
            raise BusinessError(
                "E_MASTER_SILENT", "master", "cannot measure loudness", EXIT_RENDER
            )
        correction = target_loudness_lufs - loudness
        if abs(correction) > 0.01:
            data *= 10.0 ** (correction / 20.0)
        peak = true_peak_dbtp(data, sample_rate)
        if peak <= true_peak_limit_dbtp:
            break
        result = lookahead_limiter(
            data,
            sample_rate,
            peak_limit * 10.0 ** (-0.15 / 20.0),
            lookahead_ms,
            release_ms,
        )
        data = result.signal
        limiter_applied = True
        limiter_passes += 1
        limiter_triggered = max(limiter_triggered, result.triggered_samples)
        limiter_cumulative_triggered += result.triggered_samples
        lookahead_samples = result.lookahead_samples
        maximum_reduction = max(maximum_reduction, result.maximum_reduction_db)

    loudness = integrated_loudness_lufs(data, sample_rate)
    peak = true_peak_dbtp(data, sample_rate)
    if peak > true_peak_limit_dbtp:
        over_before_trim = np.count_nonzero(
            np.max(np.abs(data), axis=1) > peak_limit
        )
        data *= peak_limit / (10.0 ** (peak / 20.0))
        peak = true_peak_dbtp(data, sample_rate)
        limiter_applied = True
        limiter_passes += 1
        if over_before_trim > 0:
            limiter_triggered = max(limiter_triggered, int(over_before_trim))
        limiter_cumulative_triggered += int(over_before_trim)
        maximum_reduction = max(
            maximum_reduction,
            max(0.0, peak - true_peak_limit_dbtp),
        )
        if limiter_triggered == 0:
            limiter_triggered = 1

    loudness = integrated_loudness_lufs(data, sample_rate)
    correction = target_loudness_lufs - loudness
    if abs(correction) > 0.01:
        candidate = data * 10.0 ** (correction / 20.0)
        candidate_peak = true_peak_dbtp(candidate, sample_rate)
        if candidate_peak <= true_peak_limit_dbtp:
            data = candidate
            loudness = integrated_loudness_lufs(data, sample_rate)
            peak = candidate_peak

    deviation = loudness - target_loudness_lufs
    if abs(deviation) > 0.5 + 1.0e-9:
        raise BusinessError(
            "E_LOUDNESS_TARGET_UNREACHABLE",
            "master",
            f"loudness deviation {deviation:.3f} LU exceeds 0.5 LU while "
            f"respecting the {true_peak_limit_dbtp:.2f} dBTP ceiling",
            EXIT_RENDER,
        )
    if peak > true_peak_limit_dbtp + 1.0e-9:
        raise BusinessError(
            "E_TRUE_PEAK_EXCEEDED",
            "master",
            f"true peak {peak:.3f} dBTP exceeds limit {true_peak_limit_dbtp:.3f} dBTP",
            EXIT_RENDER,
        )
    return MasterResult(
        signal=data,
        integrated_loudness_lufs=loudness,
        true_peak_dbtp=peak,
        target_loudness_lufs=target_loudness_lufs,
        loudness_deviation_lu=deviation,
        limiter_applied=limiter_applied,
        limiter_triggered_samples=limiter_triggered,
        limiter_cumulative_triggered_samples=limiter_cumulative_triggered,
        limiter_passes=limiter_passes,
        limiter_lookahead_samples=lookahead_samples,
        maximum_limiter_reduction_db=maximum_reduction,
    )

