from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np
from scipy import ndimage, signal


def db(value: float) -> float:
    return 20.0 * math.log10(max(float(value), 1e-15))


def energy_db(energy: float) -> float:
    return 10.0 * math.log10(max(float(energy), 1e-30))


def shift_audio(data: np.ndarray, samples: int) -> np.ndarray:
    if samples == 0:
        return np.asarray(data, dtype=np.float64).copy()
    source = np.asarray(data, dtype=np.float64)
    if samples > 0:
        return np.concatenate((np.zeros(samples, dtype=np.float64), source))
    if -samples >= len(source):
        return np.zeros(0, dtype=np.float64)
    return source[-samples:].copy()


def detect_onset(data: np.ndarray, sample_rate: int) -> int | None:
    """Return the first clearly active sample of an isolated mono stem.

    The detector combines a fast absolute-amplitude gate with a 1 ms energy
    envelope.  It is deliberately independent per channel: one failure never
    changes any other channel's sample offset.
    """

    values = np.asarray(data, dtype=np.float64)
    if values.size == 0 or not np.all(np.isfinite(values)):
        return None
    absolute = np.abs(values)
    peak = float(np.max(absolute))
    if peak <= 1e-10:
        return None
    window = max(2, int(round(0.001 * sample_rate)))
    envelope = ndimage.uniform_filter1d(values * values, size=window, mode="nearest")
    envelope = np.sqrt(np.maximum(envelope, 0.0))
    noise_window = min(len(values), max(window, int(round(0.030 * sample_rate))))
    noise_floor = float(np.median(envelope[:noise_window]))
    amplitude_threshold = max(peak * 0.0015, noise_floor * 8.0, 1e-9)
    envelope_threshold = max(peak * 0.001, noise_floor * 4.0, 1e-11)
    candidates = np.flatnonzero((absolute >= amplitude_threshold) & (envelope >= envelope_threshold))
    if candidates.size == 0:
        return None
    first = int(candidates[0])
    # Backtrack over the 1 ms window so the reported position is the leading
    # edge of the transient rather than the first envelope crossing.
    lower = max(0, first - window)
    preceding = np.flatnonzero(absolute[lower : first + 1] >= amplitude_threshold)
    if preceding.size:
        first = lower + int(preceding[0])
    return first


def bandpass_fft(data: np.ndarray, sample_rate: int, low: float, high: float) -> np.ndarray:
    source = np.asarray(data, dtype=np.float64)
    if source.size == 0:
        return source.copy()
    frequencies = np.fft.rfftfreq(len(source), d=1.0 / sample_rate)
    spectrum = np.fft.rfft(source)
    nyquist = sample_rate / 2.0
    low = max(0.0, min(float(low), nyquist * 0.999))
    high = max(low + 1.0, min(float(high), nyquist * 0.999))
    transition = max(1.0, (high - low) * 0.04)
    mask = np.zeros_like(frequencies)
    mask[(frequencies >= low) & (frequencies <= high)] = 1.0
    left = (frequencies >= low - transition) & (frequencies < low)
    right = (frequencies > high) & (frequencies <= high + transition)
    mask[left] = 0.5 - 0.5 * np.cos(
        np.pi * (frequencies[left] - (low - transition)) / transition
    )
    mask[right] = 0.5 + 0.5 * np.cos(
        np.pi * (frequencies[right] - high) / transition
    )
    return np.fft.irfft(spectrum * mask, n=len(source))


def window_gate(length: int, sample_rate: int, windows_seconds: list[tuple[float, float]], fade_seconds: float = 0.006) -> np.ndarray:
    gate = np.zeros(length, dtype=np.float64)
    fade = max(2, int(round(fade_seconds * sample_rate)))
    for start_seconds, end_seconds in windows_seconds:
        start = max(0, int(round(start_seconds * sample_rate)))
        end = min(length, int(round(end_seconds * sample_rate)))
        if end <= start:
            continue
        segment = np.ones(end - start, dtype=np.float64)
        ramp = min(fade, len(segment) // 2)
        if ramp > 1:
            curve = 0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, ramp))
            segment[:ramp] *= curve
            segment[-ramp:] *= curve[::-1]
        gate[start:end] = np.maximum(gate[start:end], segment)
    return gate


def merge_intervals(mask: np.ndarray) -> list[tuple[int, int]]:
    active = np.flatnonzero(mask)
    if active.size == 0:
        return []
    breaks = np.flatnonzero(np.diff(active) > 1) + 1
    starts = np.concatenate(([active[0]], active[breaks]))
    ends = np.concatenate((active[breaks - 1], [active[-1]]))
    return [(int(start), int(end) + 1) for start, end in zip(starts, ends)]


