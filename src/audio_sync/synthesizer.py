"""Deterministic local multichannel drift sample generator."""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from .wavio import write_json_atomic, write_wav_i16


def _smooth_noise(rng: np.random.Generator, length: int, width: int) -> np.ndarray:
    raw = rng.standard_normal(length + width)
    kernel = np.hanning(width + 2)[1:-1]
    kernel = kernel / max(kernel.sum(), 1e-12)
    filtered = np.convolve(raw, kernel, mode="valid")
    filtered = filtered[:length]
    filtered -= np.mean(filtered)
    scale = np.std(filtered)
    return filtered / max(scale, 1e-12)


def _marker_pulse(sample_rate: int, pulse_samples: int, frequency: float) -> np.ndarray:
    if pulse_samples <= 0:
        return np.empty(0, dtype=np.float64)
    time = np.arange(pulse_samples, dtype=np.float64) / sample_rate
    envelope = np.hanning(pulse_samples)
    return envelope * np.sin(2.0 * np.pi * frequency * time)


def _actual_marker(
    nominal: int,
    offset: float,
    ppm: float,
    step_at: int | None,
    step_samples: float,
) -> float:
    position = (1.0 + ppm * 1e-6) * nominal + offset
    if step_at is not None and nominal >= step_at:
        position += step_samples
    return position


