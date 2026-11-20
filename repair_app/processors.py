from __future__ import annotations

import hashlib
import json
from typing import Iterable

import numpy as np
from scipy import signal

from .models import ChainConfig, Interval


def active_mask(
    intervals: Iterable[Interval], start_frame: int, frames: int
) -> tuple[np.ndarray, list[Interval]]:
    mask = np.zeros(frames, dtype=bool)
    relevant: list[Interval] = []
    block_end = start_frame + frames
    for interval in intervals:
        if interval.end_frame <= start_frame or interval.start_frame >= block_end:
            continue
        relevant.append(interval)
        local_start = max(interval.start_frame, start_frame) - start_frame
        local_end = min(interval.end_frame, block_end) - start_frame
        mask[local_start:local_end] = True
    return mask, relevant


def block_config_manifest(
    *,
    chain: ChainConfig,
    intervals: Iterable[Interval],
    start_frame: int,
    frames: int,
    sample_rate: int,
    channels: int,
) -> dict[str, object]:
    mask, relevant = active_mask(intervals, start_frame, frames)
    if not np.any(mask):
        return {
            "mode": "pass_through",
            "start_frame": start_frame,
            "frames": frames,
            "sample_rate": sample_rate,
            "channels": channels,
        }
    block_end = start_frame + frames
    interval_manifest = []
    for interval in relevant:
        interval_manifest.append(
            {
                "id": interval.id,
                "active_start": max(interval.start_frame, start_frame) - start_frame,
                "active_end": min(interval.end_frame, block_end) - start_frame,
                "start_frame": interval.start_frame,
                "end_frame": interval.end_frame,
                "gain_db": interval.gain_db,
                "fade_in_ms": interval.fade_in_ms,
                "fade_out_ms": interval.fade_out_ms,
            }
        )
    return {
        "mode": "repair",
        "start_frame": start_frame,
        "frames": frames,
        "sample_rate": sample_rate,
        "channels": channels,
        "chain": chain.to_dict(),
        "intervals": interval_manifest,
    }


