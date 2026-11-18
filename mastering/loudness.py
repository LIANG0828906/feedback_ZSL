from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np


def _biquad_impulse(coefficients: tuple[float, float, float], length: int) -> np.ndarray:
    b0, b1, b2, a1, a2 = coefficients
    impulse = np.zeros(length, dtype=np.float64)
    x1 = x2 = y1 = y2 = 0.0
    for index in range(length):
        x0 = 1.0 if index == 0 else 0.0
        y0 = b0 * x0 + b1 * x1 + b2 * x2 - a1 * y1 - a2 * y2
        impulse[index] = y0
        x2, x1 = x1, x0
        y2, y1 = y1, y0
    return impulse


def k_weighting_coefficients(sample_rate: int) -> tuple[np.ndarray, np.ndarray]:
    """ITU-R BS.1770 K-weighting biquads, bilinear-designed for any rate."""

    f0 = 1681.974450955533
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    k = math.tan(math.pi * f0 / sample_rate)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh ** 0.4996667741545416
    a0 = 1.0 + k / q + k * k
    shelf = np.asarray(
        [
            (vh + vb * k / q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / q + k * k) / a0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / q + k * k) / a0,
        ],
        dtype=np.float64,
    )

    f0 = 38.13547087602444
    q = 0.5003270373238773
    k = math.tan(math.pi * f0 / sample_rate)
    a0 = 1.0 + k / q + k * k
    highpass = np.asarray(
        [
            1.0,
            -2.0,
            1.0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / q + k * k) / a0,
        ],
        dtype=np.float64,
    )
    return shelf, highpass


def _fft_convolve_same(x: np.ndarray, impulse: np.ndarray) -> np.ndarray:
    """Overlap-add FIR convolution; keeps memory bounded for long masters."""

    length = x.shape[0]
    impulse_length = impulse.shape[0]
    if impulse_length == 1:
        return x * impulse[0]
    block = 131072
    fft_length = 1 << (block + impulse_length - 1).bit_length()
    spectrum = np.fft.rfft(impulse, fft_length)
    result = np.zeros(length + impulse_length - 1, dtype=np.float64)
    for start in range(0, length, block):
        chunk = x[start : start + block]
        chunk_spectrum = np.fft.rfft(chunk, fft_length)
        chunk_spectrum *= spectrum
        filtered = np.fft.irfft(chunk_spectrum, fft_length)
        valid = min(block + impulse_length - 1, result.shape[0] - start)
        result[start : start + valid] += filtered[:valid]
    return result[:length]


