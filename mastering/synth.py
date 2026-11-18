from __future__ import annotations

import json
from pathlib import Path

import numpy as np

from .wavio import encode_pcm, wav_header


def write_pcm_wav(path: Path, samples: np.ndarray, sample_rate: int, bit_depth: int = 24) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    channels = samples.shape[1]
    payload = encode_pcm(samples, bit_depth)
    header = wav_header(sample_rate, channels, bit_depth, samples.shape[0])
    with path.open("wb") as handle:
        handle.write(header)
        handle.write(payload)


def _envelope(length: int, sample_rate: int, pauses: list[tuple[float, float]]) -> np.ndarray:
    time = np.arange(length, dtype=np.float64) / sample_rate
    envelope = np.ones(length, dtype=np.float64)
    for start, end in pauses:
        envelope[(time >= start) & (time < end)] = 0.0
    syllable_rate = 3.7
    modulation = 0.55 + 0.45 * np.sin(2.0 * np.pi * syllable_rate * time) ** 2
    fade = min(length, int(round(sample_rate * 0.012)))
    if fade:
        ramp = np.linspace(0.0, 1.0, fade, endpoint=False)
        envelope[:fade] *= ramp
        envelope[-fade:] *= ramp[::-1]
    return envelope * modulation


def make_voice(
    language: str,
    duration_seconds: float,
    sample_rate: int,
    pauses: list[tuple[float, float]],
    trailing_silence_seconds: float = 0.0,
) -> np.ndarray:
    length = int(round(duration_seconds * sample_rate))
    time = np.arange(length, dtype=np.float64) / sample_rate
    base = {"en": 143.0, "ja": 191.0, "de": 117.0}.get(language, 155.0)
    vibrato = 1.0 + 0.012 * np.sin(2.0 * np.pi * 4.8 * time)
    phase = 2.0 * np.pi * base * np.cumsum(vibrato) / sample_rate
    signal = (
        0.52 * np.sin(phase)
        + 0.25 * np.sin(2.0 * phase + 0.3)
        + 0.13 * np.sin(3.0 * phase + 1.2)
        + 0.06 * np.sin(5.0 * phase + 0.7)
    )
    signal *= _envelope(length, sample_rate, pauses)
    signal *= 0.19
    plosive_position = int(round(0.72 * sample_rate))
    plosive_width = min(160, length - plosive_position)
    if plosive_width > 0:
        plosive = np.sin(np.linspace(0.0, np.pi, plosive_width))
        signal[plosive_position : plosive_position + plosive_width] += 0.93 * plosive
    if trailing_silence_seconds > 0.0:
        silence = int(round(trailing_silence_seconds * sample_rate))
        signal[-silence:] = 0.0
    separated = np.empty((length, 2), dtype=np.float64)
    separated[:, 0] = signal
    separated[:, 1] = (
        0.92 * signal
        + 0.025 * np.sin(2.0 * np.pi * 223.0 * time) * np.abs(signal)
    )
    return np.clip(separated, -0.995, 0.995)


