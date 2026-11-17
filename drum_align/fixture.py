"""Deterministic synthetic multi-camera drum session generator."""

from __future__ import annotations

import json
import wave
from pathlib import Path

import numpy as np


SAMPLE_RATE = 48_000


def _kick_burst(length: int, strength: float = 1.0) -> np.ndarray:
    t = np.arange(length, dtype=np.float64) / SAMPLE_RATE
    frequency = 82.0 - 175.0 * t
    phase = 2.0 * np.pi * (82.0 * t - 87.5 * t * t)
    body = np.sin(phase) * np.exp(-t / 0.135)
    click = np.exp(-t / 0.0025) * np.sin(2.0 * np.pi * 1_900.0 * t) * 0.20
    noise = np.sin(2.0 * np.pi * 7_100.0 * t) * np.exp(-t / 0.0018) * 0.08
    return (body + click + noise) * strength


def _hat_burst(length: int, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    t = np.arange(length, dtype=np.float64) / SAMPLE_RATE
    noise = rng.standard_normal(length)
    # A first difference makes the deterministic noise bright enough for a
    # hi-hat while leaving the low-frequency kick detector unaffected.
    bright = np.diff(noise, prepend=noise[0])
    return bright * np.exp(-t / 0.018) * 0.035


def _render_source(duration_seconds: float, beat_seconds: float) -> np.ndarray:
    total = int(round(duration_seconds * SAMPLE_RATE)) + SAMPLE_RATE
    source = np.zeros(total, dtype=np.float64)
    beat = int(round(beat_seconds * SAMPLE_RATE))
    kick = _kick_burst(int(0.42 * SAMPLE_RATE))
    bar_beats = 4
    beat_index = 1
    position = int(round(1.0 * SAMPLE_RATE))
    while position + kick.size < source.size:
        source[position : position + kick.size] += kick
        # Add genuine double-kick figures on selected beats without changing
        # the regular downbeat anchor grid.
        if beat_index % 13 in (5, 6) or beat_index % 17 == 8:
            second = position + int(round(0.145 * SAMPLE_RATE))
            if second + kick.size < source.size:
                source[second : second + kick.size] += 0.72 * kick
        position += beat
        beat_index += 1

    hat = _hat_burst(int(0.08 * SAMPLE_RATE), seed=20_260_111)
    position = int(round(1.0 * SAMPLE_RATE + beat_seconds * SAMPLE_RATE / 2.0))
    hat_index = 0
    while position + hat.size < source.size:
        shifted = np.roll(hat, hat_index % 97)
        source[position : position + shifted.size] += shifted
        position += beat
        hat_index += 1

    # A quiet cymbal-like bed exercises the detector without masking kicks.
    t = np.arange(source.size, dtype=np.float64) / SAMPLE_RATE
    source += 0.008 * np.sin(2.0 * np.pi * 235.0 * t)
    source += 0.004 * np.sin(2.0 * np.pi * 3_700.0 * t)
    peak = float(np.max(np.abs(source)))
    if peak > 0:
        source *= 0.68 / peak
    return source


def _camera_track(
    source: np.ndarray,
    *,
    frame_count: int,
    origin_samples: float,
    clock_ratio: float,
    noise_amplitude: float,
    seed: int,
    polarity: float,
) -> np.ndarray:
    source_positions = origin_samples + np.arange(frame_count) * clock_ratio
    valid = (source_positions >= 0) & (source_positions <= source.size - 1)
    track = np.zeros(frame_count, dtype=np.float64)
    track[valid] = np.interp(
        source_positions[valid],
        np.arange(source.size, dtype=np.float64),
        source,
    )
    rng = np.random.default_rng(seed)
    track += rng.standard_normal(frame_count) * noise_amplitude
    track *= polarity
    return np.clip(np.rint(track * 32767.0), -32768, 32767).astype("<i2")


def _write_wav(path: Path, samples: np.ndarray, sample_rate: int) -> None:
    with wave.open(str(path), "wb") as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)
        wav.setframerate(sample_rate)
        wav.setnframes(samples.size)
        wav.writeframes(samples.astype("<i2").tobytes())


def generate_demo(output_dir: str | Path, duration_seconds: float = 26.0) -> Path:
    """Write deterministic WAVs plus project.json and return project path."""
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    if duration_seconds < 6.0:
        raise ValueError("duration must be at least 6 seconds")
    beat_seconds = 0.5
    source = _render_source(duration_seconds + 1.0, beat_seconds)
    reference_frames = int(round(duration_seconds * SAMPLE_RATE))
    auxiliary_frames = reference_frames + SAMPLE_RATE

    camera_specs = {
        "cam_a": {
            "origin": 0.0,
            "ratio": 1.0,
            "noise": 0.0018,
            "seed": 101,
            "polarity": 1.0,
            "gain": 1.0,
        },
        "cam_b": {
            "origin": 0.137 * SAMPLE_RATE,
            "ratio": 1.00042,
            "noise": 0.0022,
            "seed": 202,
            "polarity": 1.0,
            "gain": 0.82,
        },
        "cam_c": {
            "origin": -0.083 * SAMPLE_RATE,
            "ratio": 0.99971,
            "noise": 0.0020,
            "seed": 303,
            "polarity": -1.0,
            "gain": 0.76,
        },
    }
    tracks = []
    for camera, spec in camera_specs.items():
        frames = reference_frames if camera == "cam_a" else auxiliary_frames
        samples = _camera_track(
            source,
            frame_count=frames,
            origin_samples=float(spec["origin"]),
            clock_ratio=float(spec["ratio"]),
            noise_amplitude=float(spec["noise"]),
            seed=int(spec["seed"]),
            polarity=float(spec["polarity"]),
        )
        filename = f"{camera}.wav"
        _write_wav(output / filename, samples, SAMPLE_RATE)
        tracks.append({"camera": camera, "path": filename, "gain": spec["gain"]})

    bar_starts = []
    position = 1.0
    while position < duration_seconds - 0.5:
        bar_starts.append(int(round(position * SAMPLE_RATE)))
        position += 4.0 * beat_seconds

    project = {
        "schema_version": 1,
        "sample_rate": SAMPLE_RATE,
        "base_camera": "cam_a",
        "bar_starts_samples": bar_starts,
        "min_bar_starts": len(bar_starts),
        "tracks": tracks,
        "phase_check": {
            "window_samples": 4_096,
            "pre_roll_samples": 480,
            "threshold": 0.30,
            "attenuation_gain": 0.45,
        },
        "curve_points": min(10, len(bar_starts)),
        "max_offset_seconds": 2.0,
        "render": {"block_samples": 16_384},
        "outputs": {
            "master": "aligned_master.wav",
            "report": "alignment_report.json",
            "snapshot": "alignment_state.json",
        },
    }
    project_path = output / "project.json"
    project_path.write_text(
        json.dumps(project, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return project_path