def k_weight_channels(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    """K-weight each channel using a long, deterministic FIR approximation."""

    shelf, highpass = k_weighting_coefficients(sample_rate)
    impulse_length = max(2048, min(65536, int(round(sample_rate * 0.5))))
    shelf_ir = _biquad_impulse(
        (
            float(shelf[0]),
            float(shelf[1]),
            float(shelf[2]),
            float(shelf[3]),
            float(shelf[4]),
        ),
        impulse_length,
    )
    highpass_ir = _biquad_impulse(
        (
            float(highpass[0]),
            float(highpass[1]),
            float(highpass[2]),
            float(highpass[3]),
            float(highpass[4]),
        ),
        impulse_length,
    )
    fft_length = 1 << (shelf_ir.shape[0] + highpass_ir.shape[0] - 1).bit_length()
    combined = np.fft.irfft(
        np.fft.rfft(shelf_ir, fft_length) * np.fft.rfft(highpass_ir, fft_length),
        fft_length,
    )[:impulse_length]
    filtered = np.empty_like(samples, dtype=np.float64)
    for channel in range(samples.shape[1]):
        filtered[:, channel] = _fft_convolve_same(samples[:, channel], combined)
    return filtered


@dataclass(frozen=True)
class LoudnessResult:
    integrated_lufs: float
    block_count: int
    gated_block_count: int
    absolute_gate_lufs: float
    relative_gate_lufs: float | None

    def as_dict(self) -> dict[str, Any]:
        return {
            "integrated_lufs": round(self.integrated_lufs, 9),
            "block_duration_seconds": 0.4,
            "block_step_seconds": 0.1,
            "block_count": self.block_count,
            "gated_block_count": self.gated_block_count,
            "absolute_gate_lufs": self.absolute_gate_lufs,
            "relative_gate_lufs": (
                None
                if self.relative_gate_lufs is None
                else round(self.relative_gate_lufs, 9)
            ),
        }


def integrated_loudness(samples: np.ndarray, sample_rate: int) -> LoudnessResult:
    """ITU-R BS.1770-4 integrated loudness with 400 ms / 100 ms gating."""

    if samples.ndim != 2:
        raise ValueError("samples must have shape (frames, channels)")
    if samples.shape[0] == 0:
        raise ValueError("cannot measure empty audio")
    channel_weights = np.ones(samples.shape[1], dtype=np.float64)
    weighted = k_weight_channels(samples, sample_rate)
    block_frames = max(1, int(round(0.4 * sample_rate)))
    step_frames = max(1, int(round(0.1 * sample_rate)))
    if weighted.shape[0] < block_frames:
        raise ValueError("audio must be at least 400 ms for BS.1770 integrated loudness")

    starts = np.arange(
        0,
        weighted.shape[0] - block_frames + 1,
        step_frames,
        dtype=np.int64,
    )
    block_powers = np.empty(starts.shape[0], dtype=np.float64)
    for index, start in enumerate(starts):
        block = weighted[start : start + block_frames] ** 2
        block_powers[index] = float(np.sum(np.mean(block, axis=0) * channel_weights))
    block_loudness = -0.691 + 10.0 * np.log10(np.maximum(block_powers, 1e-30))
    absolute_gate = -70.0
    above_absolute = block_loudness > absolute_gate
    if not np.any(above_absolute):
        raise ValueError("audio is below the BS.1770 absolute gate")
    ungated_power = float(np.mean(block_powers[above_absolute]))
    ungated_loudness = -0.691 + 10.0 * math.log10(max(ungated_power, 1e-30))
    relative_gate = ungated_loudness - 10.0
    above_relative = above_absolute & (block_loudness > relative_gate)
    if not np.any(above_relative):
        raise ValueError("no blocks pass the BS.1770 relative gate")
    gated_power = float(np.mean(block_powers[above_relative]))
    integrated = -0.691 + 10.0 * math.log10(max(gated_power, 1e-30))
    return LoudnessResult(
        integrated_lufs=float(integrated),
        block_count=int(starts.shape[0]),
        gated_block_count=int(np.count_nonzero(above_relative)),
        absolute_gate_lufs=absolute_gate,
        relative_gate_lufs=float(relative_gate),
    )


def loudness_gain_for_target(
    samples: np.ndarray,
    sample_rate: int,
    target_lufs: float,
    tolerance_lu: float = 0.5,
) -> tuple[np.ndarray, dict[str, Any]]:
    """Apply a pre-limiter loudness gain and reject an out-of-tolerance derivation."""

    before = integrated_loudness(samples, sample_rate)
    gain_db = float(target_lufs) - before.integrated_lufs
    gain_linear = 10.0 ** (gain_db / 20.0)
    calibrated = samples * gain_linear
    after = integrated_loudness(calibrated, sample_rate)
    deviation = after.integrated_lufs - float(target_lufs)
    report = {
        "target_lufs": round(float(target_lufs), 9),
        "pre_gain_lufs": round(before.integrated_lufs, 9),
        "gain_db": round(gain_db, 9),
        "calibrated_lufs": round(after.integrated_lufs, 9),
        "deviation_lu": round(deviation, 9),
        "tolerance_lu": tolerance_lu,
    }
    if abs(deviation) > tolerance_lu:
        from .errors import LoudnessOutOfTolerance

        raise LoudnessOutOfTolerance(
            "derived loudness gain did not meet the configured tolerance",
            report,
        )
    return calibrated, report

