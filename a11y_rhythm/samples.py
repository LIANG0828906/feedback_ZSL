"""Deterministic local sample generator (stdlib only)."""

from __future__ import annotations

import array
import hashlib
import json
import math
import random
import struct
import wave
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any

from .models import canonical_json, content_hash, samples_to_timecode
from .timebase import (
    FRAME_ENTRY,
    FRAME_HEADER,
    FRAME_MAGIC,
    FRAME_VERSION,
    round_fraction,
)


@dataclass(frozen=True)
class Scenario:
    name: str
    seed: int
    speech_count: int
    description_count: int
    duration_seconds: int
    channels: int
    max_cps: float
    contention: float
    fps_num: int = 25
    fps_den: int = 1


SCENARIOS = (
    Scenario("mono_conflict", 20261122, 12, 5, 32, 1, 16.0, 0.92),
    Scenario("mono_sparse", 314159, 7, 1, 16, 1, 18.0, 0.45),
    Scenario("surround_dense", 271828, 10, 5, 28, 6, 15.0, 0.96),
)


def render_timecode(sample: int, sample_rate: int) -> str:
    return samples_to_timecode(sample, sample_rate)


def write_frame_table(
    path: str | Path,
    sample_rate: int,
    frame_count: int,
    fps_num: int,
    fps_den: int,
) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    header = FRAME_HEADER.pack(
        FRAME_MAGIC,
        FRAME_VERSION,
        sample_rate,
        frame_count,
        fps_num,
        fps_den,
        FRAME_ENTRY.size,
        0,
    )
    chunks = [header]
    for index in range(frame_count):
        sample = round_fraction(Fraction(index * sample_rate * fps_den, fps_num))
        chunks.append(FRAME_ENTRY.pack(sample, index, 0))
    target.write_bytes(b"".join(chunks))


def write_pcm_wav(
    path: str | Path,
    *,
    sample_rate: int,
    channels: int,
    sample_count: int,
    seed: int,
) -> None:
    """Write deterministic 16-bit PCM with deterministic tone/noise content."""
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    rng = random.Random(seed)
    channel_freq = [110.0, 165.0, 220.0, 275.0, 330.0, 385.0]
    channel_phase = [rng.random() * math.tau for _ in range(channels)]
    block = array.array("h")
    for index in range(sample_count):
        time_value = index / sample_rate
        for channel in range(channels):
            freq = channel_freq[channel % len(channel_freq)]
            tone = math.sin(math.tau * freq * time_value + channel_phase[channel])
            modulation = 0.55 + 0.25 * math.sin(math.tau * 0.7 * time_value + channel)
            block.append(int(1400.0 * tone * modulation))
    with wave.open(str(target), "wb") as wav:
        wav.setnchannels(channels)
        wav.setsampwidth(2)
        wav.setframerate(sample_rate)
        wav.writeframes(block.tobytes())


def _sentence_text(target_length: int, sentence_index: int) -> str:
    base = (
        f"Scene {sentence_index + 1:02d} documents the river, the stone bridge, "
        "the working boats and the people who keep this place alive."
    )
    if target_length <= len(base):
        return base[: max(1, target_length)].rstrip()
    repeats = (target_length + len(base) - 1) // len(base)
    return (base + " ") * repeats


