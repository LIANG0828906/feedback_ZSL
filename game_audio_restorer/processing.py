"""Segmented, serializable restoration processing chain."""

from __future__ import annotations

import math
from typing import Any

import numpy as np

from .errors import BusinessError, ErrorCode
from .metrics import audio_metrics, integrated_loudness_lufs, peak_dbfs, rms_dbfs
from .recipes import Recipe

DEFAULT_SEGMENT_SECONDS = 1.0
_EPS = 1e-12


def segment_bounds(total_frames: int, sample_rate: int, segment_seconds: float) -> list[tuple[int, int]]:
    length = max(1, int(round(sample_rate * segment_seconds)))
    return [
        (start, min(start + length, total_frames))
        for start in range(0, total_frames, length)
    ]


def _as_frames(samples: np.ndarray) -> np.ndarray:
    arr = np.asarray(samples, dtype=np.float64)
    if arr.ndim == 1:
        arr = arr[:, None]
    if arr.ndim != 2:
        raise ValueError("audio must be frames x channels")
    return arr


def _map_channels(
    samples: np.ndarray, mode: str
) -> tuple[np.ndarray, dict[str, Any]]:
    arr = _as_frames(samples)
    channels = arr.shape[1]
    if mode == "preserve":
        mapped = arr.copy()
    elif mode == "mono_mix":
        mapped = np.mean(arr, axis=1, keepdims=True, dtype=np.float64)
    elif mode == "left":
        mapped = arr[:, 0:1].copy()
    elif mode == "right":
        mapped = arr[:, channels - 1 : channels].copy()
    else:
        raise ValueError(f"unsupported channel mode: {mode}")
    return mapped, {
        "stage": "channel_map",
        "mode": mode,
        "input_channels": channels,
        "output_channels": int(mapped.shape[1]),
        "mix_formula": {
            "preserve": "identity",
            "mono_mix": "mean(all input channels)",
            "left": "input channel 0",
            "right": "last input channel",
        }[mode],
    }


def _remove_dc(
    samples: np.ndarray, enabled: bool
) -> tuple[np.ndarray, dict[str, Any]]:
    arr = _as_frames(samples)
    means = np.mean(arr, axis=0) if arr.shape[0] else np.zeros(arr.shape[1])
    if enabled:
        processed = arr - means[None, :]
    else:
        processed = arr.copy()
    return processed, {
        "stage": "dc_offset",
        "enabled": enabled,
        "statistics": "per_channel",
        "mean_before": [round(float(value), 12) for value in means],
        "removed": [round(float(value), 12) for value in means] if enabled else [0.0] * arr.shape[1],
    }


