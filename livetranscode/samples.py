from __future__ import annotations

import wave
from pathlib import Path
from typing import Any

import numpy as np

from .util import (
    ensure_dir,
    sha256_file,
    write_json_atomic,
)


def _deterministic_signal(
    sample_rate: int,
    channels: int,
    sample_count: int,
    base_frequency: int,
    seed: int,
) -> bytes:
    positions = np.arange(sample_count, dtype=np.int64)
    channel_bytes = []
    for channel in range(channels):
        frequency = base_frequency + channel * 220
        phase = np.mod(positions * frequency, sample_rate)
        triangle = (phase * 2 * 18_000 // sample_rate - 18_000).astype(np.int64)
        mixed = positions.astype(np.uint64) + np.uint64(seed + channel * 104729)
        mixed ^= mixed >> np.uint64(30)
        mixed *= np.uint64(0xBF58476D1CE4E5B9)
        mixed ^= mixed >> np.uint64(27)
        mixed *= np.uint64(0x94D049BB133111EB)
        mixed ^= mixed >> np.uint64(31)
        noise = (mixed & np.uint64(0xFFFF)).astype(np.int64) - 32768
        values = np.clip(triangle + noise // 12, -32768, 32767).astype("<i2")
        channel_bytes.append(values.tobytes())
    interleaved = bytearray()
    for index in range(sample_count):
        for channel in range(channels):
            start = index * 2
            interleaved.extend(channel_bytes[channel][start : start + 2])
    return bytes(interleaved)


def _write_wav(
    path: Path,
    sample_rate: int,
    channels: int,
    sample_count: int,
    base_frequency: int,
    seed: int,
) -> None:
    pcm = _deterministic_signal(sample_rate, channels, sample_count, base_frequency, seed)
    buffer = path.with_suffix(".tmp")
    ensure_dir(path.parent)
    with wave.open(str(buffer), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(2)
        handle.setframerate(sample_rate)
        handle.setcomptype("NONE", "not compressed")
        handle.writeframes(pcm)
    import os

    os.replace(buffer, path)


def _frames_value(frame_count: int, timescale: int, frame_duration: int) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "timescale": timescale,
        "duration": frame_count * frame_duration,
        "frames": [
            {
                "index": index,
                "pts": index * frame_duration,
                "duration": frame_duration,
            }
            for index in range(frame_count)
        ],
    }


def _track(
    track_id: str,
    bitrate: int,
    resample_rate: int,
    gain_db: float,
    quantization: int,
    memory_limit: int,
) -> dict[str, Any]:
    return {
        "id": track_id,
        "target_bitrate_kbps": bitrate,
        "audio": {"resample_rate": resample_rate, "gain_db": gain_db},
        "video": {"quantization": quantization},
        "memory_limit_bytes": memory_limit,
        "degrade_stages": [
            {"name": "full", "block_size": 65536, "temp_factor": 1.0},
            {"name": "shrink_block", "block_size": 16384, "temp_factor": 0.75},
            {"name": "minimal_buffers", "block_size": 4096, "temp_factor": 0.5},
        ],
    }


def _config(
    tracks: list[dict[str, Any]],
    parallelism: int,
    memory_budget: int,
    slice_memory_budget: int,
    slice_duration_ms: int = 500,
    failure_injection: dict[str, str] | None = None,
) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "slice_duration_ms": slice_duration_ms,
        "global_parallelism": parallelism,
        "memory_budget_bytes": memory_budget,
        "slice_memory_budget_bytes": slice_memory_budget,
        "decode_buffer_capacity_ms": 5000,
        "target_buffer_ms": 1600,
        "low_buffer_ms": 500,
        "max_decode_buffer_bytes": 4_000_000,
        "tracks": tracks,
        "failure_injection": failure_injection or {},
    }


def _bandwidth(pattern: list[int]) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "samples": [
            {"boundary_index": index, "bandwidth_kbps": value}
            for index, value in enumerate(pattern)
        ],
    }


