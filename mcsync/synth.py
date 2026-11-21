"""Deterministic synthetic multi-channel recordings with known clock drift."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .atomic import atomic_write_json
from .models import MarkerConfig
from .utils import json_ready, sha256_bytes
from .wavio import AudioData, encode_wav_bytes, write_multichannel_wav


@dataclass
class SyntheticConfig:
    channels: int = 8
    sample_rate: int = 48000
    duration_seconds: float = 24.0
    marker_period_seconds: float = 0.5
    marker_frequency_hz: float = 1000.0
    marker_duration_seconds: float = 0.002
    marker_amplitude: float = 0.72
    background_amplitude: float = 0.035
    noise_rms: float = 0.0025
    drift_ppm: tuple[float, ...] = (
        0.0,
        120.0,
        -95.0,
        180.0,
        -150.0,
        60.0,
        -35.0,
        210.0,
    )
    initial_offset_samples: tuple[float, ...] = (
        0.0,
        3.0,
        -2.0,
        7.5,
        -6.0,
        1.5,
        -4.0,
        10.0,
    )
    event_jumps: dict[int, list[dict[str, float]]] = field(default_factory=dict)
    missing_markers: dict[int, list[int]] = field(default_factory=dict)
    low_snr_markers: dict[int, list[int]] = field(default_factory=dict)
    seed: int = 20261122

    def drifted_channels(self) -> int:
        return min(self.channels, len(self.drift_ppm))

    def validate(self) -> None:
        if self.channels <= 0:
            raise ValueError("channels must be positive")
        if self.duration_seconds <= 0:
            raise ValueError("duration must be positive")
        if self.marker_period_seconds <= 0:
            raise ValueError("marker period must be positive")
        if self.drifted_channels() == 0:
            raise ValueError("at least one channel drift value is required")


@dataclass
class SyntheticResult:
    audio: AudioData
    truth: dict[str, Any]
    baseline: dict[str, Any]
    wav_sha256: str

    def write(self, directory: str | Path) -> dict[str, str]:
        output = Path(directory)
        output.mkdir(parents=True, exist_ok=True)
        wav_path = output / "sample_8ch.wav"
        truth_path = output / "known_truth.json"
        baseline_path = output / "baseline_residual.json"
        self.wav_sha256 = write_multichannel_wav(
            wav_path, self.audio.samples, self.audio.sample_rate
        )
        atomic_write_json(truth_path, self.truth)
        atomic_write_json(baseline_path, self.baseline)
        return {
            "wav": str(wav_path),
            "truth": str(truth_path),
            "baseline": str(baseline_path),
        }


def _clock_scale(ppm: float) -> float:
    return 1.0 + ppm * 1.0e-6


def _apply_event_jumps(
    event_samples: np.ndarray,
    event_times: np.ndarray,
    jumps: list[dict[str, float]],
) -> tuple[np.ndarray, list[dict[str, float]]]:
    adjusted = event_samples.copy()
    centers = event_samples.copy()
    applied: list[dict[str, float]] = []
    for jump in jumps:
        time_s = float(jump.get("time_s", jump.get("after_seconds", 0.0)))
        jump_samples = float(jump["jump_samples"])
        mask = event_times >= time_s
        adjusted[mask] += jump_samples
        centers[mask] += jump_samples
        applied.append(
            {
                "time_s": time_s,
                "jump_samples": jump_samples,
                "affected_marker_start": int(np.argmax(mask)) if np.any(mask) else None,
            }
        )
    return adjusted, applied


def _moving_average_1d(values: np.ndarray, width: int) -> np.ndarray:
    if width <= 1:
        return values
    half = width // 2
    padded = np.pad(values, (half, width - half - 1), mode="edge")
    cumulative = np.concatenate(([0.0], np.cumsum(padded)))
    return (cumulative[width:] - cumulative[:-width]) / float(width)


def generate_synthetic_audio(config: SyntheticConfig | None = None) -> SyntheticResult:
    cfg = config or SyntheticConfig()
    cfg.validate()
    rng = np.random.default_rng(cfg.seed)
    channel_count = cfg.channels
    sample_rate = cfg.sample_rate
    frame_count = int(round(cfg.duration_seconds * sample_rate))
    event_times = np.arange(
        cfg.marker_period_seconds * 0.5,
        cfg.duration_seconds,
        cfg.marker_period_seconds,
        dtype=np.float64,
    )
    marker_count = int(event_times.size)
    pulse_samples = max(
        8, int(round(cfg.marker_duration_seconds * sample_rate))
    )
    pulse_half = pulse_samples // 2
    pulse_offsets = np.arange(-pulse_half, pulse_samples - pulse_half)
    pulse_envelope = 0.5 - 0.5 * np.cos(
        2.0 * np.pi * (pulse_offsets + pulse_half) / max(1, pulse_samples - 1)
    )
    pulse_carrier = np.sin(
        2.0 * np.pi * cfg.marker_frequency_hz * pulse_offsets / sample_rate
    )
    pulse = cfg.marker_amplitude * pulse_envelope * pulse_carrier

    common_white = rng.normal(0.0, 1.0, size=frame_count + 2)
    common_background = _moving_average_1d(common_white, 17)[:frame_count]
    common_background = common_background / max(
        float(np.std(common_background)), 1.0e-12
    )
    common_background *= cfg.background_amplitude

    samples = np.zeros((channel_count, frame_count), dtype=np.float64)
    channel_truth: list[dict[str, Any]] = []
    generated_marker_positions: dict[int, np.ndarray] = {}
    for channel in range(channel_count):
        ppm = float(cfg.drift_ppm[channel]) if channel < len(cfg.drift_ppm) else 0.0
        offset = (
            float(cfg.initial_offset_samples[channel])
            if channel < len(cfg.initial_offset_samples)
            else 0.0
        )
        scale = _clock_scale(ppm)
        nominal = np.rint((event_times + offset / sample_rate) * sample_rate * scale)
        adjusted, applied_jumps = _apply_event_jumps(
            nominal,
            event_times,
            cfg.event_jumps.get(channel, []),
        )
        positions = adjusted.astype(np.int64)
        generated_marker_positions[channel] = adjusted.astype(np.float64)

        source_time = np.arange(frame_count, dtype=np.float64) / (
            sample_rate * scale
        ) - offset / sample_rate
        source_index = np.clip(source_time * sample_rate, 0.0, frame_count - 1.0)
        channel_signal = np.interp(
            source_index,
            np.arange(frame_count, dtype=np.float64),
            common_background,
        )
        channel_noise = rng.normal(0.0, cfg.noise_rms, size=frame_count)
        channel_signal += channel_noise

        missing = set(cfg.missing_markers.get(channel, []))
        weak = set(cfg.low_snr_markers.get(channel, []))
        marker_records: list[dict[str, Any]] = []
        for marker_index, (center, position) in enumerate(zip(adjusted, positions)):
            start = int(position) - pulse_half
            end = start + pulse_samples
            if marker_index not in missing and end > 0 and start < frame_count:
                source_start = max(0, -start)
                source_end = pulse_samples - max(0, end - frame_count)
                destination_start = max(0, start)
                destination_end = min(frame_count, end)
                channel_signal[destination_start:destination_end] += pulse[
                    source_start:source_end
                ]
            if marker_index in weak:
                noise_start = max(0, int(position) - pulse_samples * 4)
                noise_end = min(frame_count, int(position) + pulse_samples * 4)
                burst = rng.normal(
                    0.0,
                    cfg.marker_amplitude * 0.5,
                    size=noise_end - noise_start,
                )
                channel_signal[noise_start:noise_end] += burst
            marker_records.append(
                {
                    "marker_index": marker_index,
                    "arrival_sample": float(center),
                    "present": marker_index not in missing,
                    "low_snr": marker_index in weak,
                }
            )
        samples[channel] = np.clip(channel_signal, -1.0, 1.0)
        channel_truth.append(
            {
                "channel": channel,
                "name": f"mic_{channel + 1}",
                "clock_scale": scale,
                "drift_ppm": ppm,
                "initial_offset_samples": offset,
                "event_jumps": applied_jumps,
                "markers": marker_records,
            }
        )

    audio = AudioData(
        samples=samples,
        sample_rate=sample_rate,
        channel_names=tuple(f"mic_{index + 1}" for index in range(channel_count)),
    )
    reference = 0
    reference_positions = generated_marker_positions[reference]
    baseline_channels: list[dict[str, Any]] = []
    for channel in range(channel_count):
        positions = generated_marker_positions[channel]
        finite = min(positions.size, reference_positions.size)
        observed_delay = positions[:finite] - reference_positions[:finite]
        slope = _clock_scale(
            float(cfg.drift_ppm[channel]) if channel < len(cfg.drift_ppm) else 0.0
        ) / _clock_scale(
            float(cfg.drift_ppm[reference])
            if reference < len(cfg.drift_ppm)
            else 0.0
        ) - 1.0
        reference_samples = reference_positions[:finite]
        baseline_slope, baseline_offset = _least_squares_line(
            reference_samples, observed_delay
        )
        prediction = baseline_offset + baseline_slope * reference_samples
        residual = observed_delay - prediction
        baseline_channels.append(
            {
                "channel": channel,
                "known_clock_slope": slope,
                "known_drift_ppm": slope * 1.0e6,
                "marker_delay_rms_samples": float(np.sqrt(np.mean(residual**2))),
                "marker_delay_max_abs_samples": float(
                    np.max(np.abs(residual)) if residual.size else 0.0
                ),
                "baseline_fit_slope": baseline_slope,
                "baseline_fit_offset_samples": baseline_offset,
            }
        )
    truth = {
        "schema_version": "synthetic-truth-v1",
        "seed": cfg.seed,
        "sample_rate": sample_rate,
        "channel_count": channel_count,
        "frame_count": frame_count,
        "duration_seconds": cfg.duration_seconds,
        "marker_period_seconds": cfg.marker_period_seconds,
        "marker_period_samples": cfg.marker_period_seconds * sample_rate,
        "marker_count": marker_count,
        "reference_channel": reference,
        "channels": channel_truth,
        "audio_sha256": audio.content_hash(),
    }
    baseline = {
        "schema_version": "baseline-residual-v1",
        "sample_rate": sample_rate,
        "tolerance_samples": 2.0,
        "channels": baseline_channels,
        "max_channel_rms_samples": max(
            item["marker_delay_rms_samples"] for item in baseline_channels
        ),
    }
    wav_sha = sha256_bytes(encode_wav_bytes(samples, sample_rate))
    return SyntheticResult(
        audio=audio,
        truth=json_ready(truth),
        baseline=json_ready(baseline),
        wav_sha256=wav_sha,
    )


def _least_squares_line(x: np.ndarray, y: np.ndarray) -> tuple[float, float]:
    if x.size == 0:
        return 0.0, 0.0
    if x.size == 1 or float(np.ptp(x)) == 0.0:
        return 0.0, float(np.mean(y))
    design = np.column_stack((np.ones_like(x), x))
    coefficients, _, _, _ = np.linalg.lstsq(design, y, rcond=None)
    return float(coefficients[1]), float(coefficients[0])


def generate_sample_bundle(
    directory: str | Path,
    config: SyntheticConfig | None = None,
) -> tuple[SyntheticResult, dict[str, str]]:
    cfg = config or SyntheticConfig()
    output = Path(directory)
    output.mkdir(parents=True, exist_ok=True)
    result = generate_synthetic_audio(cfg)
    wav_path = output / "sample_8ch.wav"
    write_multichannel_wav(wav_path, result.audio.samples, result.audio.sample_rate)
    truth_path = output / "known_truth.json"
    baseline_path = output / "baseline_residual.json"
    marker_config = MarkerConfig(
        period_seconds=cfg.marker_period_seconds,
        pulse_duration_samples=max(
            8, int(round(cfg.marker_duration_seconds * cfg.sample_rate))
        ),
    )
    truth = dict(result.truth)
    truth["marker_config"] = marker_config.to_dict()
    atomic_write_json(truth_path, truth)
    atomic_write_json(baseline_path, result.baseline)
    return result, {
        "wav": str(wav_path),
        "truth": str(truth_path),
        "baseline": str(baseline_path),
    }