def k_weighting_coefficients(sample_rate: int) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    # ITU-R BS.1770-4 analogue prototype translated with a bilinear transform.
    rate = float(sample_rate)
    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = math.tan(math.pi * f0 / rate)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / q + k * k
    shelf_b = np.array(
        [
            (vh + vb * k / q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / q + k * k) / a0,
        ],
        dtype=np.float64,
    )
    shelf_a = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0],
        dtype=np.float64,
    )
    f0 = 38.13547087602444
    q = 0.5003270373238773
    k = math.tan(math.pi * f0 / rate)
    a0 = 1.0 + k / q + k * k
    highpass_b = np.array([1.0, -2.0, 1.0], dtype=np.float64)
    highpass_a = np.array(
        [1.0, 2.0 * (k * k - 1.0) / a0, (1.0 - k / q + k * k) / a0],
        dtype=np.float64,
    )
    return shelf_b, shelf_a, highpass_b, highpass_a


def _loudness_blocks(stereo: np.ndarray, sample_rate: int) -> np.ndarray:
    x = np.asarray(stereo, dtype=np.float64)
    if x.ndim != 2 or x.shape[1] != 2:
        raise ValueError("响度计算要求 shape=(frames, 2)")
    shelf_b, shelf_a, highpass_b, highpass_a = k_weighting_coefficients(sample_rate)
    filtered = signal.lfilter(shelf_b, shelf_a, x, axis=0)
    filtered = signal.lfilter(highpass_b, highpass_a, filtered, axis=0)
    block = max(1, int(round(0.400 * sample_rate)))
    step = max(1, int(round(0.100 * sample_rate)))
    if len(filtered) < block:
        filtered = np.pad(filtered, ((0, block - len(filtered)), (0, 0)))
    starts = range(0, len(filtered) - block + 1, step)
    powers = np.array(
        [float(np.mean(np.sum(filtered[start : start + block] ** 2, axis=1))) for start in starts],
        dtype=np.float64,
    )
    return powers


def integrated_loudness(stereo: np.ndarray, sample_rate: int) -> float:
    powers = _loudness_blocks(stereo, sample_rate)
    if powers.size == 0 or np.all(powers <= 1e-30):
        return -120.0
    absolute = -0.691 + 10.0 * np.log10(np.maximum(powers, 1e-30))
    absolute_gated = powers[absolute > -70.0]
    if absolute_gated.size == 0:
        return -120.0
    absolute_mean = float(np.mean(absolute_gated))
    relative_gate = -0.691 + 10.0 * math.log10(max(absolute_mean, 1e-30)) - 10.0
    final = powers[(absolute > -70.0) & (absolute > relative_gate)]
    if final.size == 0:
        return -120.0
    return -0.691 + 10.0 * math.log10(max(float(np.mean(final)), 1e-30))


def true_peak_linear(stereo: np.ndarray, oversample: int = 4) -> float:
    x = np.asarray(stereo, dtype=np.float64)
    if x.size == 0:
        return 0.0
    peaks = []
    for channel in range(x.shape[1]):
        upsampled = signal.resample_poly(x[:, channel], oversample, 1, padtype="constant")
        peaks.append(float(np.max(np.abs(upsampled))))
    return max(peaks, default=0.0)


def true_peak_dbtp(stereo: np.ndarray, oversample: int = 4) -> float:
    peak = true_peak_linear(stereo, oversample)
    return db(peak) if peak > 0 else -120.0


@dataclass
class LimiterResult:
    audio: np.ndarray
    gain: np.ndarray
    triggered: bool
    triggered_frames: int
    latency_samples: int


def _linked_limit_once(
    stereo: np.ndarray,
    sample_rate: int,
    ceiling_linear: float,
    lookahead_ms: float = 3.0,
    release_ms: float = 70.0,
) -> LimiterResult:
    x = np.asarray(stereo, dtype=np.float64)
    factor = 4
    up = np.stack(
        [
            signal.resample_poly(x[:, channel], factor, 1, padtype="constant")
            for channel in range(2)
        ],
        axis=1,
    )
    envelope = np.max(np.abs(up), axis=1)
    desired = np.minimum(1.0, ceiling_linear / np.maximum(envelope, 1e-15))
    lookahead = max(1, int(round(lookahead_ms * 0.001 * sample_rate * factor)))
    gain_up = ndimage.minimum_filter1d(desired, size=2 * lookahead + 1, mode="nearest")
    release = max(1.0, release_ms * 0.001 * sample_rate * factor)
    release_alpha = 1.0 - math.exp(-1.0 / release)
    initial_state = np.array([(1.0 - release_alpha) * gain_up[0]], dtype=np.float64)
    softened, _ = signal.lfilter(
        [release_alpha],
        [1.0, -(1.0 - release_alpha)],
        gain_up,
        zi=initial_state,
    )
    gain_up = np.minimum(gain_up, softened)
    usable = len(gain_up) // factor
    gain = gain_up[: usable * factor].reshape(usable, factor).min(axis=1)
    if len(gain) < len(x):
        gain = np.pad(gain, (0, len(x) - len(gain)), mode="edge")
    gain = gain[: len(x)]
    gain = np.minimum(gain, 1.0)
    output = x * gain[:, None]
    triggered_frames = int(np.count_nonzero(gain < 0.9999999))
    return LimiterResult(
        audio=output,
        gain=gain,
        triggered=triggered_frames > 0,
        triggered_frames=triggered_frames,
        latency_samples=0,
    )


