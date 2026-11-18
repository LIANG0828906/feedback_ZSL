from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np
from scipy import signal
from scipy.io import wavfile

from .resample import expected_source_sample
from .scene import SyncAnchor


def _fade_envelope(
    length: int, sample_rate: int, attack_s: float, release_s: float
) -> np.ndarray:
    envelope = np.ones(length, dtype=np.float64)
    attack = min(length, max(1, int(round(attack_s * sample_rate))))
    release = min(length, max(1, int(round(release_s * sample_rate))))
    envelope[:attack] *= np.linspace(0.0, 1.0, attack, endpoint=False)
    if release:
        envelope[-release:] *= np.linspace(1.0, 0.0, release, endpoint=False)
    return envelope


def _speech_like(length: int, sample_rate: int, seed: int) -> np.ndarray:
    time = np.arange(length, dtype=np.float64) / float(sample_rate)
    rng = np.random.default_rng(seed)
    f0 = 158.0 + 54.0 * (time / max(time[-1], 1e-9)) + 4.0 * np.sin(2.0 * np.pi * 2.4 * time)
    phase = 2.0 * np.pi * np.cumsum(f0, dtype=np.float64) / sample_rate
    voiced = (
        0.48 * np.sin(phase)
        + 0.22 * np.sin(2.02 * phase + 0.15)
        + 0.12 * np.sin(3.01 * phase + 0.3)
        + 0.06 * np.sin(4.97 * phase + 0.7)
    )
    noise = signal.lfilter([0.08], [1.0, -0.92], rng.standard_normal(length))
    modulation = 0.76 + 0.24 * np.sin(2.0 * np.pi * 3.1 * time + 0.2)
    return (voiced + 0.10 * noise) * modulation * _fade_envelope(
        length, sample_rate, 0.004, 0.025
    )


def _sfx_like(length: int, sample_rate: int, seed: int) -> np.ndarray:
    time = np.arange(length, dtype=np.float64) / float(sample_rate)
    rng = np.random.default_rng(seed)
    start_hz = 1250.0
    end_hz = 310.0
    chirp_phase = 2.0 * np.pi * (
        start_hz * time + 0.5 * (end_hz - start_hz) * time**2 / max(time[-1], 1e-9)
    )
    noise = signal.lfilter([0.2, -0.12], [1.0, -0.7], rng.standard_normal(length))
    transient = np.exp(-time / 0.075)
    return (
        0.45 * np.sin(chirp_phase) * transient
        + 0.20 * noise * np.exp(-time / 0.16)
    ) * _fade_envelope(length, sample_rate, 0.002, 0.03)


def _ambience_stereo(length: int, sample_rate: int, seed: int) -> np.ndarray:
    time = np.arange(length, dtype=np.float64) / float(sample_rate)
    rng = np.random.default_rng(seed)
    noise = rng.standard_normal((length, 2))
    sos = signal.butter(2, [80.0, 1600.0], btype="bandpass", fs=sample_rate, output="sos")
    filtered = signal.sosfilt(sos, noise, axis=0)
    left = (
        0.018 * np.sin(2.0 * np.pi * 73.0 * time)
        + 0.012 * np.sin(2.0 * np.pi * 117.0 * time + 0.4)
        + 0.035 * filtered[:, 0]
    )
    right = (
        0.018 * np.sin(2.0 * np.pi * 73.0 * time + 0.11)
        + 0.012 * np.sin(2.0 * np.pi * 119.0 * time + 0.8)
        + 0.035 * filtered[:, 1]
    )
    stereo = np.column_stack([left, right])
    return stereo * _fade_envelope(length, sample_rate, 0.04, 0.08)[:, None]


def _music_stereo(length: int, sample_rate: int, seed: int) -> np.ndarray:
    time = np.arange(length, dtype=np.float64) / float(sample_rate)
    chord = (196.0, 246.94, 293.66, 392.0)
    left = np.zeros(length, dtype=np.float64)
    right = np.zeros(length, dtype=np.float64)
    for index, frequency in enumerate(chord):
        pan = -0.65 + 0.43 * index
        gain_left = np.sqrt(0.5 * (1.0 - pan))
        gain_right = np.sqrt(0.5 * (1.0 + pan))
        lfo = 0.72 + 0.28 * np.sin(2.0 * np.pi * (0.16 + index * 0.013) * time + index)
        tone = np.sin(2.0 * np.pi * frequency * time + index * 0.7)
        left += 0.034 * gain_left * lfo * tone
        right += 0.034 * gain_right * lfo * tone
    stereo = np.column_stack([left, right])
    return stereo * _fade_envelope(length, sample_rate, 0.12, 0.15)[:, None]