def synthesize_dataset(
    output_dir: str | Path,
    duration_seconds: float = 30.0,
    channels: int = 8,
    sample_rate: int = 48_000,
    marker_interval_seconds: float = 0.5,
    seed: int = 20261122,
    noise_rms: float = 0.0035,
    common_rms: float = 0.075,
    drift_ppm: list[float] | None = None,
    initial_offsets: list[float] | None = None,
    missing_markers: dict[int, list[int]] | None = None,
    step_samples: dict[int, float] | None = None,
) -> dict:
    """Create a reference WAV and a marker description with known drift truth."""

    if channels < 2:
        raise ValueError("at least two channels are required")
    if duration_seconds <= 0 or marker_interval_seconds <= 0:
        raise ValueError("duration and marker interval must be positive")

    root = Path(output_dir).expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(seed)
    frames = int(round(duration_seconds * sample_rate))
    if frames < sample_rate // 2:
        raise ValueError("duration is too short for cross-correlation")

    if drift_ppm is None:
        base = [0.0, 82.0, -61.0, 118.0, -93.0, 47.0, -132.0, 69.0]
        drift_ppm = [base[index % len(base)] for index in range(channels)]
    if len(drift_ppm) != channels:
        raise ValueError("drift_ppm must contain one value per channel")
    if initial_offsets is None:
        base_offsets = [0.0, 17.0, -11.0, 29.0, -23.0, 7.0, -31.0, 19.0]
        initial_offsets = [base_offsets[index % len(base_offsets)] for index in range(channels)]
    if len(initial_offsets) != channels:
        raise ValueError("initial_offsets must contain one value per channel")

    missing_markers = missing_markers or {}
    step_samples = step_samples or {}
    first_marker = int(round(0.4 * sample_rate))
    period = int(round(marker_interval_seconds * sample_rate))
    marker_count = max(1, int(np.floor((frames - 1 - first_marker) / period)) + 1)
    expected = [first_marker + index * period for index in range(marker_count)]
    pulse_width = max(16, int(round(0.004 * sample_rate)))
    pulse = _marker_pulse(sample_rate, pulse_width, 1800.0)

    time_axis = np.arange(frames, dtype=np.float64)
    coarse = _smooth_noise(rng, frames, max(8, sample_rate // 6000))
    fine = _smooth_noise(rng, frames, max(2, sample_rate // 24000))
    source = common_rms * (0.72 * coarse + 0.28 * fine)
    source += 0.018 * np.sin(2.0 * np.pi * 193.0 * time_axis / sample_rate)
    source += 0.010 * np.sin(2.0 * np.pi * 431.0 * time_axis / sample_rate + 0.7)

    output = np.zeros((channels, frames), dtype=np.float64)
    actual_positions: dict[str, list[float]] = {}
    step_at_nominal = first_marker + (marker_count // 2) * period
    marker_step_at = step_at_nominal
    sample_step_at = frames // 2

    for channel in range(channels):
        ppm = float(drift_ppm[channel])
        offset = float(initial_offsets[channel])
        channel_step = float(step_samples.get(channel, 0.0))
        real_time = (time_axis - offset) / (1.0 + ppm * 1e-6)
        if channel_step:
            real_time = np.where(
                time_axis >= sample_step_at,
                (time_axis - offset - channel_step) / (1.0 + ppm * 1e-6),
                real_time,
            )
        signal = np.interp(real_time, time_axis, source)
        signal += noise_rms * _smooth_noise(rng, frames, max(3, sample_rate // 16000))
        if channel == 0:
            signal = np.interp(real_time, time_axis, source)
            signal += 0.35 * noise_rms * _smooth_noise(rng, frames, 8)

        missing = set(missing_markers.get(channel, []))
        positions: list[float] = []
        for marker_index, nominal in enumerate(expected):
            if marker_index in missing:
                positions.append(None)  # type: ignore[arg-type]
                continue
            position = _actual_marker(
                nominal,
                offset,
                ppm,
                marker_step_at if channel_step else None,
                channel_step,
            )
            positions.append(position)
            center = int(round(position))
            start = center - pulse_width // 2
            stop = start + pulse_width
            source_start = max(0, -start)
            source_stop = pulse_width - max(0, stop - frames)
            if source_start < source_stop and start + source_start < frames:
                target_start = max(0, start)
                target_stop = min(frames, stop)
                count = target_stop - target_start
                pulse_slice = pulse[source_start : source_start + count]
                gain = 0.78 * (1.0 + 0.02 * ((channel * 7 + marker_index) % 5 - 2))
                signal[target_start:target_stop] += gain * pulse_slice
        actual_positions[str(channel)] = positions
        output[channel] = signal

    peak = float(np.max(np.abs(output)))
    scale = min(1.0, 0.98 / max(peak, 1e-12))
    pcm = np.rint(output * scale * 32767.0).astype(np.int16)
    wav_path = root / "drift_8ch.wav"
    markers_path = root / "markers.json"
    write_wav_i16(wav_path, pcm, sample_rate)

    description = {
        "schema_version": "1.0",
        "sample_rate": sample_rate,
        "channel_count": channels,
        "marker": {
            "first_marker_sample": first_marker,
            "period_samples": period,
            "marker_count": marker_count,
            "pulse_width_samples": pulse_width,
            "search_radius_samples": max(pulse_width, int(period * 0.42)),
            "min_confidence": 0.3,
            "template": [round(float(value), 8) for value in pulse],
        },
        "marker_samples": expected,
        "ground_truth": {
            "duration_seconds": frames / sample_rate,
            "seed": seed,
            "drift_ppm": [float(value) for value in drift_ppm],
            "initial_offsets_samples": [float(value) for value in initial_offsets],
            "missing_markers": {str(key): list(value) for key, value in missing_markers.items()},
            "step_samples": {str(key): float(value) for key, value in step_samples.items()},
            "actual_marker_positions": actual_positions,
        },
    }
    write_json_atomic(markers_path, description)
    manifest = {
        "wav": str(wav_path),
        "markers": str(markers_path),
        "sample_rate": sample_rate,
        "channels": channels,
        "frames": frames,
        "duration_seconds": frames / sample_rate,
        "marker_count": marker_count,
        "seed": seed,
    }
    write_json_atomic(root / "synthetic_manifest.json", manifest)
    return manifest


def load_json(path: str | Path) -> dict:
    return json.loads(Path(path).read_text(encoding="utf-8"))
