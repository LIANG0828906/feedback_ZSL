from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .wavio import write_json_atomic, write_wav_atomic


@dataclass(frozen=True)
class _TrackPlan:
    name: str
    sample_rate: int
    bits_per_sample: int
    channels: int
    length_fraction: float
    drift_ppm: float | None
    kind: str
    target_peak: float


_TRACK_PLANS = (
    _TrackPlan("vocal", 48000, 24, 2, 1.0, None, "vocal", 0.68),
    _TrackPlan("drums", 48000, 24, 1, 1.0, 300.0, "drums", 0.82),
    _TrackPlan("bass", 44100, 16, 1, 1.0, -300.0, "bass", 0.72),
    _TrackPlan("guitar", 44100, 16, 1, 1.0 / 3.0, None, "guitar", 0.58),
    _TrackPlan("keys", 48000, 16, 1, 1.0 / 3.0, None, "keys", 0.52),
    _TrackPlan("perc", 96000, 16, 1, 0.278, None, "perc", 0.43),
    _TrackPlan("strings", 96000, 16, 1, 0.25, None, "strings", 0.37),
    _TrackPlan("brass", 44100, 16, 1, 0.37, None, "brass", 0.44),
    _TrackPlan("pad", 48000, 16, 1, 0.444, None, "pad", 0.33),
    _TrackPlan("fx", 96000, 16, 1, 0.20, None, "fx", 0.28),
    _TrackPlan("room", 48000, 16, 1, 0.25, None, "room", 0.24),
)


def _fade_window(length: int, fade_samples: int) -> np.ndarray:
    window = np.ones(max(0, length), dtype=np.float64)
    fade = min(max(1, fade_samples), length // 2)
    if fade <= 0:
        return window
    ramp = 0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, fade, dtype=np.float64))
    window[:fade] = ramp
    window[-fade:] = ramp[::-1]
    return window


def _add_burst(
    destination: np.ndarray,
    start: int,
    sample_rate: int,
    amplitude: float,
    frequency: float,
    seconds: float = 0.012,
    channel_phase: tuple[float, ...] = (0.0,),
) -> None:
    if start < 0 or start >= destination.shape[0]:
        return
    length = max(1, int(round(seconds * sample_rate)))
    length = min(length, destination.shape[0] - start)
    time = np.arange(length, dtype=np.float64) / sample_rate
    envelope = _fade_window(length, max(1, int(round(0.002 * sample_rate))))
    if destination.ndim == 1:
        destination[start : start + length] += (
            amplitude * envelope * np.sin(2.0 * np.pi * frequency * time)
        )
        return
    for channel in range(destination.shape[1]):
        phase = channel_phase[channel % len(channel_phase)]
        destination[start : start + length, channel] += (
            amplitude
            * envelope
            * np.sin(2.0 * np.pi * frequency * time + phase)
        )


def _add_impulse(
    destination: np.ndarray,
    start: int,
    sample_rate: int,
    amplitude: float,
    frequency: float,
    decay_seconds: float,
) -> None:
    if start < 0 or start >= destination.shape[0]:
        return
    length = max(1, int(round(decay_seconds * 3.0 * sample_rate)))
    length = min(length, destination.shape[0] - start)
    time = np.arange(length, dtype=np.float64) / sample_rate
    envelope = np.exp(-time / max(decay_seconds, 1.0 / sample_rate))
    wave = np.sin(2.0 * np.pi * frequency * time + 0.15)
    if destination.ndim == 1:
        destination[start : start + length] += amplitude * envelope * wave
        return
    for channel in range(destination.shape[1]):
        destination[start : start + length, channel] += amplitude * envelope * wave