def _place_mono(
    destination: np.ndarray, clip: np.ndarray, start_sample: int
) -> None:
    if start_sample < 0:
        clip = clip[-start_sample:]
        start_sample = 0
    available = destination.shape[0] - start_sample
    if available <= 0:
        return
    count = min(available, clip.shape[0])
    destination[start_sample : start_sample + count] += clip[:count]


def _place_stereo(
    destination: np.ndarray, clip: np.ndarray, start_sample: int
) -> None:
    if start_sample < 0:
        clip = clip[-start_sample:]
        start_sample = 0
    available = destination.shape[0] - start_sample
    if available <= 0:
        return
    count = min(available, clip.shape[0])
    destination[start_sample : start_sample + count] += clip[:count]


def _write_int16(path: Path, samples: np.ndarray, sample_rate: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    peak = float(np.max(np.abs(samples))) if samples.size else 0.0
    if peak > 0.89:
        samples = samples * (0.89 / peak)
    pcm = np.rint(np.clip(samples, -1.0, 1.0) * 32767.0).astype(np.int16)
    wavfile.write(str(path), sample_rate, pcm)


def generate_demo(out_dir: str | Path) -> dict[str, Any]:
    """Generate a deterministic multi-rate scene and return its JSON object."""

    root = Path(out_dir).resolve()
    assets = root / "assets"
    root.mkdir(parents=True, exist_ok=True)
    assets.mkdir(parents=True, exist_ok=True)
    duration = 6.0

    dialogue_rate = 44100
    sfx_rate = 96000
    stereo_rate = 48000
    dialogue_samples = int(duration * dialogue_rate)
    sfx_samples = int(duration * sfx_rate)
    stereo_samples = int(duration * stereo_rate)

    dialogue_anchors_raw = [
        {"source_sample": 0, "target_time": 0.0},
        {"source_sample": 66150, "target_time": 1.493},
        {"source_sample": 132300, "target_time": 2.997},
        {"source_sample": 220500, "target_time": 4.988},
        {"source_sample": 264600, "target_time": 6.0},
    ]
    dialogue_anchors = tuple(
        SyncAnchor(anchor["source_sample"], anchor["target_time"])
        for anchor in dialogue_anchors_raw
    )

    dialogue = np.zeros(dialogue_samples, dtype=np.float64)
    dialogue_events = [
        ("DLG_001", 0.55, 1.25, 2),
        ("DLG_002", 3.55, 1.10, 17),
    ]
    placed_event_samples: dict[str, int] = {}
    for event_id, start_time, event_duration, seed in dialogue_events:
        nominal = expected_source_sample(
            dialogue_anchors, start_time, dialogue_rate
        )
        jitter_samples = int(round(dialogue_rate * (0.0030 if event_id == "DLG_001" else 0.0014)))
        start_sample = int(round(nominal)) + jitter_samples
        placed_event_samples[event_id] = start_sample
        clip = _speech_like(
            int(round(event_duration * dialogue_rate)), dialogue_rate, seed
        )
        _place_mono(dialogue, clip, start_sample)

    sfx = np.zeros(sfx_samples, dtype=np.float64)
    sfx_nominal = expected_source_sample((), 2.15, sfx_rate)
    sfx_start = int(round(sfx_nominal)) + int(round(sfx_rate * 0.0012))
    placed_event_samples["SFX_001"] = sfx_start
    _place_mono(
        sfx,
        _sfx_like(int(round(0.65 * sfx_rate)), sfx_rate, 31),
        sfx_start,
    )

    ambience = _ambience_stereo(stereo_samples, stereo_rate, 47)
    music = _music_stereo(stereo_samples, stereo_rate, 61)

    dialogue_path = assets / "dialogue_44100.wav"
    sfx_path = assets / "effects_96000.wav"
    ambience_path = assets / "ambience_48000.wav"
    music_path = assets / "music_48000.wav"
    _write_int16(dialogue_path, dialogue, dialogue_rate)
    _write_int16(sfx_path, sfx, sfx_rate)
    _write_int16(ambience_path, ambience, stereo_rate)
    _write_int16(music_path, music, stereo_rate)

    scene: dict[str, Any] = {
        "version": 1,
        "name": "deterministic_corridor_demo",
        "duration": duration,
        "output": {
            "sample_rate": 48000,
            "channels": 2,
            "block_frames": 8192,
            "report_frame_stride": 4800,
            "target_loudness_lufs": -23.0,
            "true_peak_limit_dbtp": -1.0,
        },
        "camera": {
            "up": [0.0, 0.0, 1.0],
            "path": [
                {
                    "time": 0.0,
                    "position": [0.0, 0.0, 1.7],
                    "forward": [0.0, 1.0, 0.0],
                },
                {
                    "time": 1.5,
                    "position": [0.8, 1.6, 1.7],
                    "forward": [0.18, 1.0, 0.0],
                },
                {
                    "time": 3.0,
                    "position": [1.8, 3.1, 1.72],
                    "forward": [0.95, 0.4, 0.0],
                },
                {
                    "time": 4.5,
                    "position": [2.6, 4.5, 1.7],
                    "forward": [0.2, 1.0, 0.0],
                },
                {
                    "time": 6.0,
                    "position": [2.2, 6.0, 1.7],
                    "forward": [0.0, 1.0, 0.0],
                },
            ],
        },
        "tracks": [
            {
                "name": "dialogue",
                "path": "assets/dialogue_44100.wav",
                "sample_rate": dialogue_rate,
                "channels": 1,
                "gain_db": 0.0,
                "sync_anchors": dialogue_anchors_raw,
                "events": [
                    {
                        "id": "DLG_001",
                        "type": "dialogue",
                        "start_time": 0.55,
                        "duration": 1.25,
                        "source_position": [2.4, 2.8, 1.6],
                        "gain_db": 0.0,
                        "detection": {"threshold": 0.08, "search_ms": 35.0},
                    },
                    {
                        "id": "DLG_002",
                        "type": "dialogue",
                        "start_time": 3.55,
                        "duration": 1.10,
                        "source_position": [-0.5, 4.0, 1.5],
                        "gain_db": -1.0,
                        "detection": {"threshold": 0.08, "search_ms": 35.0},
                    },
                ],
            },
            {
                "name": "effects",
                "path": "assets/effects_96000.wav",
                "sample_rate": sfx_rate,
                "channels": 1,
                "gain_db": 0.0,
                "events": [
                    {
                        "id": "SFX_001",
                        "type": "effect",
                        "start_time": 2.15,
                        "duration": 0.65,
                        "source_position": [-2.5, 1.5, 0.8],
                        "gain_db": -2.0,
                        "detection": {"threshold": 0.06, "search_ms": 35.0},
                    }
                ],
            },
            {
                "name": "ambience",
                "path": "assets/ambience_48000.wav",
                "sample_rate": stereo_rate,
                "channels": 2,
                "gain_db": 0.0,
                "events": [
                    {
                        "id": "AMB_001",
                        "type": "ambience",
                        "start_time": 0.0,
                        "duration": 6.0,
                        "source_position": [0.0, 5.0, 2.5],
                        "gain_db": -12.0,
                        "detection": {"threshold": 0.05, "search_ms": 20.0},
                    }
                ],
            },
            {
                "name": "music",
                "path": "assets/music_48000.wav",
                "sample_rate": stereo_rate,
                "channels": 2,
                "gain_db": 0.0,
                "events": [
                    {
                        "id": "MUS_001",
                        "type": "music",
                        "start_time": 0.0,
                        "duration": 6.0,
                        "source_position": [1.0, 6.0, 3.0],
                        "gain_db": -8.0,
                        "detection": {"threshold": 0.05, "search_ms": 20.0},
                    }
                ],
            },
        ],
        "reverb": {
            "blend_width": 0.20,
            "regions": [
                {"start_time": 0.0, "end_time": 2.0, "rt60": 0.72, "wet_dry": 0.18},
                {"start_time": 2.0, "end_time": 4.0, "rt60": 1.18, "wet_dry": 0.31},
                {"start_time": 4.0, "end_time": 6.0, "rt60": 0.90, "wet_dry": 0.24},
            ],
        },
        "occluders": [
            {
                "id": "partition",
                "min": [-1.2, 2.0, 0.0],
                "max": [1.5, 2.12, 3.0],
            }
        ],
        "spatial_limits": {
            "max_ild_db": 10.0,
            "max_itd_us": 620.0,
            "max_ild_rate_db_per_s": 90.0,
            "max_itd_rate_us_per_s": 6000.0,
            "head_radius_m": 0.0875,
            "speed_of_sound_m_s": 343.0,
            "ild_strength_db": 8.0,
        },
    }
    scene_path = root / "scene.json"
    scene_path.write_text(
        json.dumps(scene, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    return scene
