from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np


EPS = 1.0e-24


def _biquad_impulse(b: tuple[float, float, float], a: tuple[float, float], length: int) -> np.ndarray:
    h = np.zeros(length, dtype=np.float64)
    for index in range(length):
        value = b[0] if index == 0 else 0.0
        if index == 1:
            value += b[1]
        elif index == 2:
            value += b[2]
        if index >= 1:
            value -= a[0] * h[index - 1]
        if index >= 2:
            value -= a[1] * h[index - 2]
        h[index] = value
    return h


def _k_weighting_impulse(sample_rate: int, length: int = 16_384) -> np.ndarray:
    if sample_rate != 48_000:
        raise ValueError("BS.1770 K-weighting coefficients are specified here for 48 kHz")
    shelf = _biquad_impulse(
        (1.53512485958697, -2.69169618940638, 1.19839281085285),
        (-1.69065929318241, 0.73248077421585),
        length,
    )
    highpass = _biquad_impulse(
        (1.0, -2.0, 1.0),
        (-1.99004745483398, 0.99007225036621),
        length,
    )
    nfft = 1 << (2 * length - 1).bit_length()
    combined = np.fft.irfft(
        np.fft.rfft(shelf, n=nfft) * np.fft.rfft(highpass, n=nfft), n=nfft
    )[:length]
    return combined


def _fir_same(x: np.ndarray, impulse: np.ndarray, block_frames: int = 262_144) -> np.ndarray:
    x = np.asarray(x, dtype=np.float32)
    impulse = np.asarray(impulse, dtype=np.float64)
    length = int(x.shape[0])
    taps = int(impulse.shape[0])
    output = np.empty(length, dtype=np.float32)
    if taps == 0:
        return x.copy()
    carry = np.zeros(taps - 1, dtype=np.float64)
    spectrum = None
    nfft = 0
    for start in range(0, length, block_frames):
        end = min(length, start + block_frames)
        chunk = x[start:end].astype(np.float64)
        needed = chunk.shape[0] + taps - 1
        current_nfft = 1 << (needed - 1).bit_length()
        if spectrum is None or current_nfft != nfft:
            nfft = current_nfft
            spectrum = np.fft.rfft(impulse, n=nfft)
        convolved = np.fft.irfft(np.fft.rfft(chunk, n=nfft) * spectrum, n=nfft)[:needed]
        overlap = min(carry.size, convolved.size)
        convolved[:overlap] += carry[:overlap]
        take = min(chunk.shape[0], convolved.size)
        output[start : start + take] = convolved[:take]
        carry = convolved[take:].copy()
    return output


def integrated_loudness_lufs(stereo: np.ndarray, sample_rate: int = 48_000) -> float:
    """ITU-R BS.1770 integrated loudness for a 48 kHz stereo signal."""
    if stereo.ndim != 2 or stereo.shape[1] != 2:
        raise ValueError("stereo input must have shape (frames, 2)")
    impulse = _k_weighting_impulse(sample_rate)
    left = _fir_same(stereo[:, 0], impulse)
    right = _fir_same(stereo[:, 1], impulse)
    frames = int(stereo.shape[0])
    block = int(round(0.400 * sample_rate))
    step = int(round(0.100 * sample_rate))
    if frames < block:
        block = frames
        step = max(1, frames)
    starts = np.arange(0, frames - block + 1, step, dtype=np.int64)
    if starts.size == 0:
        starts = np.array([0], dtype=np.int64)
    left_sq = left.astype(np.float64) ** 2
    right_sq = right.astype(np.float64) ** 2
    left_cumsum = np.concatenate(([0.0], np.cumsum(left_sq)))
    right_cumsum = np.concatenate(([0.0], np.cumsum(right_sq)))
    left_energy = (left_cumsum[starts + block] - left_cumsum[starts]) / block
    right_energy = (right_cumsum[starts + block] - right_cumsum[starts]) / block
    z = left_energy + right_energy
    with np.errstate(divide="ignore", invalid="ignore"):
        loudness = -0.691 + 10.0 * np.log10(z + EPS)
    absolute = loudness > -70.0
    if not np.any(absolute):
        return -math.inf
    ungated = float(np.mean(z[absolute]))
    relative_threshold = -0.691 + 10.0 * math.log10(max(ungated, EPS)) - 10.0
    gated = absolute & (loudness > relative_threshold)
    if not np.any(gated):
        return -math.inf
    integrated = -0.691 + 10.0 * math.log10(max(float(np.mean(z[gated])), EPS))
    return float(integrated)


def _interpolation_spectrum() -> tuple[np.ndarray, int, int]:
    oversample = 4
    half = 16
    length = half * oversample * 2 + 1
    center = (length - 1) // 2
    n = np.arange(length, dtype=np.float64) - center
    impulse = np.sinc(n / oversample) * np.hamming(length)
    impulse /= impulse[center]
    return impulse, center, oversample


