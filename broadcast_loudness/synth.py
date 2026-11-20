"""Deterministic local multi-channel test material and independent references."""

from __future__ import annotations

import math
import os
from pathlib import Path
from typing import Any

import numpy as np

from .dsp import (
    K_PRE_A,
    K_PRE_B,
    K_RLB_A,
    K_RLB_B,
    block_bounds,
    direct_biquad_reference,
    lufs_from_energy,
)
from .params import (
    SAMPLE_RATE,
    AnalysisParameters,
    canonical_json,
)
from .wavio import AudioBuffer, read_wav, write_wav

GENERATOR_ID = "broadcast-loudness-deterministic-synth-v1"


def synthesize_signal(
    seed: int,
    channels: int,
    duration_seconds: float = 12.0,
    *,
    sample_rate: int = SAMPLE_RATE,
) -> np.ndarray:
    """Create a repeatable choral-like chord plus exponential sweep."""
    if channels not in range(1, 7):
        raise ValueError("channels must be between 1 and 6")
    if sample_rate != SAMPLE_RATE:
        raise ValueError("generator only supports 48 kHz")
    frames = max(1, round(duration_seconds * sample_rate))
    time = np.arange(frames, dtype=np.float64) / sample_rate
    rng = np.random.default_rng(seed)
    channel_gains = {
        1: (1.0,),
        2: (0.92, 0.88),
        3: (0.82, 0.78, 0.72),
        4: (0.78, 0.74, 0.52, 0.49),
        5: (0.78, 0.74, 0.66, 0.50, 0.46),
        6: (0.78, 0.73, 0.65, 0.22, 0.48, 0.43),
    }[channels]
    note_length = 0.62
    notes = np.asarray([196.0, 220.0, 246.94, 293.66, 329.63, 293.66, 246.94, 220.0])
    note_index = np.minimum((time / note_length).astype(np.int64), notes.size - 1)
    note_start = note_index.astype(np.float64) * note_length
    local_time = time - note_start
    note_frequency = notes[note_index]
    vibrato = 1.0 + 0.004 * np.sin(2.0 * np.pi * 5.2 * time + seed % 13)
    output = np.empty((frames, channels), dtype=np.float64)
    sweep_end = 6_200.0
    sweep_start = 95.0
    sweep_duration = max(duration_seconds, 1e-9)
    ratio = (sweep_end / sweep_start) ** (1.0 / sweep_duration)
    sweep_phase = (
        2.0 * np.pi * sweep_start * (np.power(ratio, time) - 1.0) / math.log(ratio)
    )
    sweep_gate = np.clip(
        np.sin(np.pi * np.mod(time, 1.75) / 1.75) ** 2,
        0.0,
        1.0,
    )
    for channel in range(channels):
        phase = 2.0 * np.pi * np.cumsum(note_frequency * vibrato) / SAMPLE_RATE
        vocal = (
            np.sin(phase)
            + 0.42 * np.sin(2.0 * phase + 0.17 * channel)
            + 0.22 * np.sin(3.0 * phase + 0.31 * channel)
            + 0.11 * np.sin(5.0 * phase + 0.43 * channel)
        )
        formant = (
            0.55
            + 0.45
            * np.sin(2.0 * np.pi * (0.17 + 0.013 * channel) * time + 0.2 * channel) ** 2
        )
        onset = 0.65 + 0.35 * np.exp(-local_time / 0.12)
        sweep = np.sin(sweep_phase * (1.0 + 0.007 * channel) + 0.37 * channel)
        noise = rng.standard_normal(frames)
        noise = np.convolve(noise, np.ones(17) / 17.0, mode="same")
        mixed = (
            0.32 * vocal * formant * onset + 0.20 * sweep * sweep_gate + 0.015 * noise
        )
        output[:, channel] = channel_gains[channel] * mixed
    peak = float(np.max(np.abs(output)))
    if peak > 0.52:
        output *= 0.52 / peak
    return output


def generate_sample_file(
    path: str | os.PathLike[str],
    *,
    seed: int,
    channels: int = 6,
    duration_seconds: float = 12.0,
    bit_depth: int = 24,
) -> dict[str, Any]:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    samples = synthesize_signal(seed, channels, duration_seconds)
    write_wav(destination, samples, bit_depth)
    audio = read_wav(destination)
    params = AnalysisParameters.for_channels(channels)
    reference = reference_measurement(audio, params)
    manifest = {
        "generator": GENERATOR_ID,
        "seed": int(seed),
        "filename": destination.name,
        "sha256": audio.source_sha256,
        "sample_rate": audio.sample_rate,
        "bit_depth": audio.bit_depth,
        "channels": audio.channels,
        "layout": list(audio.layout),
        "frames": audio.frame_count,
        "duration_seconds": audio.frame_count / audio.sample_rate,
        "reference": reference,
        "parameters": params.as_dict(),
    }
    manifest_path = destination.with_suffix(destination.suffix + ".json")
    payload = canonical_json(manifest).encode("utf-8") + b"\n"
    temporary = manifest_path.with_name(manifest_path.name + ".tmp")
    with temporary.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, manifest_path)
    manifest["_manifest_path"] = str(manifest_path)
    return manifest