def _write_case(
    root: Path,
    name: str,
    sample_rate: int,
    channels: int,
    seconds: int,
    fps: int,
    tracks: list[dict[str, Any]],
    parallelism: int,
    memory_budget: int,
    slice_memory_budget: int,
    bandwidth_pattern: list[int],
    seed: int,
) -> dict[str, Any]:
    case = root / name
    ensure_dir(case)
    timescale = 90000
    frame_duration = timescale // fps
    if timescale % fps:
        raise ValueError("fps must exactly divide 90000")
    if (sample_rate * seconds) % fps:
        raise ValueError("audio duration must align with the video frame rate")
    frame_count = seconds * fps
    sample_count = sample_rate * seconds
    audio_path = case / "audio.wav"
    frames_path = case / "frames.json"
    config_path = case / "config.json"
    bandwidth_path = case / "bandwidth.json"
    _write_wav(audio_path, sample_rate, channels, sample_count, 220 + seed % 100, seed + sample_rate)
    write_json_atomic(frames_path, _frames_value(frame_count, timescale, frame_duration))
    write_json_atomic(
        config_path,
        _config(
            tracks,
            parallelism=parallelism,
            memory_budget=memory_budget,
            slice_memory_budget=slice_memory_budget,
        ),
    )
    write_json_atomic(bandwidth_path, _bandwidth(bandwidth_pattern))
    return {
        "name": name,
        "sample_rate": sample_rate,
        "channels": channels,
        "duration_seconds": seconds,
        "frame_rate": fps,
        "tracks": [track["id"] for track in tracks],
        "files": {
            "audio.wav": sha256_file(audio_path),
            "frames.json": sha256_file(frames_path),
            "config.json": sha256_file(config_path),
            "bandwidth.json": sha256_file(bandwidth_path),
        },
    }


def generate_samples(root: str | Path, seed: int = 1234) -> dict[str, Any]:
    target = Path(root).resolve()
    ensure_dir(target)
    cases: list[dict[str, Any]] = []
    cases.append(
        _write_case(
            target,
            "esports_48k_stereo",
            sample_rate=48000,
            channels=2,
            seconds=6,
            fps=30,
            tracks=[
                _track("low", 600, 24000, -1.0, 14, 1_300_000),
                _track("medium", 1200, 32000, -0.5, 10, 1_050_000),
                _track("high", 2400, 48000, 0.0, 6, 900_000),
            ],
            parallelism=2,
            memory_budget=1_500_000,
            slice_memory_budget=1_400_000,
            bandwidth_pattern=[4200, 4200, 1600, 1600, 700, 700, 3000, 3000, 1100, 1100, 5000, 5000],
            seed=seed + 1,
        )
    )
    cases.append(
        _write_case(
            target,
            "music_44k1_mono",
            sample_rate=44100,
            channels=1,
            seconds=4,
            fps=25,
            tracks=[
                _track("low", 500, 32000, -1.5, 16, 1_300_000),
                _track("medium", 1000, 44100, -0.5, 10, 1_050_000),
                _track("high", 2000, 48000, 0.0, 6, 900_000),
            ],
            parallelism=3,
            memory_budget=1_500_000,
            slice_memory_budget=1_400_000,
            bandwidth_pattern=[3500, 3500, 900, 900, 1800, 1800, 4500, 4500],
            seed=seed + 2,
        )
    )
    cases.append(
        _write_case(
            target,
            "voice_16k_mono",
            sample_rate=16000,
            channels=1,
            seconds=5,
            fps=50,
            tracks=[
                _track("voice_low", 240, 16000, -1.0, 18, 1_200_000),
                _track("voice_high", 640, 24000, 0.0, 8, 900_000),
            ],
            parallelism=2,
            memory_budget=1_500_000,
            slice_memory_budget=1_400_000,
            bandwidth_pattern=[1800, 1800, 500, 500, 900, 900, 1600, 1600, 300, 300],
            seed=seed + 3,
        )
    )
    cases.append(
        _write_case(
            target,
            "budget_degrade",
            sample_rate=48000,
            channels=2,
            seconds=3,
            fps=30,
            tracks=[
                _track("degrade_auto", 900, 48000, 0.0, 8, 620_000),
                _track("degrade_min", 500, 24000, -1.0, 14, 260_000),
                _track("reject_tiny", 300, 16000, -2.0, 20, 130_000),
            ],
            parallelism=2,
            memory_budget=1_500_000,
            slice_memory_budget=1_400_000,
            bandwidth_pattern=[2500, 2500, 1200, 1200, 700, 700],
            seed=seed + 4,
        )
    )
    baseline = {
        "schema_version": 1,
        "generator": "livetranscode.samples",
        "seed": seed,
        "cases": cases,
    }
    write_json_atomic(target / "baseline.json", baseline)
    return baseline

