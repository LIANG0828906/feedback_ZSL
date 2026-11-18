from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path

import numpy as np

from .manifest import ProjectSpec, pitch_to_frequency
from .wavio import write_wav


def _rng(seed: int, *labels: object) -> np.random.Generator:
    payload = json.dumps([seed, *labels], separators=(",", ":"), sort_keys=True)
    derived = int.from_bytes(
        hashlib.sha256(payload.encode("utf-8")).digest()[:16], "little"
    )
    return np.random.Generator(np.random.PCG64(derived))


def _event_wave(
    sample_rate: int,
    frequency: float,
    kind: str,
    rng: np.random.Generator,
    duration: float,
) -> np.ndarray:
    count = max(1, int(round(duration * sample_rate)))
    time = np.arange(count, dtype=np.float64) / sample_rate
    if kind == "harmonic":
        partials = (2.0,)
        amplitudes = (0.72,)
        decay = 1.5
    elif kind == "stopped":
        partials = (1.0, 2.01, 3.03, 4.08, 5.16, 6.28)
        amplitudes = tuple(0.82 / (index**1.55) for index in range(1, 7))
        decay = 1.05
    else:
        partials = (1.0, 2.0, 3.0, 4.01, 5.03, 6.08, 7.15, 8.24)
        amplitudes = tuple(0.90 / (index**1.35) for index in range(1, 9))
        decay = 1.65

    phases = rng.uniform(0.0, 2.0 * math.pi, size=len(partials))
    wave = np.zeros(count, dtype=np.float64)
    for ratio, amplitude, phase in zip(partials, amplitudes, phases):
        partial_frequency = frequency * ratio
        if partial_frequency >= sample_rate * 0.45:
            continue
        envelope = np.exp(-time / (decay / (ratio**0.55)))
        wave += amplitude * envelope * np.sin(
            2.0 * math.pi * partial_frequency * time + phase
        )
    attack_count = max(2, int(round(0.004 * sample_rate)))
    wave[:attack_count] *= np.linspace(0.0, 1.0, attack_count, endpoint=False)
    wave /= max(1.0, np.max(np.abs(wave)))
    return wave


def _add_pluck(
    destination: np.ndarray,
    start: int,
    frequency: float,
    kind: str,
    rng: np.random.Generator,
    sample_rate: int,
    duration: float,
) -> None:
    if start >= destination.size:
        return
    wave = _event_wave(sample_rate, frequency, kind, rng, duration)
    body_gain = 0.145 if kind != "harmonic" else 0.115
    wave *= body_gain
    stop = min(destination.size, start + wave.size)
    destination[start:stop] += wave[: stop - start]

    click_count = min(
        destination.size - start, max(8, int(round(0.006 * sample_rate)))
    )
    if click_count <= 0:
        return
    click_time = np.arange(click_count, dtype=np.float64) / sample_rate
    click_noise = rng.standard_normal(click_count)
    click_envelope = np.exp(-click_time / 0.0011)
    click = 0.20 * click_noise * click_envelope
    click[0] = 0.42 + click_noise[0]
    destination[start : start + click_count] += click


def _activity_kind(channel_index: int, event_index: int) -> str:
    if channel_index % 5 == 4 and event_index % 3 == 1:
        return "harmonic"
    if (channel_index + event_index) % 3 == 1:
        return "stopped"
    if (channel_index + event_index) % 4 == 2:
        return "harmonic"
    return "open"


def _shift_with_zeros(samples: np.ndarray, offset: int) -> np.ndarray:
    if offset == 0:
        return samples.copy()
    shifted = np.zeros_like(samples)
    if offset > 0:
        shifted[offset:] = samples[:-offset]
    else:
        shifted[:offset] = samples[-offset:]
    return shifted


