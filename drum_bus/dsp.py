from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np
from numpy.lib.stride_tricks import sliding_window_view

from .errors import RenderError
from .wavio import quantize_pcm


@dataclass(frozen=True)
class LimiterStats:
    gain: np.ndarray
    triggered_frames: int
    triggered_channel_samples: int
    lookahead_samples: int
    minimum_gain_db: float


@dataclass(frozen=True)
class MasterResult:
    samples: np.ndarray
    pcm: np.ndarray
    integrated_lufs: float
    true_peak_dbtp: float
    target_lufs: float
    deviation_lu: float
    true_peak_ceiling_dbtp: float
    limiter: LimiterStats
    safety_scale: float
    attempts: int
    compliant: bool

    def as_dict(self) -> dict[str, Any]:
        return {
            "target_lufs": self.target_lufs,
            "measured_lufs": self.integrated_lufs,
            "deviation_lu": self.deviation_lu,
            "true_peak_dbtp": self.true_peak_dbtp,
            "true_peak_ceiling_dbtp": self.true_peak_ceiling_dbtp,
            "limiter_triggered_samples": self.limiter.triggered_frames,
            "limiter_triggered_channel_samples": (
                self.limiter.triggered_channel_samples
            ),
            "limiter_lookahead_samples": self.limiter.lookahead_samples,
            "limiter_minimum_gain_db": self.limiter.minimum_gain_db,
            "limiter_latency_compensation_samples": self.limiter.lookahead_samples,
            "limiter_safety_scale": self.safety_scale,
            "loudness_attempts": self.attempts,
            "compliant": self.compliant,
        }


def _biquad_coefficients(sample_rate: int) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    if sample_rate == 48000:
        shelf_b = np.array([1.53512485958697, -2.69169618940638, 1.19839281085285])
        shelf_a = np.array([1.0, -1.69065929318241, 0.73248077421585])
        pass_b = np.array([1.0, -2.0, 1.0])
        pass_a = np.array([1.0, -1.99004745483398, 0.99007225036621])
        return shelf_b, shelf_a, pass_b, pass_a

    gain_db = 4.0
    shelf_frequency = 1681.974450955533
    shelf_q = 0.7071752369554196
    pass_frequency = 38.13547087602444
    pass_q = 0.5003270373238773
    amplitude = 10.0 ** (gain_db / 40.0)
    omega = 2.0 * np.pi * shelf_frequency / sample_rate
    cosine = np.cos(omega)
    sine = np.sin(omega)
    alpha = sine / (2.0 * shelf_q)
    sqrt_a = np.sqrt(amplitude)
    shelf_b = np.array(
        [
            amplitude
            * ((amplitude + 1.0) + (amplitude - 1.0) * cosine + 2.0 * sqrt_a * alpha),
            -2.0
            * amplitude
            * ((amplitude - 1.0) + (amplitude + 1.0) * cosine),
            amplitude
            * ((amplitude + 1.0) + (amplitude - 1.0) * cosine - 2.0 * sqrt_a * alpha),
        ]
    )
    shelf_a = np.array(
        [
            (amplitude + 1.0) - (amplitude - 1.0) * cosine + 2.0 * sqrt_a * alpha,
            2.0 * ((amplitude - 1.0) - (amplitude + 1.0) * cosine),
            (amplitude + 1.0) - (amplitude - 1.0) * cosine - 2.0 * sqrt_a * alpha,
        ]
    )
    shelf_b = shelf_b / shelf_a[0]
    shelf_a = shelf_a / shelf_a[0]

    omega = 2.0 * np.pi * pass_frequency / sample_rate
    cosine = np.cos(omega)
    sine = np.sin(omega)
    alpha = sine / (2.0 * pass_q)
    pass_b = np.array([(1.0 + cosine) / 2.0, -(1.0 + cosine), (1.0 + cosine) / 2.0])
    pass_a = np.array([1.0 + alpha, -2.0 * cosine, 1.0 - alpha])
    pass_b = pass_b / pass_a[0]
    pass_a = pass_a / pass_a[0]
    return shelf_b, shelf_a, pass_b, pass_a