def _music_bed(
    time: np.ndarray,
    sample_rate: int,
    kind: str,
    rng: np.random.Generator,
) -> np.ndarray:
    active = np.clip((time - 0.62) / 0.20, 0.0, 1.0)
    slow = 0.72 + 0.28 * np.sin(2.0 * np.pi * 0.071 * time + 0.2)
    tremolo = 0.68 + 0.32 * np.sin(2.0 * np.pi * 1.7 * time) ** 2

    if kind == "vocal":
        vibrato = 1.0 + 0.004 * np.sin(2.0 * np.pi * 5.1 * time)
        base = 196.0 * vibrato
        return (
            0.46 * np.sin(2.0 * np.pi * base * time)
            + 0.20 * np.sin(2.0 * np.pi * 2.0 * base * time + 0.3)
            + 0.11 * np.sin(2.0 * np.pi * 3.0 * base * time + 1.1)
        ) * active * tremolo
    if kind == "drums":
        return 0.05 * rng.standard_normal(time.size) * active * (0.4 + 0.6 * tremolo)
    if kind == "bass":
        note = 55.0 * (1.0 + 0.02 * np.sin(2.0 * np.pi * 0.09 * time))
        return (
            0.78 * np.sin(2.0 * np.pi * note * time)
            + 0.16 * np.sin(2.0 * np.pi * 2.0 * note * time)
        ) * active * slow
    if kind == "guitar":
        notes = (146.83, 185.00, 220.00, 293.66)
        out = np.zeros_like(time)
        for index, note in enumerate(notes):
            out += (0.19 / (index + 1)) * np.sin(
                2.0 * np.pi * note * time + 0.31 * index
            )
        return out * active * (0.55 + 0.45 * tremolo)
    if kind == "keys":
        notes = (261.63, 329.63, 392.00)
        out = sum(
            0.17 * np.sin(2.0 * np.pi * note * time + 0.17 * index)
            for index, note in enumerate(notes)
        )
        return out * active * slow
    if kind == "perc":
        return 0.02 * rng.standard_normal(time.size) * active
    if kind == "strings":
        notes = (130.81, 164.81, 196.00, 261.63)
        out = sum(
            0.12 * np.sin(2.0 * np.pi * note * time + 0.23 * index)
            for index, note in enumerate(notes)
        )
        return out * active * (0.6 + 0.4 * slow)
    if kind == "brass":
        note = 174.61
        return (
            0.26 * np.sin(2.0 * np.pi * note * time)
            + 0.12 * np.sin(2.0 * np.pi * 2.0 * note * time)
            + 0.07 * np.sin(2.0 * np.pi * 3.0 * note * time)
        ) * active * tremolo
    if kind == "pad":
        return (
            0.22 * np.sin(2.0 * np.pi * 98.0 * time)
            + 0.12 * np.sin(2.0 * np.pi * 147.0 * time + 0.7)
        ) * active * slow
    if kind == "fx":
        phase = 2.0 * np.pi * (240.0 * time + 5.0 * time * time)
        return 0.35 * np.sin(phase) * active * (0.3 + 0.7 * tremolo)
    if kind == "room":
        colored = rng.standard_normal(time.size)
        colored = np.convolve(colored, np.ones(16) / 16.0, mode="same")
        return 0.28 * colored * active
    raise ValueError(f"unknown generator kind {kind}")


def _add_rhythm_events(
    destination: np.ndarray,
    sample_rate: int,
    duration_seconds: float,
    kind: str,
) -> None:
    beat = 0.5
    first = 0.75
    beat_index = 0
    while first + beat_index * beat < duration_seconds:
        start = int(round((first + beat_index * beat) * sample_rate))
        if kind == "drums":
            _add_impulse(destination, start, sample_rate, 0.45, 62.0, 0.035)
            if beat_index % 2 == 1:
                _add_burst(
                    destination,
                    start,
                    sample_rate,
                    0.22,
                    1900.0,
                    seconds=0.045,
                    channel_phase=(0.0, 0.6),
                )
            hat = int(round((first + beat_index * beat + beat / 2.0) * sample_rate))
            _add_burst(destination, hat, sample_rate, 0.08, 6500.0, seconds=0.018)
        elif kind == "perc":
            _add_burst(
                destination,
                start,
                sample_rate,
                0.20,
                4200.0 if beat_index % 2 else 2600.0,
                seconds=0.05,
            )
        beat_index += 1


def _anchor_positions(
    duration: float,
    output_rate: int,
    source_rate: int,
    drift_ppm: float | None,
) -> tuple[list[dict[str, int]], list[int]]:
    if drift_ppm is None:
        return [], []
    fractions = (0.12, 0.36, 0.64, 0.88)
    anchors: list[dict[str, int]] = []
    output_samples: list[int] = []
    for fraction in fractions:
        output_sample = int(round(fraction * duration * output_rate))
        source_sample = int(
            round(fraction * duration * source_rate * (1.0 + drift_ppm * 1e-6))
        )
        anchors.append(
            {"output_sample": output_sample, "source_sample": source_sample}
        )
        output_samples.append(output_sample)
    return anchors, output_samples


def _automation(duration: float) -> list[dict[str, float]]:
    base = (
        (0.0, -3.0),
        (20.0, -3.0),
        (20.005, 3.0),
        (60.0, 3.0),
        (60.004, -6.0),
        (120.0, -6.0),
        (120.006, 0.0),
        (180.0, 0.0),
    )
    scale = duration / 180.0
    return [{"time": time * scale, "gain_db": gain} for time, gain in base]