def _resonance_leak(
    sample_rate: int,
    frequency: float,
    start: int,
    stop: int,
    gain: float,
    seed: int,
    source_name: str,
    target_name: str,
) -> np.ndarray:
    count = stop - start
    result = np.zeros(count, dtype=np.float64)
    if count <= 0:
        return result
    rng = _rng(seed, "leak", source_name, target_name, start, stop)
    time = np.arange(count, dtype=np.float64) / sample_rate
    phase = rng.uniform(0.0, 2.0 * math.pi)
    for harmonic, relative in ((1.0, 0.22), (2.0, 0.055)):
        if frequency * harmonic < sample_rate * 0.45:
            result += relative * np.sin(
                2.0 * math.pi * frequency * harmonic * time + phase * harmonic
            )
    edge = min(int(round(0.002 * sample_rate)), count // 2)
    if edge > 0:
        ramp = np.linspace(0.0, 1.0, edge, endpoint=False)
        result[:edge] *= ramp
        result[-edge:] *= ramp[::-1]
    return gain * result


def synthesize_project(project: ProjectSpec, output_directory: Path) -> dict[str, str]:
    output_directory.mkdir(parents=True, exist_ok=True)
    total = project.total_samples
    base_channels: dict[str, np.ndarray] = {}
    channel_map = project.channel_map

    for channel_index, channel in enumerate(project.channels):
        rng = _rng(project.seed, "channel", channel.name, channel.string_number)
        clean = np.zeros(total, dtype=np.float64)
        for event_index, reference_time in enumerate(project.reference_times):
            start = int(round(reference_time * project.sample_rate))
            kind = (
                "harmonic"
                if channel.reference
                else _activity_kind(channel_index, event_index)
            )
            _add_pluck(
                clean,
                start,
                channel.frequency_hz,
                kind,
                rng,
                project.sample_rate,
                2.15,
            )
        clean += rng.standard_normal(total) * 0.00018
        base_channels[channel.name] = _shift_with_zeros(
            clean, channel.start_offset_samples
        )

    rendered: dict[str, np.ndarray] = {
        name: samples.copy() for name, samples in base_channels.items()
    }
    for leak in project.crosstalk:
        start = int(round(leak.start_seconds * project.sample_rate))
        stop = int(round(leak.end_seconds * project.sample_rate))
        stop = min(stop, total)
        source_frequency = channel_map[leak.source].frequency_hz
        rendered[leak.target][start:stop] += _resonance_leak(
            project.sample_rate,
            source_frequency,
            start,
            stop,
            leak.gain,
            project.seed,
            leak.source,
            leak.target,
        )

    hashes: dict[str, str] = {}
    for channel in project.channels:
        destination = output_directory / channel.path
        write_wav(
            destination,
            rendered[channel.name],
            project.sample_rate,
            project.bit_depth,
        )
        hashes[channel.name] = hashlib.sha256(destination.read_bytes()).hexdigest()
    return hashes


def demo_manifest(seed: int = 20261118, duration: float = 10.0) -> dict:
    names = ["ch1_C2", "ch2_D2", "ch3_E2", "ch4_G2", "ch5_A2", "ch6_C3_ref"]
    pitches = ["C2", "D2", "E2", "G2", "A2", "C3"]
    strings = [1, 2, 3, 4, 5, 6]
    pans = [-0.78, -0.48, -0.18, 0.20, 0.49, 0.76]
    offsets = [0, 960, 1680, 480, 2400, 1200]
    references = [0.90, 2.10, 3.30, 4.70, 6.00, 7.20, 8.50]
    channels = []
    for index, (name, pitch, string_number, pan, offset) in enumerate(
        zip(names, pitches, strings, pans, offsets)
    ):
        channels.append(
            {
                "name": name,
                "path": f"audio/{name}.wav",
                "string": string_number,
                "pitch": pitch,
                "pan": pan,
                "start_offset_samples": offset,
                "reference": index == 5,
            }
        )
    segment_gains = [
        {
            "name": "intro",
            "start_seconds": 0.0,
            "end_seconds": 3.4,
            "gains": {
                names[0]: 0.78,
                names[1]: 0.80,
                names[2]: 0.74,
                names[3]: 0.84,
                names[4]: 0.72,
                names[5]: 0.64,
            },
        },
        {
            "name": "development",
            "start_seconds": 3.4,
            "end_seconds": 6.7,
            "gains": {
                names[0]: 0.88,
                names[1]: 0.86,
                names[2]: 0.82,
                names[3]: 0.93,
                names[4]: 0.82,
                names[5]: 0.75,
            },
        },
        {
            "name": "coda",
            "start_seconds": 6.7,
            "end_seconds": duration,
            "gains": {
                names[0]: 0.72,
                names[1]: 0.75,
                names[2]: 0.80,
                names[3]: 0.86,
                names[4]: 0.78,
                names[5]: 0.70,
            },
        },
    ]
    return {
        "schema_version": 1,
        "project_id": "deterministic_guqin_six_channel",
        "seed": seed,
        "sample_rate": 48000,
        "bit_depth": 24,
        "duration_seconds": duration,
        "reference_times_seconds": references,
        "target_loudness_lufs": -16.0,
        "true_peak_limit_dbtp": -1.0,
        "block_size": 4096,
        "crossfade_seconds": 0.06,
        "channels": channels,
        "crosstalk": [
            {
                "source": names[0],
                "target": names[1],
                "window_seconds": [0.86, 1.66],
                "gain": 0.135,
            },
            {
                "source": names[1],
                "target": names[2],
                "window_seconds": [2.06, 2.72],
                "gain": 0.125,
            },
            {
                "source": names[2],
                "target": names[3],
                "window_seconds": [4.66, 5.34],
                "gain": 0.120,
            },
        ],
        "decoupling": {
            "residual_threshold_db": -50.0,
            "minimum_attenuation_db": 18.0,
            "detection_margin_db": 12.0,
            "max_unrelated_peak_loss_db": 1.0,
        },
        "segments": segment_gains,
    }


def write_demo_project(directory: Path, seed: int = 20261118) -> Path:
    from .manifest import load_manifest

    directory.mkdir(parents=True, exist_ok=True)
    manifest_path = directory / "manifest.json"
    with manifest_path.open("w", encoding="utf-8", newline="\n") as handle:
        handle.write(
            json.dumps(demo_manifest(seed=seed), indent=2, ensure_ascii=False) + "\n"
        )
    project = load_manifest(manifest_path)
    synthesize_project(project, directory)
    return manifest_path
