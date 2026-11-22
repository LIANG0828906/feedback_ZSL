"""Segment-local STFT, noise estimation, spectral subtraction and exact OLA."""

from __future__ import annotations

from dataclasses import dataclass
from functools import lru_cache

import numpy as np
from scipy.ndimage import uniform_filter1d
from scipy.signal import get_window

from .config import RepairConfig
from .errors import param_error


@dataclass(frozen=True)
class STFTResult:
    spectrum: np.ndarray
    n_samples: int
    pad_left: int
    frame_size: int
    hop_size: int
    window: np.ndarray


@dataclass(frozen=True)
class DSPResult:
    output: np.ndarray
    mean_gain: float
    attenuation_db: float
    noise_power_mean: float


@lru_cache(maxsize=64)
def _window(name: str, frame_size: int) -> np.ndarray:
    if name == "rectangular":
        return np.ones(frame_size, dtype=np.float64)
    window = get_window(name, frame_size, fftbins=True).astype(np.float64)
    return np.ascontiguousarray(window)


def analysis_frames(samples: np.ndarray, frame_size: int, hop_size: int) -> tuple[np.ndarray, int]:
    """Zero-pad once, then form frames whose starts advance by exactly one hop."""

    x = np.asarray(samples, dtype=np.float64)
    if x.ndim == 1:
        x = x[:, None]
    n_samples = x.shape[0]
    if n_samples <= 0:
        raise param_error("cannot frame an empty segment")
    pad_left = frame_size // 2
    span = n_samples + 2 * pad_left - frame_size
    n_frames = 1 + max(0, int(np.ceil(span / hop_size)))
    pad_right = max(0, (n_frames - 1) * hop_size + frame_size - pad_left - n_samples)
    padded = np.pad(x, ((pad_left, pad_right), (0, 0)), mode="constant")
    view = np.lib.stride_tricks.sliding_window_view(padded, frame_size, axis=0)
    frames = np.ascontiguousarray(view[::hop_size].transpose(0, 2, 1))
    return frames, pad_left


def stft(samples: np.ndarray, config: RepairConfig) -> STFTResult:
    config.validate()
    x = np.asarray(samples, dtype=np.float64)
    if x.ndim == 1:
        x = x[:, None]
    frames, pad_left = analysis_frames(x, config.frame_size, config.hop_size)
    window = _window(config.window, config.frame_size)
    windowed = frames * window[None, :, None]
    spectrum = np.fft.rfft(windowed, axis=1)
    return STFTResult(
        spectrum=spectrum,
        n_samples=int(x.shape[0]),
        pad_left=pad_left,
        frame_size=config.frame_size,
        hop_size=config.hop_size,
        window=window,
    )


def istft(result: STFTResult, n_samples: int | None = None) -> np.ndarray:
    """Windowed overlap-add with per-sample synthesis normalization.

    The requested sample count is trimmed from the padded OLA buffer.  A zero
    synthesis weight in the requested region is rejected rather than silently
    filled, so frame/hop combinations that cannot reconstruct the timeline
    surface as E_PARAM.
    """

    target_length = result.n_samples if n_samples is None else int(n_samples)
    if target_length <= 0:
        raise param_error("target sample count must be positive")
    frames = np.fft.irfft(result.spectrum, n=result.frame_size, axis=1)
    n_frames = frames.shape[0]
    output_length = (n_frames - 1) * result.hop_size + result.frame_size
    output = np.zeros((output_length, frames.shape[2]), dtype=np.float64)
    weight = np.zeros(output_length, dtype=np.float64)
    squared_window = result.window * result.window
    for index in range(n_frames):
        start = index * result.hop_size
        output[start : start + result.frame_size] += (
            frames[index] * result.window[:, None]
        )
        weight[start : start + result.frame_size] += squared_window

    stop = result.pad_left + target_length
    if stop > output_length:
        raise param_error("not enough OLA frames to cover requested samples")
    target_weight = weight[result.pad_left : stop]
    if np.any(target_weight <= 1e-12):
        raise param_error(
            "window and hop do not cover the full timeline",
            window=result.window.tolist()[:8],
            hop_size=result.hop_size,
            zero_weight_samples=int(np.count_nonzero(target_weight <= 1e-12)),
        )
    trimmed = output[result.pad_left : stop] / target_weight[:, None]
    return np.ascontiguousarray(trimmed)


