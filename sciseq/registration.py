"""Subpixel translation registration using phase correlation."""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from .config import RegistrationConfig
from .errors import SciSeqError

AMPLITUDE_WEIGHT_EXPONENT = 0.8


@dataclass(frozen=True)
class RegistrationEstimate:
    offset: tuple[float, float]
    phase_peak: float
    integer_peak: tuple[int, int]
    upsample_factor: int


def _window_2d(shape: tuple[int, int], name: str) -> np.ndarray | None:
    if name == "none":
        return None
    height, width = shape
    if name == "hann":
        row = np.hanning(height)
        column = np.hanning(width)
    elif name == "hamming":
        row = np.hamming(height)
        column = np.hamming(width)
    elif name == "blackman":
        row = np.blackman(height)
        column = np.blackman(width)
    else:
        raise SciSeqError(
            "E_PARA",
            "unsupported registration window",
            field="registration.window",
            details={"actual": name},
        )
    return np.outer(row, column).astype(np.float64)


def _sample_local_dft(
    cross_power: np.ndarray,
    integer_peak: tuple[int, int],
    upsample_factor: int,
) -> tuple[float, float, float]:
    height, width = cross_power.shape
    peak_y, peak_x = integer_peak
    fy = np.fft.fftfreq(height)
    fx = np.fft.fftfreq(width)
    centered = cross_power * np.exp(
        2j * np.pi * (fy[:, None] * peak_y + fx[None, :] * peak_x)
    )
    half = max(1, upsample_factor // 2)
    deltas = np.arange(-half, half + 1, dtype=np.float64) / float(upsample_factor)
    matrix_y = np.exp(2j * np.pi * fy[:, None] * deltas[None, :])
    matrix_x = np.exp(2j * np.pi * fx[:, None] * deltas[None, :])
    samples = matrix_y.T @ centered @ matrix_x / float(height * width)
    magnitude = np.abs(samples)
    best_y, best_x = np.unravel_index(int(np.argmax(magnitude)), magnitude.shape)
    step = 1.0 / float(upsample_factor)
    delta_y = 0.0
    delta_x = 0.0
    if 0 < best_y < magnitude.shape[0] - 1:
        left = float(magnitude[best_y - 1, best_x])
        center = float(magnitude[best_y, best_x])
        right = float(magnitude[best_y + 1, best_x])
        denominator = left - 2.0 * center + right
        if abs(denominator) > 1e-15:
            delta_y = step * 0.5 * (left - right) / denominator
    if 0 < best_x < magnitude.shape[1] - 1:
        left = float(magnitude[best_y, best_x - 1])
        center = float(magnitude[best_y, best_x])
        right = float(magnitude[best_y, best_x + 1])
        denominator = left - 2.0 * center + right
        if abs(denominator) > 1e-15:
            delta_x = step * 0.5 * (left - right) / denominator
    return (
        float(peak_x + deltas[best_x] + delta_x),
        float(peak_y + deltas[best_y] + delta_y),
        float(magnitude[best_y, best_x]),
    )


def _signed_wrapped_peak(peak: tuple[int, int], shape: tuple[int, int]) -> tuple[int, int]:
    y, x = peak
    height, width = shape
    if y > height // 2:
        y -= height
    if x > width // 2:
        x -= width
    return y, x


def estimate_translation(
    reference: np.ndarray,
    moving: np.ndarray,
    config: RegistrationConfig,
    *,
    frame_index: int | None = None,
) -> RegistrationEstimate:
    """Estimate moving-frame content displacement relative to the reference."""

    config.validate(reference.shape)
    if reference.shape != moving.shape or reference.ndim != 2:
        raise SciSeqError(
            "E_META",
            "registration inputs must be same-shape 2D frames",
            frame=frame_index,
            field="shape",
            details={"reference": list(reference.shape), "moving": list(moving.shape)},
        )
    if not np.all(np.isfinite(reference)) or not np.all(np.isfinite(moving)):
        raise SciSeqError(
            "E_DECODE",
            "registration input contains non-finite pixels",
            frame=frame_index,
            field="pixels",
        )

    reference_work = reference.astype(np.float64, copy=False)
    moving_work = moving.astype(np.float64, copy=False)
    window = _window_2d(reference.shape, config.window)
    if window is not None:
        reference_work = reference_work * window
        moving_work = moving_work * window
    reference_work = reference_work - float(np.mean(reference_work))
    moving_work = moving_work - float(np.mean(moving_work))

    spectrum_reference = np.fft.fft2(reference_work)
    spectrum_moving = np.fft.fft2(moving_work)
    cross_power = spectrum_moving * np.conj(spectrum_reference)
    magnitude = np.abs(cross_power)
    scale = float(np.max(magnitude)) if magnitude.size else 0.0
    if not math.isfinite(scale) or scale <= 1e-12:
        raise SciSeqError(
            "E_REG",
            "registration signal is degenerate",
            frame=frame_index,
            field="phase_correlation",
            details={"max_cross_power": scale},
        )
    weighted_magnitude = np.power(
        np.maximum(magnitude / scale, 1e-12),
        AMPLITUDE_WEIGHT_EXPONENT,
    )
    cross_power = cross_power / np.maximum(magnitude, scale * 1e-12) * weighted_magnitude
    correlation = np.fft.ifft2(cross_power).real
    raw_peak_y, raw_peak_x = np.unravel_index(int(np.argmax(correlation)), correlation.shape)
    integer_peak = _signed_wrapped_peak(
        (int(raw_peak_y), int(raw_peak_x)),
        correlation.shape,
    )
    peak_value = float(correlation[raw_peak_y, raw_peak_x])

    if int(config.upsample_factor) == 1:
        subpixel_x = float(integer_peak[1])
        subpixel_y = float(integer_peak[0])
    else:
        subpixel_x, subpixel_y, peak_value = _sample_local_dft(
            cross_power,
            (int(raw_peak_y), int(raw_peak_x)),
            int(config.upsample_factor),
        )
        # Undo the raw correlation-array wrap before returning a signed displacement.
        if raw_peak_y > correlation.shape[0] // 2:
            subpixel_y -= correlation.shape[0]
        if raw_peak_x > correlation.shape[1] // 2:
            subpixel_x -= correlation.shape[1]

    if abs(subpixel_x) > config.max_shift or abs(subpixel_y) > config.max_shift:
        raise SciSeqError(
            "E_REG",
            "estimated displacement exceeds max_shift",
            frame=frame_index,
            field="registration.max_shift",
            details={
                "actual_displacement": [subpixel_x, subpixel_y],
                "max_shift": float(config.max_shift),
            },
        )
    return RegistrationEstimate(
        offset=(float(subpixel_x), float(subpixel_y)),
        phase_peak=float(peak_value),
        integer_peak=integer_peak,
        upsample_factor=int(config.upsample_factor),
    )


def translate_to_reference(image: np.ndarray, offset: tuple[float, float]) -> np.ndarray:
    """Apply the inverse of the measured content shift to align a frame."""

    dx, dy = offset
    height, width = image.shape
    fy = np.fft.fftfreq(height)[:, None]
    fx = np.fft.fftfreq(width)[None, :]
    phase = np.exp(2j * np.pi * (fx * dx + fy * dy))
    aligned = np.fft.ifft2(np.fft.fft2(image.astype(np.float64)) * phase).real
    return np.ascontiguousarray(aligned, dtype=np.float32)

