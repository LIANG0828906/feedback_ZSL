from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Sequence

import numpy as np

from .wavio import write_wav


DEFAULT_REFERENCE_TIMES = (
    0.42,
    0.80,
    1.18,
    1.62,
    2.05,
    2.44,
    2.83,
    3.27,
    3.68,
    4.10,
)


@dataclass(frozen=True)
class DemoConfig:
    seed: int = 241
    sample_rate: int = 48000
    bit_depth: int = 24
    duration_seconds: float = 5.0
    reference_times: tuple[float, ...] = DEFAULT_REFERENCE_TIMES
    board_offset: int = 37
    bangzi_offset: int = -23
    tanggu_offset: int = 61
    ambient_offset: int = 0
    reflection_delays_ms: tuple[float, ...] = (9.5, 18.0)
    reflection_gains_db: tuple[float, ...] = (-11.5, -16.5)
    target_lufs: float = -16.0
    true_peak_db: float = -1.0
    block_size: int = 4096


def _stable_seed(base_seed: int, key: str) -> int:
    payload = f"{base_seed}:{key}".encode("utf-8")
    return int.from_bytes(hashlib.sha256(payload).digest()[:8], "little", signed=False)


def _rng(base_seed: int, key: str) -> np.random.Generator:
    return np.random.Generator(np.random.PCG64(_stable_seed(base_seed, key)))


def _event(
    profile: str,
    sample_rate: int,
    rng: np.random.Generator,
) -> np.ndarray:
    if profile == "board_drum":
        duration = 0.105
    elif profile == "bangzi":
        duration = 0.24
    elif profile == "tanggu":
        duration = 0.82
    else:
        raise ValueError(f"unknown percussion profile {profile!r}")
    count = max(8, int(round(duration * sample_rate)))
    t = np.arange(count, dtype=np.float64) / sample_rate
    click = np.exp(-t / 0.00055)
    if profile == "board_drum":
        body = (
            0.62 * np.sin(2.0 * np.pi * 1880.0 * t)
            + 0.38 * np.sin(2.0 * np.pi * 3270.0 * t + 0.31)
            + 0.20 * np.sin(2.0 * np.pi * 6120.0 * t + 1.07)
        )
        body *= np.exp(-t / 0.028)
        noise = rng.standard_normal(count) * np.exp(-t / 0.0012)
        signal = 0.92 * click + 0.55 * body + 0.20 * noise
    elif profile == "bangzi":
        body = (
            0.70 * np.sin(2.0 * np.pi * 690.0 * t)
            + 0.36 * np.sin(2.0 * np.pi * 1180.0 * t + 0.46)
            + 0.18 * np.sin(2.0 * np.pi * 2070.0 * t + 0.91)
        )
        body *= np.exp(-t / 0.135)
        noise = rng.standard_normal(count) * np.exp(-t / 0.0018)
        signal = 0.80 * click + 0.62 * body + 0.16 * noise
    else:
        body = (
            0.82 * np.sin(2.0 * np.pi * 63.0 * t)
            + 0.46 * np.sin(2.0 * np.pi * 92.0 * t + 0.22)
            + 0.24 * np.sin(2.0 * np.pi * 138.0 * t + 0.67)
        )
        body *= np.exp(-t / 0.48)
        noise = rng.standard_normal(count) * np.exp(-t / 0.0014)
        signal = 0.76 * click + 0.72 * body + 0.18 * noise
    peak = float(np.max(np.abs(signal)))
    return signal / max(peak, 1e-12)


def _add_at(buffer: np.ndarray, signal: np.ndarray, offset: int, gain: float) -> None:
    if offset >= buffer.size:
        return
    start = max(0, offset)
    source_start = start - offset
    length = min(signal.size - source_start, buffer.size - start)
    if length > 0:
        buffer[start : start + length] += gain * signal[source_start : source_start + length]


def synthesize_drum_channel(
    profile: str,
    reference_times: Sequence[float],
    config: DemoConfig,
) -> np.ndarray:
    frames = int(round(config.duration_seconds * config.sample_rate))
    output = np.zeros(frames, dtype=np.float64)
    event_rng = _rng(config.seed, f"{profile}:events")
    base_event = _event(profile, config.sample_rate, event_rng)
    direct = np.zeros_like(output)
    for index, reference in enumerate(reference_times):
        position = int(round(reference * config.sample_rate))
        variation = 0.86 + 0.14 * float(event_rng.random())
        _add_at(direct, base_event, position, variation)

    output[:] = direct
    if profile in ("board_drum", "bangzi"):
        for delay_ms, gain_db in zip(
            config.reflection_delays_ms, config.reflection_gains_db
        ):
            delay = int(round(delay_ms * config.sample_rate / 1000.0))
            gain = 10.0 ** (gain_db / 20.0)
            shifted = np.zeros_like(direct)
            if delay < direct.size:
                shifted[delay:] = direct[: direct.size - delay]
            output += gain * shifted

    peak = float(np.max(np.abs(output)))
    target_peak = {"board_drum": 0.82, "bangzi": 0.68, "tanggu": 0.74}[profile]
    if peak > 0.0:
        output *= target_peak / peak
    return output