def make_music(duration_seconds: float, sample_rate: int) -> np.ndarray:
    length = int(round(duration_seconds * sample_rate))
    time = np.arange(length, dtype=np.float64) / sample_rate
    music = np.zeros((length, 2), dtype=np.float64)
    chords = [
        (110.0, 164.81, 220.0),
        (98.0, 146.83, 196.0),
        (123.47, 185.0, 246.94),
        (87.31, 130.81, 174.61),
    ]
    section = np.floor(time / 3.0).astype(np.int64) % len(chords)
    for index, chord in enumerate(chords):
        mask = section == index
        for frequency in chord:
            music[mask, 0] += 0.013 * np.sin(2.0 * np.pi * frequency * time[mask])
            music[mask, 1] += 0.013 * np.sin(
                2.0 * np.pi * frequency * 1.002 * time[mask] + 0.4
            )
    tremolo = 0.75 + 0.25 * np.sin(2.0 * np.pi * 0.5 * time)
    music *= tremolo[:, None]
    click_positions = np.arange(sample_rate // 2, length, sample_rate * 2)
    for position in click_positions:
        width = min(96, length - position)
        if width <= 0:
            continue
        impulse = np.exp(-np.arange(width) / 13.0)
        impulse *= np.sin(np.linspace(0.0, np.pi, width))
        music[position : position + width, 0] += 0.93 * impulse
        music[position : position + width, 1] -= 0.91 * impulse
    return np.clip(music, -0.99, 0.99)


def make_sfx(duration_seconds: float, sample_rate: int) -> np.ndarray:
    length = int(round(duration_seconds * sample_rate))
    time = np.arange(length, dtype=np.float64) / sample_rate
    sfx = np.zeros((length, 2), dtype=np.float64)
    whoosh = np.exp(-((time - 1.7) ** 2) / 0.05)
    sfx[:, 0] += 0.055 * whoosh * np.sin(2.0 * np.pi * (620.0 - 180.0 * time) * time)
    sfx[:, 1] += 0.052 * whoosh * np.sin(
        2.0 * np.pi * (590.0 - 160.0 * time) * time + 0.5
    )
    bell = np.exp(-((time - 4.1) ** 2) / 0.008)
    sfx[:, 0] += 0.07 * bell * np.sin(2.0 * np.pi * 880.0 * time)
    sfx[:, 1] += 0.07 * bell * np.sin(2.0 * np.pi * 1320.0 * time + 0.2)
    return np.clip(sfx, -0.99, 0.99)


def create_demo_workspace(base_dir: Path, *, compact: bool = False) -> tuple[Path, Path]:
    base_dir = base_dir.resolve()
    input_dir = base_dir / "input"
    output_dir = base_dir / "output"
    sample_rate = 48000
    duration = 2.4 if compact else 6.0
    scale = duration / 6.0
    voices = {
        "en": (
            "voice_en.wav",
            4.35 * scale,
            [(1.15 * scale, 1.42 * scale), (2.55 * scale, 2.86 * scale), (3.72 * scale, 3.94 * scale)],
            0.0,
        ),
        "ja": (
            "voice_ja.wav",
            6.35 * scale,
            [(1.05 * scale, 1.31 * scale), (2.45 * scale, 2.77 * scale), (3.95 * scale, 4.19 * scale)],
            0.55 * scale,
        ),
        "de": (
            "voice_de.wav",
            5.15 * scale,
            [(0.95 * scale, 1.22 * scale), (2.10 * scale, 2.38 * scale), (3.44 * scale, 3.70 * scale)],
            0.0,
        ),
    }
    for language, (filename, voice_duration, pauses, silence) in voices.items():
        samples = make_voice(language, voice_duration, sample_rate, pauses, silence)
        write_pcm_wav(input_dir / filename, samples, sample_rate)

    music = make_music(duration, sample_rate)
    sfx = make_sfx(duration, sample_rate)
    write_pcm_wav(input_dir / "music.wav", music, sample_rate)
    write_pcm_wav(input_dir / "sfx.wav", sfx, sample_rate)

    rules = {
        "schema_version": 1,
        "languages": {
            "en": {"target_lufs": -23.0, "true_peak_dbtp": -1.0},
            "ja": {"target_lufs": -24.0, "true_peak_dbtp": -1.0},
            "de": {"target_lufs": -23.5, "true_peak_dbtp": -1.0},
        },
        "buses": {
            "music_target_lufs": -34.0,
            "sfx_target_lufs": -39.0,
            "music_gain_db": 0.0,
            "sfx_gain_db": 0.0,
        },
        "ducking": {
            "depth_db": 12.0,
            "threshold_dbfs": -34.0,
            "attack_ms": 18.0,
            "release_ms": 260.0,
            "frame_ms": 10.0,
        },
        "limiter": {
            "true_peak_dbtp": -1.0,
            "lookahead_ms": 5.0,
            "release_ms": 90.0,
            "oversample": 4,
            "safety_margin_db": 0.08,
        },
        "silence": {
            "threshold_dbfs": -60.0,
            "min_active_ratio": 0.01,
            "frame_ms": 20.0,
        },
        "duration": {"trim_strategy": "trailing_silence"},
    }
    rules_path = base_dir / "rules.json"
    rules_path.write_text(
        json.dumps(rules, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )

    project = {
        "project_version": 1,
        "sample_rate": sample_rate,
        "channels": 2,
        "bit_depth": 24,
        "block_frames": 4800,
        "required_languages": ["en", "ja", "de"],
        "rules_path": "rules.json",
        "output_dir": "output",
        "tracks": {
            "voice_en": {
                "role": "voice",
                "language": "en",
                "path": "input/voice_en.wav",
            },
            "voice_ja": {
                "role": "voice",
                "language": "ja",
                "path": "input/voice_ja.wav",
            },
            "voice_de": {
                "role": "voice",
                "language": "de",
                "path": "input/voice_de.wav",
            },
            "music": {"role": "music", "path": "input/music.wav"},
            "sfx": {"role": "sfx", "path": "input/sfx.wav"},
        },
    }
    project_path = base_dir / "project.json"
    project_path.write_text(
        json.dumps(project, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    output_dir.mkdir(parents=True, exist_ok=True)
    return project_path, rules_path