def true_peak_dbtp(stereo: np.ndarray, sample_rate: int = 48_000) -> float:
    """4x windowed-sinc oversampled peak following the BS.1770 Annex 2 intent."""
    if stereo.ndim != 2 or stereo.shape[1] != 2:
        raise ValueError("stereo input must have shape (frames, 2)")
    impulse, center, oversample = _interpolation_spectrum()
    taps = impulse.shape[0]
    pad = taps + 1
    block_frames = 65_536
    peak = 0.0
    for channel in range(2):
        signal = np.asarray(stereo[:, channel], dtype=np.float32)
        frames = int(signal.shape[0])
        for start in range(0, frames, block_frames):
            end = min(frames, start + block_frames)
            padded_start = max(0, start - pad)
            padded_end = min(frames, end + pad)
            segment = signal[padded_start:padded_end].astype(np.float64)
            if segment.size == 0:
                continue
            zero_stuffed = np.zeros((segment.size - 1) * oversample + 1, dtype=np.float64)
            zero_stuffed[::oversample] = segment
            needed = zero_stuffed.size + taps - 1
            nfft = 1 << (needed - 1).bit_length()
            interpolated = np.fft.irfft(
                np.fft.rfft(zero_stuffed, n=nfft) * np.fft.rfft(impulse, n=nfft),
                n=nfft,
            )[:needed]
            core_start = (start - padded_start) * oversample
            core_end = core_start + (end - start) * oversample
            low = max(0, core_start - center)
            high = min(interpolated.size, core_end + center)
            if high > low:
                peak = max(peak, float(np.max(np.abs(interpolated[low:high]))))
    if peak <= 0.0:
        return -math.inf
    return 20.0 * math.log10(peak)


@dataclass(frozen=True)
class LimiterStats:
    triggered_samples: int
    pre_true_peak_dbtp: float
    post_true_peak_dbtp: float
    delay_compensation_samples: int
    gain_reduction_db: float


def apply_peak_limiter(
    stereo: np.ndarray,
    true_peak_limit_dbtp: float,
    sample_rate: int = 48_000,
) -> tuple[np.ndarray, LimiterStats]:
    """Deterministic stereo-linked offline limiter with zero net latency.

    The gain envelope has a short look-ahead, a linear attack/release guard and no
    time shift.  A final conservative trim guarantees the declared true-peak limit.
    """
    pre_tp = true_peak_dbtp(stereo, sample_rate)
    ceiling = 10.0 ** (true_peak_limit_dbtp / 20.0)
    if pre_tp <= true_peak_limit_dbtp:
        return stereo.astype(np.float32, copy=True), LimiterStats(
            triggered_samples=0,
            pre_true_peak_dbtp=pre_tp,
            post_true_peak_dbtp=pre_tp,
            delay_compensation_samples=0,
            gain_reduction_db=0.0,
        )

    frames = int(stereo.shape[0])
    sample_peak = np.max(np.abs(stereo), axis=1).astype(np.float64)
    target_gain = np.minimum(1.0, ceiling / (sample_peak + EPS))

    lookahead = max(1, min(frames - 1, int(round(0.006 * sample_rate))))
    forward = target_gain.copy()
    shift = 1
    while shift < lookahead:
        forward[:-shift] = np.minimum(forward[:-shift], forward[shift:])
        shift *= 2
    if shift > lookahead:
        partial = shift - lookahead
        if partial < frames:
            forward[:-partial] = np.minimum(forward[:-partial], forward[partial:])

    guard = max(3, int(round(0.004 * sample_rate)) | 1)
    cumulative = np.concatenate(([0.0], np.cumsum(forward)))
    half = guard // 2
    indices = np.arange(frames, dtype=np.int64)
    low = np.maximum(0, indices - half)
    high = np.minimum(frames, indices + half + 1)
    smooth = (cumulative[high] - cumulative[low]) / (high - low)
    # Smoothing can raise the envelope above a local target; a conservative
    # pre-attenuation prevents inter-sample overs before the final verification.
    gain = np.minimum(forward, smooth * 0.88)
    limited = stereo.astype(np.float32) * gain[:, None].astype(np.float32)
    post_tp = true_peak_dbtp(limited, sample_rate)
    if post_tp > true_peak_limit_dbtp:
        trim = 10.0 ** ((true_peak_limit_dbtp - post_tp) / 20.0)
        limited *= np.float32(trim)
        gain *= trim
        post_tp = true_peak_dbtp(limited, sample_rate)
    reduction = -20.0 * math.log10(max(float(np.min(gain)), EPS))
    triggered_samples = int(np.count_nonzero(gain < 1.0 - 1.0e-9))
    return limited, LimiterStats(
        triggered_samples=triggered_samples,
        pre_true_peak_dbtp=pre_tp,
        post_true_peak_dbtp=post_tp,
        delay_compensation_samples=0,
        gain_reduction_db=max(0.0, reduction),
    )