def manifest_sha256(manifest: dict[str, object]) -> str:
    canonical = json.dumps(
        manifest,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


def config_sha256(
    *,
    chain: ChainConfig,
    intervals: Iterable[Interval],
    start_frame: int,
    frames: int,
    sample_rate: int,
    channels: int,
) -> str:
    return manifest_sha256(
        block_config_manifest(
            chain=chain,
            intervals=intervals,
            start_frame=start_frame,
            frames=frames,
            sample_rate=sample_rate,
            channels=channels,
        )
    )


def _blend(original: np.ndarray, processed: np.ndarray, mask: np.ndarray) -> np.ndarray:
    result = original.astype(np.float64, copy=True)
    result[mask] = processed[mask]
    return result


def _remove_dc(
    samples: np.ndarray, mask: np.ndarray, params: dict[str, object]
) -> np.ndarray:
    if not bool(params.get("enabled", True)):
        return samples
    if not np.any(mask):
        return samples
    processed = samples.astype(np.float64, copy=True)
    if bool(params.get("per_channel", True)):
        for channel in range(processed.shape[1]):
            mean = float(np.mean(processed[mask, channel], dtype=np.float64))
            processed[mask, channel] -= mean
    else:
        mean = float(np.mean(processed[mask], dtype=np.float64))
        processed[mask] -= mean
    return _blend(samples, processed, mask)


def _noise_gate(
    samples: np.ndarray,
    mask: np.ndarray,
    sample_rate: int,
    params: dict[str, object],
) -> np.ndarray:
    if not bool(params.get("enabled", True)) or not np.any(mask):
        return samples
    threshold = 32768.0 * 10.0 ** (float(params["threshold_db"]) / 20.0)
    attenuation = 10.0 ** (float(params["attenuation_db"]) / 20.0)
    attack_coefficient = float(
        np.exp(-1.0 / max(1.0, sample_rate * float(params["attack_ms"]) / 1000.0))
    )
    release_coefficient = float(
        np.exp(-1.0 / max(1.0, sample_rate * float(params["release_ms"]) / 1000.0))
    )
    processed = samples.astype(np.float64, copy=True)
    for channel in range(processed.shape[1]):
        channel_samples = processed[:, channel]
        envelope = 0.0
        gain = np.ones(channel_samples.shape[0], dtype=np.float64)
        for index, value in enumerate(np.abs(channel_samples)):
            coefficient = (
                attack_coefficient if value > envelope else release_coefficient
            )
            envelope = coefficient * envelope + (1.0 - coefficient) * float(value)
            if envelope < threshold:
                gain[index] = attenuation
        processed[:, channel] = channel_samples * gain
    return _blend(samples, processed, mask)


def _spectral_subtract(
    samples: np.ndarray,
    mask: np.ndarray,
    sample_rate: int,
    params: dict[str, object],
) -> np.ndarray:
    if not bool(params.get("enabled", True)) or not np.any(mask):
        return samples
    frames = samples.shape[0]
    n_fft = int(params["n_fft"])
    hop = int(params["hop_length"])
    if frames < 2:
        return samples
    n_fft = min(n_fft, max(64, 1 << int(np.floor(np.log2(max(64, frames))))))
    hop = min(hop, max(1, n_fft // 2))
    percentile = float(params["noise_percentile"])
    oversubtraction = float(params["oversubtraction"])
    reduction = 10.0 ** (float(params["reduction_db"]) / 20.0)
    floor = 10.0 ** (float(params["floor_db"]) / 20.0)
    processed = samples.astype(np.float64, copy=True)
    for channel in range(processed.shape[1]):
        _, _, spectrum = signal.stft(
            processed[:, channel],
            fs=float(sample_rate),
            window="hann",
            nperseg=n_fft,
            noverlap=n_fft - hop,
            boundary="zeros",
            padded=True,
        )
        magnitude = np.abs(spectrum)
        phase = np.angle(spectrum)
        noise = np.percentile(magnitude, percentile, axis=1, keepdims=True)
        cleaned = np.maximum(
            magnitude - noise * oversubtraction * reduction,
            magnitude * floor,
        )
        _, reconstructed = signal.istft(
            cleaned * np.exp(1j * phase),
            fs=float(sample_rate),
            window="hann",
            nperseg=n_fft,
            noverlap=n_fft - hop,
            boundary="zeros",
        )
        if reconstructed.shape[0] < frames:
            reconstructed = np.pad(reconstructed, (0, frames - reconstructed.shape[0]))
        processed[:, channel] = reconstructed[:frames]
    return _blend(samples, processed, mask)


def _interval_envelope(
    interval: Interval, start_frame: int, frames: int, sample_rate: int
) -> tuple[int, int, np.ndarray]:
    block_end = start_frame + frames
    local_start = max(interval.start_frame, start_frame) - start_frame
    local_end = min(interval.end_frame, block_end) - start_frame
    absolute_positions = np.arange(
        start_frame + local_start, start_frame + local_end, dtype=np.float64
    )
    linear_gain = 10.0 ** (interval.gain_db / 20.0)
    envelope = np.full(absolute_positions.shape, linear_gain, dtype=np.float64)
    fade_in_samples = interval.fade_in_ms * sample_rate / 1000.0
    if fade_in_samples > 0.0:
        fade = (absolute_positions - float(interval.start_frame)) / fade_in_samples
        envelope *= np.clip(fade, 0.0, 1.0)
    fade_out_samples = interval.fade_out_ms * sample_rate / 1000.0
    if fade_out_samples > 0.0:
        fade = (float(interval.end_frame) - absolute_positions) / fade_out_samples
        envelope *= np.clip(fade, 0.0, 1.0)
    return local_start, local_end, envelope


def _gain_envelope_and_limit(
    samples: np.ndarray,
    mask: np.ndarray,
    relevant_intervals: list[Interval],
    start_frame: int,
    sample_rate: int,
    limiter_params: dict[str, object],
) -> np.ndarray:
    processed = samples.astype(np.float64, copy=True)
    for interval in relevant_intervals:
        local_start, local_end, envelope = _interval_envelope(
            interval, start_frame, samples.shape[0], sample_rate
        )
        if local_end > local_start:
            processed[local_start:local_end] *= envelope[:, None]

    if bool(limiter_params.get("enabled", True)) and np.any(mask):
        threshold = 32767.0 * 10.0 ** (float(limiter_params["threshold_db"]) / 20.0)
        active = processed[mask]
        peak = float(np.max(np.abs(active))) if active.size else 0.0
        if peak > threshold > 0.0:
            processed[mask] *= threshold / peak
    return processed


def process_block(
    *,
    samples: np.ndarray,
    start_frame: int,
    sample_rate: int,
    chain: ChainConfig,
    intervals: Iterable[Interval],
) -> np.ndarray:
    """Run the complete repair chain for exactly one fixed-size block."""

    array = np.asarray(samples)
    if array.dtype != np.int16 or array.ndim != 2:
        raise ValueError("block must be a 2-D int16 array")
    mask, relevant = active_mask(intervals, start_frame, array.shape[0])
    if not np.any(mask):
        return array.copy()

    chain_dict = chain.to_dict()
    processed = _remove_dc(array, mask, chain_dict["dc_removal"])
    processed = _noise_gate(processed, mask, sample_rate, chain_dict["noise_gate"])
    processed = _spectral_subtract(
        processed, mask, sample_rate, chain_dict["spectral_subtraction"]
    )
    processed = _gain_envelope_and_limit(
        processed,
        mask,
        relevant,
        start_frame,
        sample_rate,
        chain_dict["limiter"],
    )
    quantized = np.clip(np.rint(processed), -32768.0, 32767.0).astype(np.int16)
    blended = array.copy()
    blended[mask] = quantized[mask]
    return blended