def synthesize_ambient(config: DemoConfig) -> np.ndarray:
    frames = int(round(config.duration_seconds * config.sample_rate))
    rng = _rng(config.seed, "ambient")
    white = rng.standard_normal(frames)
    kernel = np.exp(-np.arange(96, dtype=np.float64) / 22.0)
    kernel /= np.sum(kernel)
    bed = np.convolve(white, kernel, mode="same")
    t = np.arange(frames, dtype=np.float64) / config.sample_rate
    hum = 0.12 * np.sin(2.0 * np.pi * 50.0 * t + 0.37)
    drift = 0.10 * np.sin(2.0 * np.pi * 0.17 * t + 1.11)
    ambient = bed + hum + drift
    ambient -= float(np.mean(ambient))
    rms = float(np.sqrt(np.mean(ambient * ambient)))
    target_rms = 0.0042
    if rms > 0.0:
        ambient *= target_rms / rms
    return ambient


def shift_signal(signal: np.ndarray, offset_samples: int) -> np.ndarray:
    values = np.asarray(signal)
    shifted = np.zeros_like(values)
    count = int(values.shape[0])
    offset = int(offset_samples)
    if offset >= count or offset <= -count:
        return shifted
    if offset > 0:
        shifted[offset:] = values[: count - offset]
    elif offset < 0:
        shifted[: count + offset] = values[-offset:]
    else:
        shifted[:] = values
    return shifted


def _manifest_dict(config: DemoConfig) -> dict[str, Any]:
    references = [float(value) for value in config.reference_times]
    return {
        "schema_version": 1,
        "sample_rate": config.sample_rate,
        "bit_depth": config.bit_depth,
        "seed": config.seed,
        "synthesis": {
            "duration_seconds": config.duration_seconds,
            "generator": "drum_bus.synth/v1",
        },
        "reflection": {
            "delays_ms": [float(value) for value in config.reflection_delays_ms],
            "gains_db": [float(value) for value in config.reflection_gains_db],
        },
        "channels": [
            {
                "channel": "board_drum",
                "instrument": "板鼓",
                "path": "board_drum.wav",
                "synth_profile": "board_drum",
                "start_offset_samples": config.board_offset,
                "reference_times": references,
                "reflection_window": {"start_ms": 5.0, "end_ms": 40.0},
                "early_reflections": True,
                "mix_weight": 0.92,
                "pan": -0.32,
            },
            {
                "channel": "bangzi",
                "instrument": "梆子",
                "path": "bangzi.wav",
                "synth_profile": "bangzi",
                "start_offset_samples": config.bangzi_offset,
                "reference_times": references,
                "reflection_window": {"start_ms": 6.0, "end_ms": 44.0},
                "early_reflections": True,
                "mix_weight": 0.68,
                "pan": 0.27,
            },
            {
                "channel": "tanggu",
                "instrument": "堂鼓",
                "path": "tanggu.wav",
                "synth_profile": "tanggu",
                "start_offset_samples": config.tanggu_offset,
                "reference_times": references,
                "reflection_window": {"start_ms": 6.0, "end_ms": 46.0},
                "early_reflections": False,
                "mix_weight": 0.78,
                "pan": 0.04,
            },
        ],
        "ambient": {
            "channel": "ambient",
            "instrument": "环境底噪",
            "path": "ambient.wav",
            "synth_profile": "ambient",
            "start_offset_samples": config.ambient_offset,
            "reference_times": [],
            "reflection_window": {"start_ms": 0.0, "end_ms": 1.0},
            "early_reflections": False,
            "mix_weight": 0.60,
            "pan": 0.0,
        },
        "master": {
            "target_lufs": config.target_lufs,
            "true_peak_db": config.true_peak_db,
        },
        "render": {"block_size": config.block_size},
    }


def generate_demo_project(
    output_dir: str | Path,
    config: DemoConfig | None = None,
) -> Path:
    resolved_config = config or DemoConfig()
    if not resolved_config.reference_times:
        raise ValueError("at least one reference time is required")
    if any(
        resolved_config.reference_times[index] >= resolved_config.reference_times[index + 1]
        for index in range(len(resolved_config.reference_times) - 1)
    ):
        raise ValueError("reference times must be strictly ascending")

    root = Path(output_dir).resolve()
    root.mkdir(parents=True, exist_ok=True)
    profiles = (
        ("board_drum", resolved_config.board_offset),
        ("bangzi", resolved_config.bangzi_offset),
        ("tanggu", resolved_config.tanggu_offset),
    )
    for profile, offset in profiles:
        signal = synthesize_drum_channel(
            profile, resolved_config.reference_times, resolved_config
        )
        signal = shift_signal(signal, offset)
        write_wav(
            root / f"{profile}.wav",
            signal,
            resolved_config.sample_rate,
            resolved_config.bit_depth,
        )
    ambient = shift_signal(synthesize_ambient(resolved_config), resolved_config.ambient_offset)
    write_wav(
        root / "ambient.wav",
        ambient,
        resolved_config.sample_rate,
        resolved_config.bit_depth,
    )

    manifest_path = root / "manifest.json"
    manifest_path.write_text(
        json.dumps(_manifest_dict(resolved_config), ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return manifest_path


def reflection_parameters(config: DemoConfig) -> dict[str, Any]:
    return {
        "delays_samples": [
            int(round(value * config.sample_rate / 1000.0))
            for value in config.reflection_delays_ms
        ],
        "gains_linear": [
            float(10.0 ** (value / 20.0)) for value in config.reflection_gains_db
        ],
    }