def seal_loop_endpoints(stereo: np.ndarray, length: int) -> np.ndarray:
    """Match the two loop endpoints with a short, in-overlap cosine ramp."""
    if length <= 0 or stereo.shape[0] < 2 * length:
        return stereo.astype(np.float32, copy=True)
    output = stereo.astype(np.float32, copy=True)
    t = np.arange(length, dtype=np.float64) / max(1, length - 1)
    weight = 0.5 * (1.0 - np.cos(np.pi * t))
    for channel in range(output.shape[1]):
        difference = float(output[0, channel]) - float(output[-1, channel])
        output[-length:, channel] += (0.5 * difference * weight).astype(np.float32)
        output[:length, channel] -= (0.5 * difference * (1.0 - weight)).astype(np.float32)
    return output


def seam_metrics(
    stereo: np.ndarray,
    sample_rate: int,
    window_seconds: float,
    pre_crossfade_delta: float,
    mid_step_seconds: float = 1.0,
) -> dict[str, Any]:
    length = max(1, int(round(window_seconds * sample_rate)))
    length = min(length, stereo.shape[0] // 2)
    tail_energy = float(np.mean(stereo[-length:].astype(np.float64) ** 2))
    head_energy = float(np.mean(stereo[:length].astype(np.float64) ** 2))
    energy_jump = abs(head_energy - tail_energy)
    step = max(length, int(round(mid_step_seconds * sample_rate)))
    energies: list[float] = []
    for start in range(length, stereo.shape[0] - length, step):
        segment = stereo[start : start + length].astype(np.float64)
        energies.append(float(np.mean(segment**2)))
    if len(energies) >= 2:
        mid_energy_jump = max(abs(b - a) for a, b in zip(energies, energies[1:]))
    else:
        mid_energy_jump = 0.0
    seam_delta = float(np.max(np.abs(stereo[0].astype(np.float64) - stereo[-1].astype(np.float64))))
    energy_limit = 2.0 * mid_energy_jump
    energy_jump_ok = energy_jump <= max(energy_limit, 1e-12)
    return {
        "crossfade_pre_seam_sample_delta": float(pre_crossfade_delta),
        "seam_sample_delta": seam_delta,
        "seam_tail_energy": tail_energy,
        "seam_head_energy": head_energy,
        "seam_energy_jump": energy_jump,
        "mid_max_energy_jump": float(mid_energy_jump),
        "allowed_energy_jump": float(energy_limit),
        "seam_energy_jump_ok": bool(energy_jump_ok),
    }


def _welch_power(signal: np.ndarray, sample_rate: int, block: int = 16_384) -> tuple[np.ndarray, np.ndarray]:
    values = np.asarray(signal, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    frames = values.shape[0]
    block = min(block, frames)
    hop = max(1, block // 2)
    window = np.hanning(block)
    window_energy = float(np.sum(window**2))
    frequencies = np.fft.rfftfreq(block, d=1.0 / sample_rate)
    power = np.zeros(frequencies.shape[0], dtype=np.float64)
    count = 0
    for start in range(0, frames - block + 1, hop):
        segment = values[start : start + block]
        for channel in range(values.shape[1]):
            spectrum = np.fft.rfft(segment[:, channel] * window)
            power += np.abs(spectrum) ** 2
        count += values.shape[1]
    if count == 0:
        segment = np.zeros((block, values.shape[1]), dtype=np.float64)
        for channel in range(values.shape[1]):
            spectrum = np.fft.rfft(segment[:, channel] * window)
            power += np.abs(spectrum) ** 2
        count = 1
    power /= count * max(window_energy, 1.0)
    return frequencies, power


def midband_energy_ratio(stereo: np.ndarray, mono: np.ndarray, sample_rate: int, band_hz: tuple[float, float]) -> dict[str, Any]:
    frequencies, stereo_power = _welch_power(stereo, sample_rate)
    _, mono_power = _welch_power(mono, sample_rate)
    mask = (frequencies >= band_hz[0]) & (frequencies <= band_hz[1])
    stereo_total = float(np.sum(stereo_power[1:]))
    stereo_mid = float(np.sum(stereo_power[mask]))
    mono_total = float(np.sum(mono_power[1:]))
    mono_mid = float(np.sum(mono_power[mask]))
    stereo_ratio = stereo_mid / max(stereo_total, EPS)
    mono_ratio = mono_mid / max(mono_total, EPS)
    deviation_db = abs(10.0 * math.log10(max(stereo_ratio, EPS) / max(mono_ratio, EPS)))
    return {
        "stereo_midband_energy_ratio": stereo_ratio,
        "mono_midband_energy_ratio": mono_ratio,
        "deviation_db": float(deviation_db),
    }


def scene_scene_midband_ratio(
    scene_stereo: np.ndarray,
    scene_mono: np.ndarray,
    sample_rate: int,
    band_hz: tuple[float, float],
) -> dict[str, Any]:
    return midband_energy_ratio(scene_stereo, scene_mono, sample_rate, band_hz)


def dbfs_to_linear(value: float) -> float:
    return 10.0 ** (value / 20.0)