def _render_track(
    plan: _TrackPlan,
    duration: float,
    seed: int,
    track_index: int,
    output_rate: int,
    reference_output_sample: int,
    anchors: list[dict[str, int]],
) -> np.ndarray:
    source_seconds = max(duration * plan.length_fraction, 0.8)
    if plan.drift_ppm is not None:
        source_seconds = max(source_seconds, duration * (1.0 + abs(plan.drift_ppm) * 1e-6))
    source_frames = max(1, int(round(source_seconds * plan.sample_rate)))
    time = np.arange(source_frames, dtype=np.float64) / float(plan.sample_rate)
    rng = np.random.default_rng(seed + 1009 * (track_index + 1))

    mono = _music_bed(time, plan.sample_rate, plan.kind, rng)
    if plan.kind in ("drums", "perc", "bass"):
        rhythm = np.zeros_like(mono)
        _add_rhythm_events(rhythm, plan.sample_rate, source_seconds, plan.kind)
        mono += rhythm

    if plan.kind == "bass":
        note_index = np.floor((time - 0.62) / 0.5).astype(np.int64)
        note_gate = (time >= 0.62) * (0.55 + 0.45 * (np.mod(note_index, 4) != 3))
        mono *= note_gate

    if plan.channels == 1:
        samples = mono[:, None]
    else:
        delay = max(1, int(round(0.0007 * plan.sample_rate)))
        right = np.roll(mono, delay) * 0.93
        right[:delay] = 0.0
        samples = np.column_stack((mono, right))

    pulse_start = int(
        round(reference_output_sample / float(output_rate) * plan.sample_rate)
    )
    _add_burst(
        samples,
        pulse_start,
        plan.sample_rate,
        0.34,
        1000.0,
        seconds=0.045,
        channel_phase=(0.0, 0.45),
    )

    if plan.kind == "vocal":
        for index, anchor in enumerate(anchors):
            _add_burst(
                samples,
                anchor["output_sample"],
                plan.sample_rate,
                0.17,
                3100.0 + 137.0 * index,
                seconds=0.010,
                channel_phase=(0.0, 0.8),
            )
    elif anchors:
        for index, anchor in enumerate(anchors):
            _add_burst(
                samples,
                anchor["source_sample"],
                plan.sample_rate,
                0.17,
                3100.0 + 137.0 * index,
                seconds=0.010,
                channel_phase=(0.0, 0.8),
            )

    peak = float(np.max(np.abs(samples)))
    if peak > 0:
        samples *= plan.target_peak / peak
    return samples


def generate_project(
    output_dir: str | Path,
    *,
    duration: float = 180.0,
    seed: int = 20261118,
    name: str = "deterministic_indie_band",
    output_sample_rate: int = 48000,
    output_channels: int = 2,
    output_bits_per_sample: int = 24,
    target_loudness_lufs: float | None = -16.0,
) -> Path:
    if duration <= 0:
        raise ValueError("duration must be positive")
    destination = Path(output_dir)
    tracks_dir = destination / "tracks"
    tracks_dir.mkdir(parents=True, exist_ok=True)
    reference_output_sample = int(round(0.5 * output_sample_rate))

    track_entries: list[dict] = []
    for track_index, plan in enumerate(_TRACK_PLANS):
        anchors, _ = _anchor_positions(
            duration, output_sample_rate, plan.sample_rate, plan.drift_ppm
        )
        samples = _render_track(
            plan,
            duration,
            seed,
            track_index,
            output_sample_rate,
            reference_output_sample,
            anchors,
        )
        file_name = (
            f"tracks/{plan.name}_{plan.sample_rate}Hz_{plan.bits_per_sample}bit.wav"
        )
        write_wav_atomic(
            destination / file_name,
            samples,
            plan.sample_rate,
            plan.bits_per_sample,
        )
        track_entries.append(
            {
                "automation": _automation(duration),
                "bits_per_sample": plan.bits_per_sample,
                "channels": plan.channels,
                "drift_anchors": anchors or None,
                "file": file_name,
                "name": plan.name,
                "sample_rate": plan.sample_rate,
            }
        )

    output: dict = {
        "bits_per_sample": output_bits_per_sample,
        "channels": output_channels,
        "loudness_tolerance_lu": 0.5,
        "sample_rate": output_sample_rate,
        "true_peak_limit_dbtp": -1.0,
    }
    if target_loudness_lufs is not None:
        output["target_loudness_lufs"] = target_loudness_lufs

    manifest = {
        "automation_config": {
            "gain_max_db": 6.0,
            "gain_min_db": -48.0,
            "jump_threshold_db": 3.0,
            "smoothing_seconds": 0.05,
        },
        "output": output,
        "project": {
            "name": name,
            "output_seconds": duration,
            "seed": seed,
        },
        "schema_version": 1,
        "sync": {"reference_pulse_output_sample": reference_output_sample},
        "tracks": track_entries,
    }
    manifest_path = destination / "manifest.json"
    write_json_atomic(manifest_path, manifest)
    return manifest_path


def read_manifest_json(path: str | Path) -> dict:
    with Path(path).open("r", encoding="utf-8") as handle:
        return json.load(handle)
