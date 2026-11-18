from __future__ import annotations

import math
from functools import lru_cache
from typing import Any

import numpy as np

from .util import db_from_power


SILENCE_DB = -120.0


def _biquad_response(
    frequencies: np.ndarray,
    sample_rate: int,
    b: tuple[float, float, float],
    a: tuple[float, float, float],
) -> np.ndarray:
    z1 = np.exp(-2j * np.pi * frequencies / sample_rate)
    z2 = z1 * z1
    numerator = b[0] + b[1] * z1 + b[2] * z2
    denominator = a[0] + a[1] * z1 + a[2] * z2
    return numerator / denominator


def k_weighting_filter(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    """Apply the BS.1770 K-weighting magnitude response using zero-padded FFTs."""
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    frames = data.shape[0]
    if frames == 0:
        return data.copy()

    nfft = 1
    target = max(frames * 2, 1024)
    while nfft < target:
        nfft <<= 1
    frequencies = np.fft.rfftfreq(nfft, d=1.0 / sample_rate)

    # ITU-R BS.1770-4 high-shelf stage (the specified K/Vh/Vb realization).
    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh**0.4996667741545416
    k = math.tan(math.pi * f0 / sample_rate)
    shelf_a_raw = (1.0 + k / q + k * k, 2.0 * (k * k - 1.0), 1.0 - k / q + k * k)
    shelf_b = (
        vh + vb * k / q + k * k,
        2.0 * (k * k - vh),
        vh - vb * k / q + k * k,
    )
    a0 = shelf_a_raw[0]
    shelf_a = tuple(value / a0 for value in shelf_a_raw)
    shelf_b = tuple(value / a0 for value in shelf_b)

    # ITU-R BS.1770-4 RLB high-pass stage.
    f0 = 38.13547087602444
    q = 0.5003270373238773
    w0 = 2.0 * math.pi * f0 / sample_rate
    alpha = math.sin(w0) / (2.0 * q)
    cos_w0 = math.cos(w0)
    highpass_b = ((1.0 + cos_w0) / 2.0, -(1.0 + cos_w0), (1.0 + cos_w0) / 2.0)
    highpass_a_raw = (1.0 + alpha, -2.0 * cos_w0, 1.0 - alpha)
    a0 = highpass_a_raw[0]
    highpass_a = tuple(value / a0 for value in highpass_a_raw)
    highpass_b = tuple(value / a0 for value in highpass_b)

    response = _biquad_response(frequencies, sample_rate, shelf_b, shelf_a)
    response *= _biquad_response(frequencies, sample_rate, highpass_b, highpass_a)

    filtered = np.empty_like(data)
    for channel in range(data.shape[1]):
        spectrum = np.fft.rfft(data[:, channel], n=nfft)
        filtered[:, channel] = np.fft.irfft(spectrum * response, n=nfft)[:frames]
    return filtered


def integrated_loudness(samples: np.ndarray, sample_rate: int) -> float:
    """Measure integrated loudness in LUFS with absolute and relative gating."""
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.size == 0:
        return SILENCE_DB

    weighted = k_weighting_filter(data, sample_rate)
    block = max(1, int(round(0.400 * sample_rate)))
    hop = max(1, int(round(0.100 * sample_rate)))
    if weighted.shape[0] < block:
        block = weighted.shape[0]
    starts = list(range(0, weighted.shape[0] - block + 1, hop))
    if not starts:
        starts = [0]
    energies = np.empty(len(starts), dtype=np.float64)
    for index, start in enumerate(starts):
        segment = weighted[start : start + block]
        energies[index] = np.mean(np.sum(segment * segment, axis=1))
    if energies.size == 0 or np.max(energies) <= 0:
        return SILENCE_DB

    with np.errstate(divide="ignore", invalid="ignore"):
        block_loudness = -0.691 + 10.0 * np.log10(energies)
    absolute = block_loudness > -70.0
    if not np.any(absolute):
        return SILENCE_DB
    absolute_energy = float(np.mean(energies[absolute]))
    relative_gate = -0.691 + 10.0 * math.log10(max(absolute_energy, 1e-24)) - 10.0
    gated = absolute & (block_loudness > relative_gate)
    if not np.any(gated):
        return SILENCE_DB
    return -0.691 + 10.0 * math.log10(max(float(np.mean(energies[gated])), 1e-24))


@lru_cache(maxsize=16)
def _polyphase_sinc(factor: int, taps: int) -> np.ndarray:
    if taps % 2:
        taps += 1
    half = taps // 2
    distance = np.arange(-half + 1, half + 1, dtype=np.float64)
    window = np.hanning(taps)
    phases = np.empty((factor, taps), dtype=np.float64)
    for phase in range(factor):
        offset = phase / factor
        kernel = np.sinc(distance + offset) * window
        denominator = float(np.sum(kernel))
        if abs(denominator) < 1e-15:
            raise ValueError("invalid polyphase interpolation kernel")
        phases[phase] = kernel / denominator
    return phases


def oversampled_peak(samples: np.ndarray, factor: int = 4, taps: int = 32) -> float:
    """Estimate true peak with a windowed-sinc polyphase interpolator."""
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.size == 0:
        return 0.0
    if factor < 1:
        raise ValueError("oversampling factor must be positive")
    if taps % 2:
        taps += 1
    phases = _polyphase_sinc(factor, taps)
    half = taps // 2
    padded = np.pad(data, ((half - 1, half), (0, 0)), mode="constant")
    windows = np.lib.stride_tricks.sliding_window_view(padded, taps, axis=0)
    maximum = float(np.max(np.abs(data)))
    for channel in range(data.shape[1]):
        channel_windows = windows[:, channel, :]
        interpolated = channel_windows @ phases.T
        maximum = max(maximum, float(np.max(np.abs(interpolated))))
    return maximum


def true_peak_dbtp(samples: np.ndarray, factor: int = 4, taps: int = 32) -> float:
    peak = oversampled_peak(samples, factor=factor, taps=taps)
    return SILENCE_DB if peak <= 0 else db_from_power(peak * peak)


def mono_fold_gains(rule: dict[str, Any]) -> tuple[float, float]:
    name = rule["rule"]
    if name == "sum_0.5":
        return 0.5, 0.5
    if name == "equal_power":
        scale = 1.0 / math.sqrt(2.0)
        return scale, scale
    if name == "weighted_sum":
        return float(rule["left_gain"]), float(rule["right_gain"])
    raise ValueError(f"unknown mono fold rule: {name}")


def fold_to_mono(stereo: np.ndarray, rule: dict[str, Any]) -> np.ndarray:
    data = np.asarray(stereo, dtype=np.float64)
    if data.ndim != 2 or data.shape[1] != 2:
        raise ValueError("fold_to_mono expects a stereo (frames, 2) array")
    left_gain, right_gain = mono_fold_gains(rule)
    return (left_gain * data[:, 0] + right_gain * data[:, 1])[:, None]


def band_energy(samples: np.ndarray, sample_rate: int, band_hz: tuple[float, float]) -> float:
    data = np.asarray(samples, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    if data.shape[0] == 0:
        return 0.0
    frequencies = np.fft.rfftfreq(data.shape[0], d=1.0 / sample_rate)
    mask = (frequencies >= band_hz[0]) & (frequencies <= band_hz[1])
    if not np.any(mask):
        return 0.0
    total = 0.0
    for channel in range(data.shape[1]):
        spectrum = np.fft.rfft(data[:, channel])
        power = np.abs(spectrum[mask]) ** 2
        total += float(np.sum(power))
    return total / data.shape[1]


def band_sir_db(
    narration: np.ndarray,
    interference: np.ndarray,
    sample_rate: int,
    band_hz: tuple[float, float],
) -> float:
    signal_energy = band_energy(narration, sample_rate, band_hz)
    interference_energy = band_energy(interference, sample_rate, band_hz)
    return db_from_power(signal_energy / max(interference_energy, 1e-30))


def duck_envelope(
    frames: int,
    intervals: list[dict[str, int]],
    sample_rate: int,
    depth_db: float,
    attack_ms: float,
    recovery_ms: float,
) -> tuple[np.ndarray, list[dict[str, Any]]]:
    gain = np.ones(frames, dtype=np.float64)
    gain_value = 10.0 ** (depth_db / 20.0)
    attack_requested = max(0, int(round(attack_ms * sample_rate / 1000.0)))
    recovery_requested = max(0, int(round(recovery_ms * sample_rate / 1000.0)))
    applied: list[dict[str, Any]] = []

    for interval in intervals:
        start = max(0, min(frames, int(interval["start_sample"])))
        end = max(0, min(frames, int(interval["end_sample"])))
        if end <= start:
            continue
        length = end - start
        attack = min(attack_requested, length // 2)
        recovery = min(recovery_requested, max(0, length - attack))
        if attack + recovery > length:
            recovery = max(0, length - attack)

        local = np.full(length, gain_value, dtype=np.float64)
        if attack > 0:
            phase = np.arange(attack, dtype=np.float64) / max(attack - 1, 1)
            local[:attack] = gain_value + (1.0 - gain_value) * 0.5 * (1.0 + np.cos(math.pi * phase))
        if recovery > 0:
            phase = np.arange(recovery, dtype=np.float64) / max(recovery - 1, 1)
            local[-recovery:] = gain_value + (1.0 - gain_value) * 0.5 * (1.0 - np.cos(math.pi * phase))
        if length == 1:
            local[0] = gain_value
        gain[start:end] = np.minimum(gain[start:end], local)
        applied.append(
            {
                "start_sample": start,
                "end_sample": end,
                "attack_samples": attack,
                "recovery_samples": recovery,
                "gain_db": depth_db,
            }
        )
    return gain, applied


def lookahead_limiter(
    stereo: np.ndarray,
    true_peak_ceiling_dbtp: float,
    sample_rate: int,
) -> tuple[np.ndarray, int, float]:
    """Transparent sample-domain peak safety limiter with no latency and no length change."""
    data = np.asarray(stereo, dtype=np.float64)
    ceiling = 10.0 ** (true_peak_ceiling_dbtp / 20.0)
    safety = 10.0 ** (-0.10 / 20.0)
    sample_threshold = ceiling * safety
    sample_peaks = np.max(np.abs(data), axis=1)
    desired = np.ones_like(sample_peaks)
    active = sample_peaks > sample_threshold
    desired[active] = sample_threshold / np.maximum(sample_peaks[active], 1e-30)

    lookahead = max(1, int(round(0.005 * sample_rate)))
    release = max(1, int(round(0.060 * sample_rate)))
    if np.any(active):
        padded = np.pad(desired, (0, lookahead), mode="edge")
        windows = np.lib.stride_tricks.sliding_window_view(padded, lookahead + 1)
        forward_min = np.min(windows, axis=1)
        smooth = np.empty_like(forward_min)
        current = 1.0
        release_coefficient = math.exp(-1.0 / release)
        for index, target in enumerate(forward_min):
            if target < current:
                current = target
            else:
                current = max(target, target + (current - target) * release_coefficient)
            smooth[index] = current
        peak = float(np.max(np.abs(data * smooth[:, None])))
        if peak > ceiling:
            smooth *= ceiling / peak
        output = data * smooth[:, None]
        triggered = int(np.count_nonzero(smooth < (1.0 - 1e-12)))
    else:
        output = data.copy()
        triggered = 0
    measured = true_peak_dbtp(output)
    if measured > true_peak_ceiling_dbtp:
        trim = 10.0 ** ((true_peak_ceiling_dbtp - measured) / 20.0)
        output *= trim
        measured = true_peak_dbtp(output)
    return output, triggered, measured


def normalize_loudness_and_peak(
    mix: np.ndarray,
    sample_rate: int,
    target_lufs: float,
    true_peak_ceiling_dbtp: float,
    tolerance_lu: float,
) -> tuple[np.ndarray, dict[str, Any]]:
    data = np.asarray(mix, dtype=np.float64)
    if data.size == 0:
        raise ValueError("cannot normalize empty audio")
    measured = integrated_loudness(data, sample_rate)
    if measured <= SILENCE_DB + 1:
        raise ValueError("mix is silent")
    gain_db = target_lufs - measured
    working = data * (10.0 ** (gain_db / 20.0))
    total_triggered = 0
    before_loudness = integrated_loudness(working, sample_rate)
    before_peak = true_peak_dbtp(working)
    output = working

    for _ in range(6):
        output, triggered, _ = lookahead_limiter(output, true_peak_ceiling_dbtp, sample_rate)
        total_triggered = triggered
        after_loudness = integrated_loudness(output, sample_rate)
        difference = target_lufs - after_loudness
        if abs(difference) <= 0.05:
            break
        if difference > 0 and true_peak_dbtp(output) >= true_peak_ceiling_dbtp - 0.02:
            break
        adjustment = 10.0 ** (difference / 20.0)
        before_loudness = integrated_loudness(output * adjustment, sample_rate)
        before_peak = true_peak_dbtp(output * adjustment)
        output = output * adjustment

    final_loudness = integrated_loudness(output, sample_rate)
    final_peak = true_peak_dbtp(output)
    if abs(final_loudness - target_lufs) > tolerance_lu:
        raise ValueError(
            f"cannot meet loudness target under peak ceiling: {final_loudness:.3f} LUFS"
        )
    if final_peak > true_peak_ceiling_dbtp + 1e-6:
        raise ValueError(f"true peak ceiling exceeded: {final_peak:.3f} dBTP")
    return output, {
        "target_lufs": round(target_lufs, 6),
        "measured_lufs": round(final_loudness, 6),
        "deviation_lu": round(final_loudness - target_lufs, 6),
        "true_peak_dbtp": round(final_peak, 6),
        "limit_triggered_samples": int(total_triggered),
        "before_limiter_lufs": round(before_loudness, 6),
        "before_limiter_true_peak_dbtp": round(before_peak, 6),
        "after_limiter_lufs": round(final_loudness, 6),
        "after_limiter_true_peak_dbtp": round(final_peak, 6),
    }
