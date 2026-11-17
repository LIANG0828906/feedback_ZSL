from __future__ import annotations

import hashlib
import math
import zlib
from pathlib import Path
from typing import Any

import numpy as np
from scipy import signal

from .audio_io import write_wav
from .manifest import INSTRUMENT_DISPLAY, Manifest
from .spatial import pan_parameters, source_angles


def _rng(seed: int, instrument_key: str) -> np.random.Generator:
    salt = zlib.crc32(instrument_key.encode("utf-8")) & 0xFFFFFFFF
    state = (int(seed) & 0xFFFFFFFFFFFFFFFF) ^ (salt << 17) ^ 0x9E3779B97F4A7C15
    return np.random.Generator(np.random.PCG64(state & 0xFFFFFFFFFFFFFFFF))


def _envelope(length: int, sample_rate: int, attack: float, release: float) -> np.ndarray:
    n = np.arange(length, dtype=np.float64)
    attack_n = max(1, int(round(attack * sample_rate)))
    release_n = max(1, int(round(release * sample_rate)))
    env = np.ones(length, dtype=np.float64)
    if attack_n > 1:
        env[:attack_n] = np.linspace(0.0, 1.0, attack_n, endpoint=False)
    if release_n > 1 and release_n < length:
        env[-release_n:] *= np.linspace(1.0, 0.0, release_n, endpoint=True)
    return env


def _add_at(target: np.ndarray, offset: int, values: np.ndarray) -> None:
    if offset >= target.size or offset + values.size <= 0:
        return
    source_start = max(0, -offset)
    target_start = max(0, offset)
    count = min(values.size - source_start, target.size - target_start)
    if count > 0:
        target[target_start : target_start + count] += values[source_start : source_start + count]