def estimate_noise_power(spectrum: np.ndarray, config: RepairConfig) -> np.ndarray:
    """Estimate a stationary noise profile per channel and frequency bin."""

    power = np.abs(spectrum) ** 2
    n_frames = power.shape[0]
    if n_frames == 0:
        raise param_error("spectrum has no frames")
    count = min(config.noise_frames, n_frames)
    if config.noise_mode == "leading":
        selected = power[:count]
    else:
        energy = np.sum(power, axis=(1, 2))
        search_limit = min(n_frames, max(count * 4, 24))
        search_energy = energy[:search_limit]
        threshold = float(np.quantile(search_energy, 0.2))
        mask = search_energy <= threshold
        if not np.any(mask):
            mask[0] = True
        selected = power[:search_limit][mask]
    profile = np.mean(selected, axis=0, keepdims=True)
    # Never let a padded leading frame collapse the estimate to zero.
    floor_power = max(float(np.max(profile)) * 1e-10, 1e-16)
    return np.maximum(profile, floor_power)


def _smooth_gain(
    gain: np.ndarray, frequency_width: int, temporal_amount: float
) -> np.ndarray:
    smoothed = gain
    if frequency_width > 1:
        smoothed = uniform_filter1d(
            smoothed, size=frequency_width, axis=1, mode="nearest"
        )
    if temporal_amount > 0.0:
        temporal = np.empty_like(smoothed)
        temporal[0] = smoothed[0]
        decay = float(temporal_amount)
        innovation = 1.0 - decay
        for frame_index in range(1, smoothed.shape[0]):
            temporal[frame_index] = (
                innovation * smoothed[frame_index] + decay * temporal[frame_index - 1]
            )
        smoothed = temporal
    return np.clip(smoothed, 0.0, 1.0)


def denoise_segment(samples: np.ndarray, config: RepairConfig) -> DSPResult:
    """Repair one fixed segment using only that segment's samples."""

    config.validate()
    x = np.asarray(samples, dtype=np.float64)
    if x.ndim == 1:
        x = x[:, None]
    if x.shape[0] == 0:
        raise param_error("cannot process an empty segment")
    transform = stft(x, config)
    power = np.abs(transform.spectrum) ** 2
    noise_power = estimate_noise_power(transform.spectrum, config)
    ratio = noise_power / np.maximum(power, 1e-20)
    gain_sq = np.clip(1.0 - float(config.strength) * ratio, float(config.floor) ** 2, 1.0)
    gain = _smooth_gain(
        np.sqrt(gain_sq),
        frequency_width=min(config.freq_smooth, transform.spectrum.shape[1]),
        temporal_amount=float(config.temporal_smooth),
    )
    output = istft(
        STFTResult(
            spectrum=transform.spectrum * gain,
            n_samples=transform.n_samples,
            pad_left=transform.pad_left,
            frame_size=transform.frame_size,
            hop_size=transform.hop_size,
            window=transform.window,
        ),
        x.shape[0],
    )
    if output.shape != x.shape:
        raise RuntimeError(
            f"segment reconstruction shape mismatch: {output.shape} != {x.shape}"
        )
    mean_gain = float(np.mean(gain))
    attenuation_db = 20.0 * float(np.log10(max(mean_gain, 1e-12)))
    return DSPResult(
        output=np.ascontiguousarray(output),
        mean_gain=mean_gain,
        attenuation_db=attenuation_db,
        noise_power_mean=float(np.mean(noise_power)),
    )


def reconstruct_without_gain(samples: np.ndarray, config: RepairConfig) -> np.ndarray:
    """Identity STFT/ISTFT path used by reconstruction tests."""

    transform = stft(samples, config)
    return istft(transform, transform.n_samples)