def linked_true_peak_limiter(
    stereo: np.ndarray,
    sample_rate: int,
    ceiling_dbtp: float,
    *,
    safety_db: float = 0.08,
) -> LimiterResult:
    x = np.asarray(stereo, dtype=np.float64)
    target_linear = 10.0 ** (ceiling_dbtp / 20.0)
    if true_peak_linear(x) <= target_linear:
        return LimiterResult(
            audio=x.copy(),
            gain=np.ones(len(x), dtype=np.float64),
            triggered=False,
            triggered_frames=0,
            latency_samples=0,
        )
    working_ceiling = target_linear * 10.0 ** (-safety_db / 20.0)
    result = _linked_limit_once(x, sample_rate, working_ceiling)
    # A linked time-varying gain can leave residual inter-sample peaks after
    # decimation.  A small global safety trim is mathematically incapable of
    # creating new peaks and avoids rebuilding the 4x envelope repeatedly.
    for _ in range(4):
        measured = true_peak_linear(result.audio)
        if measured <= target_linear * (1.0 + 1e-9):
            break
        if measured <= 0:
            break
        correction = (target_linear * 10.0 ** (-0.02 / 20.0)) / measured
        correction = min(1.0, correction)
        result.audio *= correction
        result.gain *= correction
    result.triggered_frames = int(np.count_nonzero(result.gain < 0.9999999))
    result.triggered = result.triggered_frames > 0
    return result


@dataclass
class MasterResult:
    audio: np.ndarray
    pre_gain: float
    limiter_gain: np.ndarray
    limiter: LimiterResult
    loudness_before_limiter: float
    loudness_after_limiter: float
    true_peak_before_limiter: float
    true_peak_after_limiter: float
    target_loudness_lufs: float
    target_true_peak_dbtp: float
    iterations: int


def apply_loudness_and_peak_limit(
    raw_stereo: np.ndarray,
    sample_rate: int,
    target_loudness_lufs: float,
    true_peak_limit_dbtp: float,
    *,
    tolerance_lu: float = 0.04,
    max_iterations: int = 14,
) -> MasterResult:
    x = np.asarray(raw_stereo, dtype=np.float64)
    initial_loudness = integrated_loudness(x, sample_rate)
    if initial_loudness <= -119.0:
        raise ValueError("母带信号为静音，无法计算响度")
    pre_gain = 10.0 ** ((target_loudness_lufs - initial_loudness) / 20.0)
    peak_ceiling = 10.0 ** ((true_peak_limit_dbtp - 0.02) / 20.0)
    last: MasterResult | None = None
    for iteration in range(1, max_iterations + 1):
        gained = x * pre_gain
        loudness_before = integrated_loudness(gained, sample_rate)
        tp_before = true_peak_dbtp(gained)
        limited = linked_true_peak_limiter(
            gained,
            sample_rate,
            true_peak_limit_dbtp - 0.02,
            safety_db=0.06,
        )
        loudness_after = integrated_loudness(limited.audio, sample_rate)
        tp_after = true_peak_dbtp(limited.audio)
        last = MasterResult(
            audio=limited.audio,
            pre_gain=pre_gain,
            limiter_gain=limited.gain,
            limiter=limited,
            loudness_before_limiter=loudness_before,
            loudness_after_limiter=loudness_after,
            true_peak_before_limiter=tp_before,
            true_peak_after_limiter=tp_after,
            target_loudness_lufs=target_loudness_lufs,
            target_true_peak_dbtp=true_peak_limit_dbtp,
            iterations=iteration,
        )
        error = target_loudness_lufs - loudness_after
        if abs(error) <= tolerance_lu and tp_after <= true_peak_limit_dbtp + 1e-6:
            return last
        if not limited.triggered and tp_before <= peak_ceiling + 1e-12 and abs(error) <= 0.25:
            # Static gain already reached the target; no limiter was required.
            return last
        # A linked limiter reduces loudness less than it reduces peak.  Damping
        # the correction avoids oscillation while retaining sub-0.5 LU accuracy.
        correction = 10.0 ** ((0.82 * error) / 20.0)
        if not math.isfinite(correction) or correction <= 0:
            break
        pre_gain *= correction
    if last is None:
        raise RuntimeError("限幅迭代未产生结果")
    return last


def pan_weights(anchor_x: float) -> tuple[float, float]:
    position = max(-1.0, min(1.0, float(anchor_x)))
    angle = (position + 1.0) * math.pi / 4.0
    return math.cos(angle), math.sin(angle)


def stable_float(value: Any, digits: int = 9) -> float:
    result = round(float(value), digits)
    return 0.0 if result == 0 else result