def _make_source(
    scenario: Scenario,
    sample_rate: int,
    frame_count: int,
) -> dict[str, Any]:
    rng = random.Random(scenario.seed)
    speech: list[dict[str, Any]] = []
    descriptions: list[dict[str, Any]] = []
    cursor = 5
    speech_slots: list[tuple[int, int]] = []
    for index in range(scenario.speech_count):
        if cursor + 24 >= frame_count - 8:
            break
        duration_frames = rng.randint(28, 42)
        if cursor + duration_frames >= frame_count - 8:
            duration_frames = max(20, frame_count - 8 - cursor)
        start_frame = cursor
        end_frame = start_frame + duration_frames
        speech_slots.append((start_frame, end_frame))
        duration_seconds = duration_frames * scenario.fps_den / scenario.fps_num
        text_length = max(10, round(duration_seconds * scenario.max_cps * scenario.contention))
        start_sample = round(start_frame * sample_rate * scenario.fps_den / scenario.fps_num)
        end_sample = round(end_frame * sample_rate * scenario.fps_den / scenario.fps_num)
        speech.append(
            {
                "id": f"sp{index + 1:03d}",
                "text": _sentence_text(text_length, index),
                "start": render_timecode(start_sample, sample_rate),
                "end": render_timecode(end_sample, sample_rate),
                "speaker": f"SPEAKER_{index % 3 + 1}",
                "priority": 40 + (index % 4),
                "max_reading_rate_cps": scenario.max_cps,
                "estimated_reading_rate_cps": round(scenario.max_cps * 0.72, 3),
                "min_gap_ms": 0,
            }
        )
        cursor = end_frame + rng.randint(2, 5)

    if not speech_slots:
        raise ValueError("scenario duration is too short for speech")
    for index in range(scenario.description_count):
        anchor = min(
            len(speech_slots) - 1,
            round((index + 0.5) * len(speech_slots) / max(1, scenario.description_count)),
        )
        speech_start, speech_end = speech_slots[anchor]
        start_frame = speech_start + min(8, max(2, (speech_end - speech_start) // 5))
        duration_frames = rng.randint(26, 38)
        end_frame = min(frame_count - 3, start_frame + duration_frames)
        duration_seconds = max(0.25, (end_frame - start_frame) * scenario.fps_den / scenario.fps_num)
        text_length = max(8, round(duration_seconds * scenario.max_cps * 0.62))
        start_sample = round(start_frame * sample_rate * scenario.fps_den / scenario.fps_num)
        end_sample = round(end_frame * sample_rate * scenario.fps_den / scenario.fps_num)
        descriptions.append(
            {
                "id": f"ds{index + 1:03d}",
                "text": _sentence_text(text_length, 100 + index),
                "start": render_timecode(start_sample, sample_rate),
                "end": render_timecode(end_sample, sample_rate),
                "speaker": "AUDIO_DESCRIPTION",
                "priority": 85 + index,
                "max_reading_rate_cps": scenario.max_cps * 0.9,
                "estimated_reading_rate_cps": round(scenario.max_cps * 0.55, 3),
                "min_gap_ms": 120,
            }
        )
    return {
        "schema_version": 1,
        "material_id": f"sample-{scenario.name}-{scenario.seed}",
        "sample_rate": sample_rate,
        "wav": f"{scenario.name}.wav",
        "frame_table": f"{scenario.name}.frames.bin",
        "params_version": "sample-params-v1",
        "min_readable_ratio": "1",
        "max_iterations": 192,
        "speech": speech,
        "descriptions": descriptions,
    }


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def generate_scenario(out_dir: str | Path, scenario: Scenario) -> dict[str, Any]:
    output = Path(out_dir)
    output.mkdir(parents=True, exist_ok=True)
    sample_rate = 48_000
    frame_count = scenario.duration_seconds * scenario.fps_num // scenario.fps_den
    sample_count = round(frame_count * sample_rate * scenario.fps_den / scenario.fps_num)
    wav_path = output / f"{scenario.name}.wav"
    frame_path = output / f"{scenario.name}.frames.bin"
    source_path = output / f"{scenario.name}.json"

    write_pcm_wav(
        wav_path,
        sample_rate=sample_rate,
        channels=scenario.channels,
        sample_count=sample_count,
        seed=scenario.seed,
    )
    write_frame_table(
        frame_path,
        sample_rate=sample_rate,
        frame_count=frame_count,
        fps_num=scenario.fps_num,
        fps_den=scenario.fps_den,
    )
    source = _make_source(scenario, sample_rate, frame_count)
    source_path.write_text(canonical_json(source) + "\n", encoding="utf-8")
    files = (wav_path, frame_path, source_path)
    return {
        "name": scenario.name,
        "seed": scenario.seed,
        "speech_count": len(source["speech"]),
        "description_count": len(source["descriptions"]),
        "duration_seconds": scenario.duration_seconds,
        "channels": scenario.channels,
        "max_cps": scenario.max_cps,
        "contention": scenario.contention,
        "sample_rate": sample_rate,
        "frames": frame_count,
        "samples": sample_count,
        "files": {path.name: _sha256_file(path) for path in files},
        "source_hash": content_hash(source),
    }


def generate_all(out_dir: str | Path) -> dict[str, Any]:
    output = Path(out_dir)
    scenarios = [generate_scenario(output, scenario) for scenario in SCENARIOS]
    manifest = {
        "schema_version": 1,
        "generator": "a11y_rhythm.samples",
        "deterministic": True,
        "scenarios": scenarios,
    }
    (output / "manifest.json").write_text(
        json.dumps(manifest, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    return manifest