def reference_measurement(
    audio: AudioBuffer,
    params: AnalysisParameters,
) -> dict[str, Any]:
    """Independent time-domain K-filter reference for generated material."""
    downmix = np.asarray(audio.samples @ np.asarray(params.downmix).T)
    filtered = np.empty_like(downmix)
    for channel in range(downmix.shape[1]):
        stage_one = direct_biquad_reference(downmix[:, channel], K_PRE_B, K_PRE_A)
        filtered[:, channel] = direct_biquad_reference(stage_one, K_RLB_B, K_RLB_A)
    weighted = np.zeros(filtered.shape[0], dtype=np.float64)
    for channel, weight in enumerate(params.weights):
        weighted += weight * np.square(filtered[:, channel])
    cumulative = np.empty(weighted.size + 1, dtype=np.float64)
    cumulative[0] = 0.0
    np.cumsum(weighted, out=cumulative[1:])
    bounds = block_bounds(audio.frame_count, params.block_samples, params.hop_samples)
    momentary_values: list[float | None] = []
    short_values: list[float | None] = []
    for start, end in bounds:
        energy = float((cumulative[end] - cumulative[start]) / (end - start))
        momentary_values.append(lufs_from_energy(energy))
        if end >= params.short_window_samples:
            short_start = end - params.short_window_samples
            short_energy = float(
                (cumulative[end] - cumulative[short_start])
                / params.short_window_samples
            )
            short_values.append(lufs_from_energy(short_energy))
        else:
            short_values.append(None)
    integrated_values = _reference_integrated(
        momentary_values,
        weighted,
        bounds,
        params.absolute_gate_lufs,
        params.relative_gate_lu,
    )
    peak = _reference_true_peak(downmix, bounds)
    momentary_finite = [value for value in momentary_values if value is not None]
    short_finite = [value for value in short_values if value is not None]
    return {
        "block_count": len(bounds),
        "integrated_lufs": integrated_values,
        "momentary_lufs_first": momentary_values[0] if momentary_values else None,
        "momentary_lufs_last": momentary_values[-1] if momentary_values else None,
        "momentary_lufs_max": max(momentary_finite) if momentary_finite else None,
        "short_lufs_max": max(short_finite) if short_finite else None,
        "true_peak_dbtp": peak["dbtp"],
        "true_peak_position_sample": peak["position_sample"],
        "true_peak_channel": peak["channel"],
        "method": {
            "k_weighting": "direct-form II-transposed scalar biquads",
            "true_peak": "8x band-limited FFT interpolation",
            "integrated_gates": [
                params.absolute_gate_lufs,
                params.relative_gate_lu,
            ],
        },
        "expected_agreement": {
            "integrated_lu": 1e-6,
            "true_peak_db": 0.05,
            "note": "4x online/offline estimator against the 8x generator reference",
        },
    }


def _reference_integrated(
    momentary_values: list[float | None],
    weighted: np.ndarray,
    bounds: list[tuple[int, int]],
    absolute_gate_lufs: float,
    relative_gate_lu: float,
) -> float | None:
    energies: list[float] = []
    for loudness, (start, end) in zip(momentary_values, bounds):
        if loudness is not None and loudness >= absolute_gate_lufs:
            energies.append(float((weighted[start:end].sum()) / (end - start)))
    if not energies:
        return None
    relative_gate = (
        lufs_from_energy(sum(energies) / len(energies)) or float("-inf")
    ) + relative_gate_lu
    selected = [
        energy
        for energy in energies
        if (lufs_from_energy(energy) or float("-inf")) > relative_gate
    ]
    if not selected:
        return None
    return lufs_from_energy(sum(selected) / len(selected))


def _reference_true_peak(
    downmixed: np.ndarray,
    bounds: list[tuple[int, int]],
    oversample: int = 8,
) -> dict[str, Any]:
    peak_amplitude = 0.0
    peak_position = 0.0
    peak_channel = 0
    for channel in range(downmixed.shape[1]):
        source = downmixed[:, channel]
        target_length = source.size * oversample
        spectrum = np.fft.rfft(source)
        padded = np.zeros(target_length // 2 + 1, dtype=np.complex128)
        padded[: spectrum.size] = spectrum
        resampled = np.fft.irfft(padded, n=target_length) * oversample
        for start, end in bounds:
            segment = np.abs(resampled[start * oversample : end * oversample])
            if segment.size == 0:
                continue
            index = int(np.argmax(segment))
            amplitude = float(segment[index])
            if amplitude > peak_amplitude:
                peak_amplitude = amplitude
                peak_position = start + index / oversample
                peak_channel = channel
    return {
        "dbtp": (20.0 * math.log10(peak_amplitude) if peak_amplitude > 0.0 else None),
        "position_sample": peak_position,
        "channel": peak_channel,
    }