def _silence_gate(
    samples: np.ndarray,
    sample_rate: int,
    threshold_dbfs: float | None,
) -> tuple[np.ndarray, dict[str, Any]]:
    arr = _as_frames(samples)
    window = max(1, int(round(sample_rate * 0.02)))
    fade = max(1, min(window // 2, int(round(sample_rate * 0.005))))
    params: dict[str, Any] = {
        "stage": "silence_gate",
        "enabled": threshold_dbfs is not None,
        "window_ms": 20.0,
        "fade_ms": round(fade * 1000.0 / sample_rate, 6),
        "threshold_dbfs": threshold_dbfs,
        "gated_windows": 0,
        "window_count": 0,
    }
    if threshold_dbfs is None or arr.shape[0] == 0:
        return arr.copy(), params

    starts = list(range(0, arr.shape[0], window))
    energies = []
    for start in starts:
        block = arr[start : start + window]
        energies.append(float(np.mean(np.square(block), dtype=np.float64)))
    energies_arr = np.asarray(energies)
    with np.errstate(divide="ignore"):
        levels = 10.0 * np.log10(np.maximum(energies_arr, 1e-24))
    gate_open = (levels >= threshold_dbfs).astype(np.float64)
    expanded = np.repeat(gate_open, window)[: arr.shape[0]]
    if fade > 1:
        kernel = np.hanning(fade + 2)[1:-1]
        kernel /= np.sum(kernel)
        expanded = np.convolve(expanded, kernel, mode="same")
    processed = arr * expanded[:, None]
    params["gated_windows"] = int(np.count_nonzero(gate_open == 0.0))
    params["window_count"] = len(starts)
    return processed, params


def _spectral_subtraction(
    samples: np.ndarray,
    sample_rate: int,
    recipe: Recipe,
) -> tuple[np.ndarray, dict[str, Any]]:
    arr = _as_frames(samples)
    strength = recipe.denoise_strength
    spectral = recipe.spectral_subtraction
    n_fft = max(32, int(round(sample_rate * spectral.segment_ms / 1000.0)))
    hop = max(1, n_fft // 2)
    frames_count = 1 if arr.shape[0] <= n_fft else int(math.ceil((arr.shape[0] - n_fft) / hop)) + 1
    span = (frames_count - 1) * hop + n_fft
    padded = np.pad(arr, ((0, max(0, span - arr.shape[0])), (0, 0)), mode="constant")
    window = np.hanning(n_fft + 1)[:-1]

    frames = np.empty((frames_count, n_fft, arr.shape[1]), dtype=np.float64)
    for index in range(frames_count):
        start = index * hop
        frames[index] = padded[start : start + n_fft] * window[:, None]

    spectra = np.fft.rfft(frames, axis=1)
    magnitude = np.abs(spectra)
    noise = np.percentile(magnitude, 20.0, axis=0)
    alpha = spectral.oversubtraction * (0.5 + 1.5 * strength)
    floor_gain = spectral.spectral_floor * (1.0 - 0.8 * strength)
    cleaned = np.maximum(magnitude - alpha * noise[None, :, :], floor_gain * noise[None, :, :])
    gain = cleaned / np.maximum(magnitude, _EPS)
    # Spectral subtraction must not silently remove DC when the recipe has
    # explicitly disabled the DC stage.
    gain[:, 0, :] = 1.0
    reconstructed_spectra = spectra * gain
    reconstructed = np.fft.irfft(reconstructed_spectra, n=n_fft, axis=1)
    reconstructed *= window[None, :, None]

    output = np.zeros_like(padded)
    normalization = np.zeros(padded.shape[0], dtype=np.float64)
    synth_window = window * window
    for index in range(frames_count):
        start = index * hop
        output[start : start + n_fft] += reconstructed[index]
        normalization[start : start + n_fft] += synth_window
    # Zero-valued Hann endpoints have extremely small overlap normalization.
    # Dividing by them creates numerical spikes that dominate loudness.
    valid = normalization > max(1e-7, 0.001 * float(np.max(normalization)))
    output[valid] /= normalization[valid, None]
    output[~valid] = 0.0
    output = output[: arr.shape[0]]

    return output, {
        "stage": "spectral_subtraction",
        "enabled": strength > 0.0,
        "denoise_strength": strength,
        "segment_ms": spectral.segment_ms,
        "oversubtraction": spectral.oversubtraction,
        "spectral_floor": spectral.spectral_floor,
        "effective_alpha": round(alpha, 9),
        "effective_floor_gain": round(floor_gain, 9),
        "n_fft_samples": n_fft,
        "hop_samples": hop,
        "window": "hann_periodic",
        "noise_percentile": 20.0,
        "stereo_alignment": "same frame boundaries per channel",
    }


def _ramped_gain(length: int, sample_rate: int, gain_db: float, ramp_ms: float) -> tuple[np.ndarray, int]:
    ramp = max(1, min(length, int(round(sample_rate * ramp_ms / 1000.0))))
    factor = 10.0 ** (gain_db / 20.0)
    envelope = np.full(length, factor, dtype=np.float64)
    if ramp > 1 and length > 1:
        fade = 0.5 - 0.5 * np.cos(np.linspace(0.0, math.pi, ramp))
        envelope[:ramp] = factor * fade
        envelope[-ramp:] = np.minimum(envelope[-ramp:], factor * fade[::-1])
    return envelope, ramp


def _gain_and_limit(
    samples: np.ndarray,
    sample_rate: int,
    recipe: Recipe,
) -> tuple[np.ndarray, dict[str, Any]]:
    arr = _as_frames(samples)
    loudness_before = integrated_loudness_lufs(arr, sample_rate)
    loudness_correction = 0.0
    if recipe.loudness_target_lufs is not None and arr.size:
        loudness_correction = (
            recipe.loudness_target_lufs - (loudness_before + recipe.gain_db)
        )
        loudness_correction = float(np.clip(loudness_correction, -24.0, 24.0))
    total_gain = recipe.gain_db + loudness_correction
    envelope, ramp = _ramped_gain(arr.shape[0], sample_rate, total_gain, 5.0)
    gained = arr * envelope[:, None]
    peak = float(np.max(np.abs(gained))) if gained.size else 0.0
    peak_before_limiter = peak
    ceiling = 10.0 ** (-0.18 / 20.0)
    limiter_gain_db = 0.0
    if peak > ceiling:
        limiter_gain_db = 20.0 * math.log10(ceiling / peak)
        gained *= 10.0 ** (limiter_gain_db / 20.0)
    return gained, {
        "stage": "gain_envelope_and_limiter",
        "fixed_gain_db": recipe.gain_db,
        "loudness_target_lufs": recipe.loudness_target_lufs,
        "loudness_before_lufs": round(loudness_before, 6),
        "loudness_correction_db": round(loudness_correction, 6),
        "total_gain_db": round(total_gain, 6),
        "gain_envelope": "constant with 5 ms raised-cosine edge ramps",
        "ramp_samples": ramp,
        "ramp_ms": 5.0,
        "limiter_ceiling_dbfs": -0.18,
        "limiter_applied_db": round(limiter_gain_db, 6),
        "peak_before_limiter_dbfs": round(
            max(-120.0, 20.0 * math.log10(peak_before_limiter))
            if peak_before_limiter > 1e-12
            else -120.0,
            6,
        ),
    }


def process_segment(
    samples: np.ndarray,
    sample_rate: int,
    recipe: Recipe,
    segment_index: int = 0,
) -> tuple[np.ndarray, dict[str, Any], dict[str, Any]]:
    """Run one complete segment without touching disk.

    Channel conversion happens before the shared DSP chain.  All stages return
    fresh arrays; any exception aborts the whole segment and therefore the
    caller cannot commit one stereo channel by itself.
    """

    original = _as_frames(samples)
    stage_params: list[dict[str, Any]] = []
    current = original
    try:
        current, params = _map_channels(current, recipe.channel_merge_mode)
        stage_params.append(params)
        current, params = _remove_dc(current, recipe.remove_dc)
        stage_params.append(params)
        current, params = _silence_gate(
            current, sample_rate, recipe.silence_threshold_dbfs
        )
        stage_params.append(params)
        current, params = _spectral_subtraction(current, sample_rate, recipe)
        stage_params.append(params)
        current, params = _gain_and_limit(current, sample_rate, recipe)
        stage_params.append(params)
    except BusinessError:
        raise
    except Exception as exc:
        failed_stage = stage_params[-1]["stage"] if stage_params else "channel_map"
        raise BusinessError(
            ErrorCode.PROCESSING,
            f"片段 {segment_index} 处理失败，未提交任何声道",
            {
                "segment": segment_index,
                "stage": failed_stage,
                "input_channels": int(original.shape[1]),
                "error_type": type(exc).__name__,
                "error": str(exc),
            },
        ) from exc

    metrics = audio_metrics(current, sample_rate)
    metrics["input_rms_dbfs"] = round(rms_dbfs(original), 6)
    metrics["input_peak_dbfs"] = round(peak_dbfs(original), 6)
    parameters = {
        "segment_index": segment_index,
        "sample_rate": sample_rate,
        "input_frames": int(original.shape[0]),
        "input_channels": int(original.shape[1]),
        "output_channels": int(current.shape[1]),
        "recipe_version": recipe.version,
        "recipe_hash": recipe.hash(),
        "stages": stage_params,
    }
    return current, metrics, parameters
