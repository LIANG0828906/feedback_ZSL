from __future__ import annotations

import json
import wave
from pathlib import Path
from typing import Any

import numpy as np

from livetranscode.util import write_json_atomic


def base_config(track_ids: list[str] | None = None) -> dict[str, Any]:
    names = track_ids or ["low", "high"]
    tracks = []
    for index, name in enumerate(names):
        tracks.append(
            {
                "id": name,
                "target_bitrate_kbps": 500 + index * 500,
                "audio": {"resample_rate": 24000 if index == 0 else 48000, "gain_db": 0.0},
                "video": {"quantization": 10},
                "memory_limit_bytes": 2_000_000,
                "degrade_stages": [
                    {"name": "full", "block_size": 65536, "temp_factor": 1.0},
                    {"name": "shrink_block", "block_size": 16384, "temp_factor": 0.75},
                    {"name": "minimal_buffers", "block_size": 4096, "temp_factor": 0.5},
                ],
            }
        )
    return {
        "schema_version": 1,
        "slice_duration_ms": 500,
        "global_parallelism": 2,
        "memory_budget_bytes": 3_000_000,
        "slice_memory_budget_bytes": 2_500_000,
        "decode_buffer_capacity_ms": 5000,
        "target_buffer_ms": 1500,
        "low_buffer_ms": 400,
        "max_decode_buffer_bytes": 4_000_000,
        "tracks": tracks,
        "failure_injection": {},
    }


def write_wav(path: Path, sample_rate: int, channels: int, seconds: float) -> None:
    count = int(sample_rate * seconds)
    positions = np.arange(count, dtype=np.int64)
    samples = np.zeros((count, channels), dtype="<i2")
    for channel in range(channels):
        samples[:, channel] = ((positions * (220 + channel * 110)) % sample_rate - sample_rate // 2).astype("<i2")
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(2)
        handle.setframerate(sample_rate)
        handle.setcomptype("NONE", "not compressed")
        handle.writeframes(samples.tobytes())


def write_frames(path: Path, seconds: float, fps: int = 30, timescale: int = 90000) -> None:
    duration = timescale // fps
    count = int(seconds * fps)
    value = {
        "timescale": timescale,
        "duration": count * duration,
        "frames": [{"index": index, "pts": index * duration, "duration": duration} for index in range(count)],
    }
    write_json_atomic(path, value)


def write_config(path: Path, config: dict[str, Any]) -> None:
    path.write_text(json.dumps(config, indent=2, sort_keys=True) + "\n", encoding="utf-8")

