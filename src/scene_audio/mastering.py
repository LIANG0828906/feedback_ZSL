from __future__ import annotations

import math
from dataclasses import dataclass
from fractions import Fraction

import numpy as np
from scipy import signal
from scipy.ndimage import maximum_filter1d


K_STAGE1_B = np.asarray(
    [1.53512485958697, -2.69169618940638, 1.19839281085285],
    dtype=np.float64,
)
K_STAGE1_A = np.asarray(
    [1.0, -1.69065929318241, 0.73248077421585],
    dtype=np.float64,
)
K_STAGE2_B = np.asarray([1.0, -2.0, 1.0], dtype=np.float64)
K_STAGE2_A = np.asarray(
    [1.0, -1.99004745483398, 0.99007225036621],
    dtype=np.float64,
)


def _to_48k(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    if sample_rate == 48000:
        return samples
    ratio = Fraction(48000, int(sample_rate)).limit_denominator(2000)
    return signal.resample_poly(
        samples, ratio.numerator, ratio.denominator, axis=0
    )


def integrated_loudness_lufs(samples: np.ndarray, sample_rate: int) -> float:
    if samples.ndim == 1:
        samples = samples[:, None]
    if samples.shape[0] == 0:
        return -math.inf
    measured = _to_48k(samples.astype(np.float64, copy=False), sample_rate)
    measured = signal.lfilter(K_STAGE1_B, K_STAGE1_A, measured, axis=0)
    measured = signal.lfilter(K_STAGE2_B, K_STAGE2_A, measured, axis=0)
    block = int(round(0.400 * 48000))
    hop = int(round(0.100 * 48000))
    if measured.shape[0] < block:
        measured = np.pad(measured, ((0, block - measured.shape[0]), (0, 0)))
    starts = list(range(0, measured.shape[0] - block + 1, hop))
    if not starts:
        starts = [0]
    energies = np.empty(len(starts), dtype=np.float64)
    for index, start in enumerate(starts):
        window = measured[start : start + block]
        energies[index] = float(np.sum(np.mean(window * window, axis=0)))
    with np.errstate(divide="ignore"):
        block_loudness = -0.691 + 10.0 * np.log10(
            np.maximum(energies, 1e-30)
        )
    absolute = block_loudness > -70.0
    if not np.any(absolute):
        return -math.inf
    absolute_mean = float(np.mean(energies[absolute]))
    relative_threshold = -0.691 + 10.0 * math.log10(
        max(absolute_mean, 1e-30)
    ) - 10.0
    gated = absolute & (block_loudness > relative_threshold)
    if not np.any(gated):
        return -0.691 + 10.0 * math.log10(max(absolute_mean, 1e-30))
    return -0.691 + 10.0 * math.log10(
        max(float(np.mean(energies[gated])), 1e-30)
    )


def true_peak_dbtp(samples: np.ndarray) -> float:
    if samples.size == 0:
        return -math.inf
    oversampled = signal.resample_poly(
        samples.astype(np.float64, copy=False), 4, 1, axis=0
    )
    peak = float(np.max(np.abs(oversampled)))
    if peak <= 0.0:
        return -math.inf
    return 20.0 * math.log10(peak)


def _lookahead_limit(
    samples: np.ndarray,
    sample_rate: int,
    ceiling_linear: float,
) -> tuple[np.ndarray, int, int]:
    lookahead = max(1, int(round(0.005 * sample_rate)))
    peak = np.max(np.abs(samples), axis=1)
    envelope = maximum_filter1d(
        peak,
        size=2 * lookahead + 1,
        mode="constant",
        cval=0.0,
    )
    desired = np.minimum(
        1.0, ceiling_linear / np.maximum(envelope, 1e-15)
    )
    release_seconds = 0.05
    alpha = math.exp(-1.0 / (release_seconds * sample_rate))
    smoothed, _ = signal.lfilter(
        [1.0 - alpha],
        [1.0, -alpha],
        desired,
        zi=[alpha * desired[0]],
    )
    gain = np.minimum(desired, smoothed)
    output = samples * gain[:, None]
    changed = np.abs(output - samples) > 1e-14
    suppressed_samples = int(np.count_nonzero(changed))
    suppressed_frames = int(np.count_nonzero(np.any(changed, axis=1)))
    return output, suppressed_samples, suppressed_frames


@dataclass
class MasterResult:
    samples: np.ndarray
    integrated_loudness_lufs: float
    true_peak_dbtp: float
    suppressed_samples: int
    suppressed_frames: int
    pre_limit_gain_db: float
    iterations: int


def master_audio(
    raw: np.ndarray,
    sample_rate: int,
    target_lufs: float,
    true_peak_limit_dbtp: float,
) -> MasterResult:
    """Loudness-normalize, peak-limit, and quantize-safe finalize doubles."""

    if raw.shape[0] == 0:
        raise ValueError("cannot master empty audio")
    effective_limit_db = true_peak_limit_dbtp - 0.08
    ceiling = 10.0 ** (effective_limit_db / 20.0)
    gain = 1.0
    output = raw.copy()
    suppressed_samples = 0
    suppressed_frames = 0
    iterations = 0
    for iterations in range(1, 10):
        scaled = raw * gain
        limited, changed_samples, changed_frames = _lookahead_limit(
            scaled, sample_rate, ceiling
        )
        peak = true_peak_dbtp(limited)
        if math.isfinite(peak) and peak > effective_limit_db:
            limited *= 10.0 ** ((effective_limit_db - peak) / 20.0)
        loudness = integrated_loudness_lufs(limited, sample_rate)
        suppressed_samples = changed_samples
        suppressed_frames = changed_frames
        output = limited
        if not math.isfinite(loudness):
            break
        error = target_lufs - loudness
        if abs(error) <= 0.01:
            break
        gain *= 10.0 ** (error / 20.0)

    output = np.clip(output, -ceiling, ceiling)
    # Quantization to 24-bit equivalent before the final peak check.
    output = np.rint(output * (2**23)) / float(2**23)
    peak = true_peak_dbtp(output)
    if math.isfinite(peak) and peak > true_peak_limit_dbtp:
        output *= 10.0 ** ((true_peak_limit_dbtp - peak) / 20.0)
        output = np.rint(output * (2**23)) / float(2**23)
    output = np.clip(
        output,
        -(10.0 ** (true_peak_limit_dbtp / 20.0)),
        10.0 ** (true_peak_limit_dbtp / 20.0),
    )
    loudness = integrated_loudness_lufs(output, sample_rate)
    final_peak = true_peak_dbtp(output)
    return MasterResult(
        samples=output,
        integrated_loudness_lufs=loudness,
        true_peak_dbtp=final_peak,
        suppressed_samples=suppressed_samples,
        suppressed_frames=suppressed_frames,
        pre_limit_gain_db=20.0 * math.log10(max(gain, 1e-30)),
        iterations=iterations,
    )