def _apply_biquad(signal: np.ndarray, b: np.ndarray, a: np.ndarray) -> np.ndarray:
    source = np.asarray(signal, dtype=np.float64)
    output = np.empty_like(source)
    b0, b1, b2 = (float(value) for value in b)
    a1, a2 = float(a[1]), float(a[2])
    x1 = x2 = y1 = y2 = 0.0
    for index in range(source.size):
        x0 = float(source[index])
        y0 = b0 * x0 + b1 * x1 + b2 * x2 - a1 * y1 - a2 * y2
        output[index] = y0
        x2, x1 = x1, x0
        y2, y1 = y1, y0
    return output


def k_weight(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    shelf_b, shelf_a, pass_b, pass_a = _biquad_coefficients(sample_rate)
    output = np.empty_like(values)
    for channel_index in range(values.shape[1]):
        channel = _apply_biquad(values[:, channel_index], shelf_b, shelf_a)
        output[:, channel_index] = _apply_biquad(channel, pass_b, pass_a)
    return output


def integrated_loudness(samples: np.ndarray, sample_rate: int) -> float:
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    filtered = k_weight(values, sample_rate)
    block_frames = max(1, int(round(0.400 * sample_rate)))
    hop_frames = max(1, int(round(0.100 * sample_rate)))
    if filtered.shape[0] < block_frames:
        padded = np.zeros((block_frames, filtered.shape[1]), dtype=np.float64)
        padded[: filtered.shape[0]] = filtered
        filtered = padded
    count = 1 + (filtered.shape[0] - block_frames) // hop_frames
    powers = np.empty(count, dtype=np.float64)
    for index in range(count):
        start = index * hop_frames
        block = filtered[start : start + block_frames]
        powers[index] = float(np.sum(np.mean(block * block, axis=0)))
    loudness = -0.691 + 10.0 * np.log10(np.maximum(powers, 1e-30))
    absolute = loudness >= -70.0
    if not np.any(absolute):
        return float("-inf")
    absolute_power = float(np.mean(powers[absolute]))
    relative_threshold = -0.691 + 10.0 * np.log10(max(absolute_power, 1e-30)) - 10.0
    selected = absolute & (loudness >= relative_threshold)
    if not np.any(selected):
        return float("-inf")
    gated_power = float(np.mean(powers[selected]))
    return float(-0.691 + 10.0 * np.log10(max(gated_power, 1e-30)))


def _oversample_filter(oversampling: int) -> np.ndarray:
    half_taps = 24
    count = 2 * half_taps * oversampling + 1
    center = half_taps * oversampling
    indices = np.arange(count, dtype=np.float64)
    sinc = np.sinc((indices - center) / oversampling)
    window = np.kaiser(count, 8.0)
    return sinc * window


def oversample_signal(samples: np.ndarray, oversampling: int = 4) -> np.ndarray:
    values = np.asarray(samples, dtype=np.float64)
    one_channel = values.ndim == 1
    if one_channel:
        values = values[:, None]
    frames = values.shape[0]
    upsampled = np.zeros((frames * oversampling, values.shape[1]), dtype=np.float64)
    upsampled[::oversampling] = values
    kernel = _oversample_filter(oversampling)
    center = kernel.size // 2
    output = np.empty_like(upsampled)
    for channel_index in range(values.shape[1]):
        filtered = np.convolve(upsampled[:, channel_index], kernel, mode="full")
        output[:, channel_index] = filtered[center : center + frames * oversampling]
    return output[:, 0] if one_channel else output


def true_peak_dbtp(samples: np.ndarray, oversampling: int = 4) -> tuple[float, np.ndarray]:
    values = np.asarray(samples, dtype=np.float64)
    one_channel = values.ndim == 1
    if one_channel:
        values = values[:, None]
    upsampled = oversample_signal(values, oversampling)
    peak = float(np.max(np.abs(upsampled), initial=0.0))
    dbtp = 20.0 * np.log10(max(peak, 1e-15))
    return float(dbtp), upsampled


def _sliding_min(values: np.ndarray, size: int) -> np.ndarray:
    if size <= 1 or values.size == 0:
        return values.copy()
    radius = size // 2
    padded = np.pad(values, (radius, radius), mode="edge")
    return sliding_window_view(padded, size).min(axis=1)


def _sliding_max(values: np.ndarray, size: int) -> np.ndarray:
    if size <= 1 or values.size == 0:
        return values.copy()
    radius = size // 2
    padded = np.pad(values, (radius, radius), mode="edge")
    return sliding_window_view(padded, size).max(axis=1)


def limit_true_peak(
    samples: np.ndarray,
    ceiling_dbtp: float,
    sample_rate: int,
) -> tuple[np.ndarray, LimiterStats]:
    values = np.asarray(samples, dtype=np.float64)
    one_channel = values.ndim == 1
    if one_channel:
        values = values[:, None]
    ceiling = 10.0 ** (ceiling_dbtp / 20.0)
    lookahead = max(1, int(round(0.002 * sample_rate)))
    release = max(1, int(round(0.080 * sample_rate)))
    oversampling = 4
    up = oversample_signal(values, oversampling)
    frame_peak = np.max(np.abs(up), axis=1).reshape(values.shape[0], oversampling)
    support = np.max(frame_peak, axis=1)
    support = _sliding_max(support, 3)
    target = np.minimum(1.0, ceiling * 0.985 / np.maximum(support, 1e-15))
    attenuated = _sliding_min(target, 2 * lookahead + 1)
    gain = np.empty_like(attenuated)
    release_coefficient = float(np.exp(-1.0 / release))
    previous = 1.0
    for index in range(attenuated.size):
        desired = float(attenuated[index])
        if desired < previous:
            current = desired
        else:
            current = desired + (previous - desired) * release_coefficient
        current = min(current, float(target[index]))
        gain[index] = current
        previous = current
    limited = values * gain[:, None]
    triggered_frames = int(np.count_nonzero(gain < 1.0 - 1e-10))
    triggered_samples = triggered_frames * values.shape[1]
    minimum_gain_db = float(20.0 * np.log10(max(float(np.min(gain)), 1e-15)))
    stats = LimiterStats(
        gain=gain,
        triggered_frames=triggered_frames,
        triggered_channel_samples=triggered_samples,
        lookahead_samples=lookahead,
        minimum_gain_db=minimum_gain_db,
    )
    if one_channel:
        limited = limited[:, 0]
    return limited, stats


def master_bus(
    bus: np.ndarray,
    sample_rate: int,
    bit_depth: int,
    target_lufs: float,
    true_peak_ceiling_dbtp: float,
) -> MasterResult:
    values = np.asarray(bus, dtype=np.float64)
    if values.ndim != 2:
        raise ValueError("bus must be a stereo frame matrix")
    current = values.copy()
    initial_loudness = integrated_loudness(current, sample_rate)
    if not np.isfinite(initial_loudness):
        raise RenderError("rendered bus is silent")
    gain_db = target_lufs - initial_loudness
    attempts = 0
    limited = current
    stats = LimiterStats(
        gain=np.ones(current.shape[0]),
        triggered_frames=0,
        triggered_channel_samples=0,
        lookahead_samples=max(1, int(round(0.002 * sample_rate))),
        minimum_gain_db=0.0,
    )
    scale_value = float(1 << (bit_depth - 1))
    pcm = quantize_pcm(current, bit_depth)

    for attempts in range(1, 9):
        candidate = values * (10.0 ** (gain_db / 20.0))
        limited, stats = limit_true_peak(candidate, true_peak_ceiling_dbtp, sample_rate)
        pcm = quantize_pcm(limited, bit_depth)
        quantized = pcm.astype(np.float64) / scale_value
        measured = integrated_loudness(quantized, sample_rate)
        error = target_lufs - measured
        if abs(error) <= 0.04:
            break
        gain_db += error

    safety_scale = 1.0
    quantized = pcm.astype(np.float64) / scale_value
    peak_db, _ = true_peak_dbtp(quantized)
    if peak_db > true_peak_ceiling_dbtp:
        ceiling = 10.0 ** (true_peak_ceiling_dbtp / 20.0)
        safety_scale = ceiling / (10.0 ** (peak_db / 20.0)) * 0.999999
        pcm = quantize_pcm(quantized * safety_scale, bit_depth)
        quantized = pcm.astype(np.float64) / scale_value
        peak_db, _ = true_peak_dbtp(quantized)

    measured = integrated_loudness(quantized, sample_rate)
    deviation = measured - target_lufs
    compliant = abs(deviation) <= 0.5 and peak_db <= true_peak_ceiling_dbtp + 0.002
    return MasterResult(
        samples=quantized,
        pcm=pcm,
        integrated_lufs=float(measured),
        true_peak_dbtp=float(peak_db),
        target_lufs=target_lufs,
        deviation_lu=float(deviation),
        true_peak_ceiling_dbtp=true_peak_ceiling_dbtp,
        limiter=stats,
        safety_scale=float(safety_scale),
        attempts=attempts,
        compliant=bool(compliant),
    )
