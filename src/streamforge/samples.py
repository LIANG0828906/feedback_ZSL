from __future__ import annotations

import json
import math
import shutil
import wave
from pathlib import Path
from typing import Any

import numpy as np

from .generator import atomic_write
from .models import canonical_json


def _write_json(path: Path, value: Any) -> None:
    atomic_write(path, canonical_json(value) + b"\n")


def write_pcm16_wav(
    path: Path,
    *,
    sample_rate: int,
    channels: int,
    duration_ms: int,
    seed: int,
) -> None:
    count = sample_rate * duration_ms // 1000
    time = np.arange(count, dtype=np.float64) / sample_rate
    rng = np.random.default_rng(seed)
    left = (
        0.24 * np.sin(2.0 * math.pi * 440.0 * time)
        + 0.08 * np.sin(2.0 * math.pi * 660.0 * time)
        + rng.normal(0.0, 0.005, count)
    )
    if channels == 2:
        right = (
            0.21 * np.sin(2.0 * math.pi * 554.37 * time)
            + 0.07 * np.sin(2.0 * math.pi * 880.0 * time)
            + rng.normal(0.0, 0.005, count)
        )
        samples = np.stack([left, right], axis=1)
    else:
        samples = left[:, None]
    pcm = np.clip(samples * 32767.0, -32768, 32767).astype("<i2")
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(2)
        handle.setframerate(sample_rate)
        handle.writeframes(pcm.tobytes(order="C"))


def _video_document(duration_ms: int, fps: int = 25) -> dict[str, Any]:
    frame_duration_ms = 1000 // fps
    frames = []
    for display_index in range(duration_ms // frame_duration_ms):
        frames.append(
            {
                "display_index": display_index,
                "duration_ms": frame_duration_ms,
                "pts_ms": display_index * frame_duration_ms,
            }
        )
    return {
        "duration_ns": duration_ms * 1_000_000,
        "fps": fps,
        "frames": frames,
    }


def generate_samples(
    root: Path,
    *,
    seed: int = 20261121,
    duration_ms: int = 3200,
    overwrite: bool = True,
) -> dict[str, Path]:
    root = root.resolve()
    if root.exists() and overwrite:
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)

    audio_stereo = root / "audio_stereo_48k.wav"
    audio_mono = root / "audio_mono_16k.wav"
    audio_44k = root / "audio_stereo_44k.wav"
    audio_24k = root / "audio_mono_24k.wav"
    write_pcm16_wav(
        audio_stereo,
        sample_rate=48000,
        channels=2,
        duration_ms=duration_ms,
        seed=seed,
    )
    write_pcm16_wav(
        audio_mono,
        sample_rate=16000,
        channels=1,
        duration_ms=duration_ms,
        seed=seed + 1,
    )
    write_pcm16_wav(
        audio_44k,
        sample_rate=44100,
        channels=2,
        duration_ms=duration_ms,
        seed=seed + 2,
    )
    write_pcm16_wav(
        audio_24k,
        sample_rate=24000,
        channels=1,
        duration_ms=duration_ms,
        seed=seed + 3,
    )
    video_path = root / "video_frames.json"
    video_document = _video_document(duration_ms)
    _write_json(video_path, video_document)

    track_config = {
        "config_version": f"demo-v1-seed{seed}",
        "source": {
            "audio": audio_stereo.name,
            "video": video_path.name,
        },
        "timeline": {
            "slice_duration_ms": 400,
            "tolerance_ms": 2,
        },
        "tiers": [
            {
                "audio_sample_rate": 32000,
                "gain_db": -2.0,
                "id": "low",
                "target_bitrate_kbps": 350,
                "video_qp": 34,
            },
            {
                "audio_sample_rate": 44100,
                "gain_db": -1.0,
                "id": "mid",
                "target_bitrate_kbps": 700,
                "video_qp": 29,
            },
            {
                "audio_sample_rate": 48000,
                "gain_db": 0.0,
                "id": "high",
                "target_bitrate_kbps": 1800,
                "video_qp": 24,
            },
        ],
    }
    budget_config = {
        "buffer_high_bytes": 1100000,
        "buffer_low_bytes": 260000,
        "decode_buffer_bytes": 1600000,
        "global_parallelism": 3,
        "initial_block_bytes": 32768,
        "min_block_bytes": 4096,
        "single_slice_memory_bytes": 1048576,
        "starvation_ms": 800,
        "starvation_rounds": 2,
        "total_memory_bytes": 3145728,
    }
    playback_config = {
        "bandwidth_safety_ratio": 0.8,
        "initial_buffer_bytes": 700000,
        "initial_tier": "mid",
    }
    bandwidth_config = {
        "samples": [
            {"kbps": 420, "time_ms": 0},
            {"kbps": 900, "time_ms": 400},
            {"kbps": 2400, "time_ms": 800},
            {"kbps": 1500, "time_ms": 1200},
            {"kbps": 520, "time_ms": 1600},
            {"kbps": 2100, "time_ms": 2000},
            {"kbps": 760, "time_ms": 2400},
            {"kbps": 1800, "time_ms": 2800},
        ]
    }
    track_path = root / "track_config.json"
    budget_path = root / "budgets.json"
    playback_path = root / "playback.json"
    bandwidth_path = root / "bandwidth.json"
    _write_json(track_path, track_config)
    _write_json(budget_path, budget_config)
    _write_json(playback_path, playback_config)
    _write_json(bandwidth_path, bandwidth_config)

    duplicate = json.loads(canonical_json(video_document))
    duplicate["frames"][3]["pts_ms"] = duplicate["frames"][2]["pts_ms"]
    invalid_video_path = root / "video_duplicate_pts.json"
    _write_json(invalid_video_path, duplicate)

    invalid_tier_config = json.loads(canonical_json(track_config))
    invalid_tier_config["tiers"][2]["audio_sample_rate"] = 1000
    invalid_tier_path = root / "track_invalid_tier.json"
    _write_json(invalid_tier_path, invalid_tier_config)

    return {
        "audio": audio_stereo,
        "audio_mono": audio_mono,
        "audio_44k": audio_44k,
        "audio_24k": audio_24k,
        "video": video_path,
        "track_config": track_path,
        "budgets": budget_path,
        "playback": playback_path,
        "bandwidth": bandwidth_path,
        "invalid_video": invalid_video_path,
        "invalid_tier_config": invalid_tier_path,
        "root": root,
    }
