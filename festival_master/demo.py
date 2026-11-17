from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .audio_io import atomic_write_bytes


def demo_manifest(
    *,
    duration_seconds: float = 7.0,
    block_samples: int = 4096,
    target_loudness_lufs: float = -16.0,
) -> dict[str, Any]:
    channels = [
        {
            "id": "lead_vocal",
            "instrument": "主唱",
            "path": "audio/lead_vocal.wav",
            "anchor": {"x": -2.4, "y": 3.2, "z": 0.8},
            "start_offset_ms": 17.0,
            "downmix_weight": 1.0,
        },
        {
            "id": "guitar",
            "instrument": "吉他",
            "path": "audio/guitar.wav",
            "anchor": {"x": 2.8, "y": 4.0, "z": 0.4},
            "start_offset_ms": 6.0,
            "downmix_weight": 0.72,
        },
        {
            "id": "bass",
            "instrument": "贝斯",
            "path": "audio/bass.wav",
            "anchor": {"x": 1.9, "y": 3.8, "z": 0.2},
            "start_offset_ms": 13.0,
            "downmix_weight": 0.85,
        },
        {
            "id": "drums",
            "instrument": "鼓组",
            "path": "audio/drums.wav",
            "anchor": {"x": 3.6, "y": 2.8, "z": 1.0},
            "start_offset_ms": 10.0,
            "downmix_weight": 0.78,
        },
        {
            "id": "stage_environment",
            "instrument": "环境",
            "path": "audio/stage_environment.wav",
            "anchor": {"x": -4.2, "y": 5.0, "z": 1.8},
            "start_offset_ms": 2.0,
            "downmix_weight": 0.48,
        },
        {
            "id": "audience",
            "instrument": "观众",
            "path": "audio/audience.wav",
            "anchor": {"x": -1.2, "y": 6.5, "z": 0.9},
            "start_offset_ms": 19.0,
            "downmix_weight": 0.55,
        },
    ]
    return {
        "schema_version": 1,
        "seed": 239239,
        "sample_rate": 48000,
        "bit_depth": 24,
        "num_channels": 1,
        "source_duration_seconds": duration_seconds,
        "stage": {
            "x": [-5.0, 5.0],
            "y": [0.0, 10.0],
            "z": [0.0, 5.0],
        },
        "listener": {"x": 0.0, "y": -1.0, "z": 1.7},
        "target_loudness_lufs": target_loudness_lufs,
        "true_peak_limit_dbtp": -1.0,
        "spatial_stability_threshold_db": 0.01,
        "block_samples": block_samples,
        "groups": {
            "人声组": {"gain_db": 0.0},
            "乐器组": {"gain_db": -1.5},
            "环境组": {"gain_db": -3.0},
        },
        "channels": channels,
    }


def write_demo_manifest(path: Path, **kwargs: Any) -> dict[str, Any]:
    payload = demo_manifest(**kwargs)
    encoded = json.dumps(
        payload, ensure_ascii=False, indent=2, allow_nan=False
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(path, encoded)
    return payload
