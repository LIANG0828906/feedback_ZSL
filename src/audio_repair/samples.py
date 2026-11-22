"""Deterministic local music/speech fixtures with fixed seeds and no I/O assets."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import numpy as np
from scipy.io import wavfile
from scipy.signal import lfilter

SEED_MUSIC = 0xA11CE
SEED_SPEECH = 0x5EEC4
SEED_EDGE = 0xED6E


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _pink_noise(rng: np.random.Generator, length: int, channels: int) -> np.ndarray:
    white = rng.normal(0.0, 1.0, (length, channels))
    return lfilter([0.049922035, -0.095993537, 0.050612699, -0.004408786],
                   [1.0, -2.494956002, 2.017265875, -0.522189400],
                   white, axis=0)


def _normalize(data: np.ndarray, peak: float) -> np.ndarray:
    maximum = float(np.max(np.abs(data)))
    if maximum <= 1e-12:
        return data
    return data * (peak / maximum)


def _music_fixture() -> tuple[int, np.ndarray]:
    sample_rate = 44100
    duration = 7.43
    total = int(round(sample_rate * duration))
    rng = np.random.default_rng(SEED_MUSIC)
    t = np.arange(total, dtype=np.float64) / sample_rate
    left = np.zeros(total, dtype=np.float64)
    right = np.zeros(total, dtype=np.float64)

    chords = (
        (110.00, (1.0, 1.25, 1.50, 2.00)),
        (146.83, (1.0, 1.20, 1.50, 1.875)),
        (174.61, (1.0, 1.25, 1.50, 2.00)),
        (130.81, (1.0, 1.20, 1.50, 2.00)),
    )
    chord_duration = 1.85
    for chord_index in range(4):
        start = 0.45 + chord_index * chord_duration
        stop = min(duration, start + chord_duration)
        mask = (t >= start) & (t < stop)
        local_t = t[mask] - start
        envelope = np.minimum(1.0, local_t / 0.035) * np.minimum(
            1.0, (stop - start - local_t) / 0.12
        )
        envelope = np.clip(envelope, 0.0, 1.0)
        root, ratios = chords[chord_index]
        for note_index, ratio in enumerate(ratios):
            frequency = root * ratio
            phase = 2.0 * np.pi * frequency * local_t
            tone = (
                0.42 * np.sin(phase)
                + 0.16 * np.sin(2.01 * phase + 0.2)
                + 0.08 * np.sin(3.02 * phase + 0.5)
            )
            pan = 0.5 + 0.18 * np.sin(note_index + chord_index)
            left[mask] += envelope * tone * (1.0 - pan)
            right[mask] += envelope * tone * pan
        bass = 0.30 * np.sin(2.0 * np.pi * (root / 2.0) * local_t)
        left[mask] += envelope * bass
        right[mask] += envelope * bass * 0.95

    beat = 0.46
    click_times = np.arange(0.45, duration, beat)
    for click_index, click_time in enumerate(click_times):
        start = int(click_time * sample_rate)
        length = int(0.085 * sample_rate)
        stop = min(total, start + length)
        if stop <= start:
            continue
        local_t = np.arange(stop - start, dtype=np.float64) / sample_rate
        if click_index % 4 == 0:
            kick = np.sin(2.0 * np.pi * (62.0 - 28.0 * local_t) * local_t)
            pulse = 0.22 * kick * np.exp(-local_t * 28.0)
        else:
            pulse = rng.normal(0.0, 1.0, stop - start) * np.exp(-local_t * 55.0) * 0.055
        left[start:stop] += pulse
        right[start:stop] += pulse * 0.92

    hiss = _pink_noise(rng, total, 2) * 0.018
    hum = 0.0055 * np.sin(2.0 * np.pi * 50.0 * t)
    hum2 = 0.0025 * np.sin(2.0 * np.pi * 150.0 * t + 0.3)
    crackle = np.zeros(total, dtype=np.float64)
    positions = rng.integers(0, total, size=28)
    crackle[positions] = rng.normal(0.0, 0.34, size=28)
    left += hiss[:, 0] + hum + hum2 + crackle
    right += hiss[:, 1] + 0.92 * hum + 0.8 * hum2 - 0.6 * crackle
    data = _normalize(np.stack([left, right], axis=1), 0.78)
    return sample_rate, data.astype(np.float32)


def _speech_fixture() -> tuple[int, np.ndarray]:
    sample_rate = 32000
    duration = 5.81
    total = int(round(sample_rate * duration))
    rng = np.random.default_rng(SEED_SPEECH)
    t = np.arange(total, dtype=np.float64) / sample_rate
    voice = np.zeros(total, dtype=np.float64)

    syllables = []
    cursor = 0.38
    vowels = (
        (730.0, 1090.0, 2440.0),
        (270.0, 2290.0, 3010.0),
        (530.0, 1840.0, 2480.0),
        (390.0, 1990.0, 2550.0),
        (640.0, 1190.0, 2390.0),
    )
    while cursor < duration - 0.4:
        length = float(rng.uniform(0.16, 0.35))
        gap = float(rng.uniform(0.045, 0.13))
        f0 = float(rng.uniform(105.0, 178.0))
        vowelset = vowels[int(rng.integers(0, len(vowels)))]
        syllables.append((cursor, min(duration, cursor + length), f0, vowelset))
        cursor += length + gap

    for syllable_index, (start, stop, f0, formants) in enumerate(syllables):
        begin = int(start * sample_rate)
        end = min(total, int(stop * sample_rate))
        if end <= begin:
            continue
        local_t = np.arange(end - begin, dtype=np.float64) / sample_rate
        vibrato = 1.0 + 0.012 * np.sin(2.0 * np.pi * 5.3 * local_t)
        phase = 2.0 * np.pi * f0 * np.cumsum(vibrato) / sample_rate
        harmonic = np.zeros_like(local_t)
        for harmonic_index in range(1, 28):
            frequency = harmonic_index * f0
            if frequency >= sample_rate * 0.44:
                break
            formant_gain = 0.0
            for formant_index, formant in enumerate(formants):
                bandwidth = (90.0, 110.0, 150.0)[formant_index]
                formant_gain += np.exp(
                    -0.5 * ((frequency - formant) / bandwidth) ** 2
                ) * (1.0 - 0.2 * formant_index)
            amplitude = (0.34 / harmonic_index) * (0.14 + formant_gain)
            harmonic += amplitude * np.sin(harmonic_index * phase)
        envelope = np.minimum(1.0, local_t / 0.018) * np.minimum(
            1.0, (local_t[-1] - local_t) / 0.035
        )
        envelope = np.clip(envelope, 0.0, 1.0)
        voice[begin:end] += harmonic * envelope
        if syllable_index % 2 == 0:
            noise_length = min(end - begin, int(0.045 * sample_rate))
            fricative = rng.normal(0.0, 1.0, noise_length)
            fricative = lfilter([0.7, -0.4], [1.0], fricative)
            fade = np.linspace(1.0, 0.0, noise_length)
            voice[begin : begin + noise_length] += 0.035 * fricative * fade

    hiss = _pink_noise(rng, total, 1)[:, 0] * 0.012
    hum = 0.004 * np.sin(2.0 * np.pi * 50.0 * t)
    voice += hiss + hum
    data = _normalize(voice, 0.72)
    return sample_rate, data.astype(np.float32)


def _edge_fixtures() -> list[tuple[str, int, np.ndarray, str]]:
    rng = np.random.default_rng(SEED_EDGE)
    rate_mono = 16000
    count_mono = 1000
    t_mono = np.arange(count_mono, dtype=np.float64) / rate_mono
    mono = 0.24 * np.sin(2.0 * np.pi * 440.0 * t_mono)
    mono += 0.01 * rng.normal(0.0, 1.0, count_mono)
    mono = _normalize(mono, 0.45).astype(np.float32)

    rate_stereo = 22050
    segment = 4096
    count_stereo = segment * 2 + 1
    t_stereo = np.arange(count_stereo, dtype=np.float64) / rate_stereo
    stereo = np.stack(
        [
            0.20 * np.sin(2.0 * np.pi * 330.0 * t_stereo),
            0.20 * np.sin(2.0 * np.pi * 550.0 * t_stereo + 0.2),
        ],
        axis=1,
    )
    stereo += 0.008 * rng.normal(0.0, 1.0, stereo.shape)
    stereo = _normalize(stereo, 0.40).astype(np.float32)
    return [
        ("boundary_mono_1000.wav", rate_mono, mono, "short-mono"),
        ("boundary_stereo_tail1.wav", rate_stereo, stereo, "one-sample-tail-stereo"),
    ]


def _sample_record(
    path: Path, sample_rate: int, data: np.ndarray, profile: str, noise_type: str
) -> dict[str, Any]:
    total = int(data.shape[0])
    return {
        "file": path.name,
        "profile": profile,
        "noise_type": noise_type,
        "sample_rate": sample_rate,
        "channels": 1 if data.ndim == 1 else int(data.shape[1]),
        "total_samples": total,
        "duration_seconds": total / sample_rate,
        "sha256": _sha256(path),
        "remainder_with_16384": total % 16384,
    }


def generate_deterministic_samples(output_dir: str | Path) -> dict[str, Any]:
    """Write deterministic WAV fixtures and a content-hash manifest."""

    directory = Path(output_dir)
    directory.mkdir(parents=True, exist_ok=True)
    records = []

    music_rate, music = _music_fixture()
    music_path = directory / "music_noisy_stereo.wav"
    wavfile.write(music_path, music_rate, music)
    records.append(
        _sample_record(
            music_path,
            music_rate,
            music,
            "music",
            "pink hiss + 50/150 Hz hum + sparse crackle",
        )
    )

    speech_rate, speech = _speech_fixture()
    speech_path = directory / "speech_noisy_mono.wav"
    wavfile.write(speech_path, speech_rate, speech)
    records.append(
        _sample_record(
            speech_path,
            speech_rate,
            speech,
            "speech",
            "pink hiss + 50 Hz hum",
        )
    )

    for name, rate, data, noise_type in _edge_fixtures():
        path = directory / name
        wavfile.write(path, rate, data)
        records.append(_sample_record(path, rate, data, "edge", noise_type))

    manifest = {
        "format": "audio-repair-deterministic-samples-v1",
        "generation": "numpy fixed-seed synthesis; no network or external audio",
        "seeds": {
            "music": SEED_MUSIC,
            "speech": SEED_SPEECH,
            "edge": SEED_EDGE,
        },
        "samples": records,
    }
    manifest_path = directory / "manifest.json"
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    return manifest