def _fade_edges(values: np.ndarray, sample_rate: int, seconds: float = 0.005) -> np.ndarray:
    result = np.asarray(values, dtype=np.float64).copy()
    n = min(result.size // 2, max(1, int(round(seconds * sample_rate))))
    if n > 1:
        ramp = np.linspace(0.0, 1.0, n, endpoint=False)
        result[:n] *= ramp
        result[-n:] *= ramp[::-1]
    return result


def _synthesize_vocal(rng: np.random.Generator, sample_rate: int, duration: float) -> np.ndarray:
    total = int(round(sample_rate * duration))
    output = np.zeros(total, dtype=np.float64)
    t_cursor = 0.0
    scale = [220.0, 246.94, 293.66, 329.63, 293.66, 246.94, 220.0, 196.0]
    index = 0
    while t_cursor < duration:
        note_duration = 0.34 + 0.055 * float(rng.random())
        offset = int(round(t_cursor * sample_rate))
        note_length = min(int(round(note_duration * sample_rate)), max(0, total - offset))
        if note_length <= 0:
            break
        frequency = scale[index % len(scale)] * (1.0 + 0.0025 * float(rng.standard_normal()))
        t = np.arange(note_length, dtype=np.float64) / sample_rate
        vibrato = 1.0 + 0.006 * np.sin(2.0 * math.pi * 5.1 * t)
        phase = 2.0 * math.pi * frequency * np.cumsum(vibrato) / sample_rate
        tone = (
            0.58 * np.sin(phase)
            + 0.25 * np.sin(2.0 * phase)
            + 0.11 * np.sin(3.0 * phase)
            + 0.06 * np.sin(5.0 * phase)
        )
        env = _envelope(note_length, sample_rate, 0.018, 0.09)
        breath = signal.lfilter([0.12, -0.04], [1.0], rng.standard_normal(note_length))
        output[offset : offset + note_length] += (tone + 0.035 * breath) * env
        t_cursor += note_duration + 0.065
        index += 1
    return output


def _synthesize_guitar(rng: np.random.Generator, sample_rate: int, duration: float) -> np.ndarray:
    total = int(round(sample_rate * duration))
    output = np.zeros(total, dtype=np.float64)
    chords = [
        (110.0, 164.81, 220.0),
        (98.0, 146.83, 196.0),
        (130.81, 196.0, 261.63),
        (123.47, 185.0, 246.94),
    ]
    t_cursor = 0.0
    chord_index = 0
    while t_cursor < duration:
        chord = chords[chord_index % len(chords)]
        for string_index, frequency in enumerate(chord):
            offset = int(round((t_cursor + string_index * 0.012) * sample_rate))
            note_length = min(int(0.95 * sample_rate), total - offset)
            if note_length <= 0:
                continue
            t = np.arange(note_length, dtype=np.float64) / sample_rate
            decay = np.exp(-3.8 * t)
            tone = np.zeros(note_length, dtype=np.float64)
            for harmonic in range(1, 7):
                detune = 1.0 + 0.0007 * (harmonic - 1) * (string_index - 1)
                tone += (0.52 / harmonic) * np.sin(
                    2.0 * math.pi * frequency * harmonic * detune * t + 0.17 * harmonic
                )
            pick = rng.standard_normal(min(note_length, int(0.015 * sample_rate)))
            pick *= np.exp(-np.arange(pick.size) / max(1.0, 0.0025 * sample_rate))
            tone[: pick.size] += 0.16 * pick
            _add_at(output, offset, tone * decay)
        t_cursor += 0.72
        chord_index += 1
    return output


def _synthesize_bass(rng: np.random.Generator, sample_rate: int, duration: float) -> np.ndarray:
    total = int(round(sample_rate * duration))
    output = np.zeros(total, dtype=np.float64)
    notes = [55.0, 55.0, 65.41, 49.0, 55.0, 73.42, 65.41, 49.0]
    t_cursor = 0.0
    index = 0
    while t_cursor < duration:
        note_length = min(int(round(0.54 * sample_rate)), total - int(round(t_cursor * sample_rate)))
        if note_length <= 0:
            break
        frequency = notes[index % len(notes)]
        t = np.arange(note_length, dtype=np.float64) / sample_rate
        tone = (
            0.72 * np.sin(2.0 * math.pi * frequency * t)
            + 0.19 * np.sin(4.0 * math.pi * frequency * t)
            + 0.09 * np.sin(6.0 * math.pi * frequency * t)
        )
        env = _envelope(note_length, sample_rate, 0.007, 0.13)
        click = rng.standard_normal(min(note_length, int(0.008 * sample_rate)))
        click *= np.exp(-np.arange(click.size) / max(1.0, 0.0015 * sample_rate))
        tone[: click.size] += 0.08 * click
        _add_at(output, int(round(t_cursor * sample_rate)), tone * env)
        t_cursor += 0.48
        index += 1
    return output


def _synthesize_drums(rng: np.random.Generator, sample_rate: int, duration: float) -> np.ndarray:
    total = int(round(sample_rate * duration))
    output = np.zeros(total, dtype=np.float64)
    beat = 60.0 / 112.0
    kick_times = np.arange(0.0, duration, beat)
    snare_times = np.arange(beat * 0.5, duration, beat)
    hat_times = np.arange(beat * 0.25, duration, beat * 0.5)

    for when in kick_times:
        length = min(int(0.35 * sample_rate), total - int(round(when * sample_rate)))
        if length <= 0:
            continue
        t = np.arange(length, dtype=np.float64) / sample_rate
        phase = 2.0 * math.pi * (58.0 * t + 92.0 * (1.0 - np.exp(-t / 0.028)))
        body = np.sin(phase) * np.exp(-8.5 * t)
        click = rng.standard_normal(min(length, int(0.004 * sample_rate)))
        click *= np.exp(-np.arange(click.size) / max(1.0, 0.0008 * sample_rate))
        body[: click.size] += 0.10 * click
        _add_at(output, int(round(when * sample_rate)), body)

    for when in snare_times:
        length = min(int(0.22 * sample_rate), total - int(round(when * sample_rate)))
        if length <= 0:
            continue
        t = np.arange(length, dtype=np.float64) / sample_rate
        noise = signal.lfilter([0.9, -0.35], [1.0, -0.18], rng.standard_normal(length))
        noise *= np.exp(-18.0 * t)
        tone = 0.24 * np.sin(2.0 * math.pi * 185.0 * t) * np.exp(-24.0 * t)
        _add_at(output, int(round(when * sample_rate)), 0.32 * noise + tone)

    for when in hat_times:
        length = min(int(0.065 * sample_rate), total - int(round(when * sample_rate)))
        if length <= 0:
            continue
        noise = signal.lfilter([1.0, -0.94], [1.0], rng.standard_normal(length))
        noise *= np.exp(-75.0 * np.arange(length) / sample_rate)
        _add_at(output, int(round(when * sample_rate)), 0.11 * noise)
    return output


def _synthesize_environment(rng: np.random.Generator, sample_rate: int, duration: float) -> np.ndarray:
    length = int(round(sample_rate * duration))
    white = rng.standard_normal(length)
    broad = signal.lfilter([0.18, 0.12, -0.06], [1.0, -0.55, 0.18], white)
    rumble = signal.lfilter([0.01], [1.0, -0.995], rng.standard_normal(length))
    t = np.arange(length, dtype=np.float64) / sample_rate
    wind = 1.0 + 0.22 * np.sin(2.0 * math.pi * 0.17 * t + 0.4)
    return (0.55 * broad + 1.4 * rumble) * wind


def _synthesize_crowd(rng: np.random.Generator, sample_rate: int, duration: float) -> np.ndarray:
    length = int(round(sample_rate * duration))
    t = np.arange(length, dtype=np.float64) / sample_rate
    bed = signal.lfilter([1.0, -0.5], [1.0, -0.82], rng.standard_normal(length))
    bed *= 0.45 + 0.10 * np.sin(2.0 * math.pi * 0.31 * t)
    claps = np.zeros(length, dtype=np.float64)
    count = max(8, int(duration * 8.5))
    positions = rng.integers(0, max(1, length), size=count)
    for position in positions:
        clap_length = min(int(0.055 * sample_rate), length - int(position))
        if clap_length <= 2:
            continue
        burst = rng.standard_normal(clap_length)
        burst *= np.exp(-70.0 * np.arange(clap_length) / sample_rate)
        claps[int(position) : int(position) + clap_length] += 0.28 * burst
    return bed + claps


SYNTHESIZERS = {
    "vocal": _synthesize_vocal,
    "guitar": _synthesize_guitar,
    "bass": _synthesize_bass,
    "drums": _synthesize_drums,
    "environment": _synthesize_environment,
    "crowd": _synthesize_crowd,
}


def synthesize_channel(manifest: Manifest, channel_index: int) -> np.ndarray:
    spec = manifest.channels[channel_index]
    rng = _rng(manifest.seed, spec.instrument_key)
    wave_data = SYNTHESIZERS[spec.instrument_key](
        rng, manifest.sample_rate, spec.source_duration_seconds
    )
    wave_data = np.nan_to_num(wave_data, nan=0.0, posinf=0.0, neginf=0.0)
    peak = float(np.max(np.abs(wave_data))) if wave_data.size else 0.0
    target_peak = 0.11 + 0.025 * ((manifest.seed + channel_index * 17) % 5) / 4.0
    if peak > 0:
        wave_data *= target_peak / peak
    return _fade_edges(wave_data, manifest.sample_rate, 0.002)


def generate_material(manifest: Manifest) -> dict[str, Any]:
    report_channels: list[dict[str, Any]] = []
    for index, spec in enumerate(manifest.channels):
        samples = synthesize_channel(manifest, index)
        wav_hash, decoded = write_wav(
            Path(spec.path),
            samples,
            manifest.sample_rate,
            spec.expected_bit_depth,
        )
        decoded_hash = hashlib.sha256(
            np.asarray(decoded, dtype="<f8").tobytes(order="C")
        ).hexdigest()
        azimuth, elevation = source_angles(spec.anchor, manifest.listener)
        pan = pan_parameters(azimuth, manifest.sample_rate)
        report_channels.append(
            {
                "channel": spec.channel_id,
                "instrument": spec.instrument,
                "instrument_key": spec.instrument_key,
                "path": spec.path_report,
                "sample_rate": manifest.sample_rate,
                "bit_depth": spec.expected_bit_depth,
                "channels": 1,
                "source_samples": int(samples.size),
                "start_offset_samples": spec.start_offset_samples,
                "anchor": {
                    "x": spec.anchor[0],
                    "y": spec.anchor[1],
                    "z": spec.anchor[2],
                },
                "azimuth_deg": azimuth,
                "elevation_deg": elevation,
                "ild_db": pan.ild_db,
                "itd_seconds": pan.itd_seconds,
                "source_hash": decoded_hash,
                "wav_sha256": wav_hash,
            }
        )
    return {
        "schema_version": 1,
        "status": "generated",
        "seed": manifest.seed,
        "manifest_sha256": manifest.digest,
        "sample_rate": manifest.sample_rate,
        "bit_depth": manifest.bit_depth,
        "channels": report_channels,
    }


def instrument_display(key: str) -> str:
    return INSTRUMENT_DISPLAY.get(key, key)
