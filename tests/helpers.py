from __future__ import annotations

import json
import wave
from pathlib import Path
from typing import Any

from a11y_rhythm.models import samples_to_timecode
from a11y_rhythm.samples import write_frame_table
from a11y_rhythm.timebase import load_source_bundle


SAMPLE_RATE = 48_000
FPS_NUM = 25
FPS_DEN = 1


def write_fixture(
    root: Path,
    *,
    name: str = "fixture",
    duration_seconds: float = 10.0,
    channels: int = 1,
    speech: list[dict[str, Any]],
    descriptions: list[dict[str, Any]] | None = None,
    max_iterations: int = 64,
    sample_rate: int = SAMPLE_RATE,
) -> dict[str, Any]:
    root.mkdir(parents=True, exist_ok=True)
    frame_count = round(duration_seconds * FPS_NUM / FPS_DEN)
    sample_count = round(frame_count * SAMPLE_RATE * FPS_DEN / FPS_NUM)
    wav_path = root / f"{name}.wav"
    frame_path = root / f"{name}.frames.bin"
    source_path = root / f"{name}.json"
    with wave.open(str(wav_path), "wb") as wav:
        wav.setnchannels(channels)
        wav.setsampwidth(2)
        wav.setframerate(SAMPLE_RATE)
        wav.writeframes(b"\x00\x00" * (sample_count * channels))
    write_frame_table(
        frame_path,
        sample_rate=SAMPLE_RATE,
        frame_count=frame_count,
        fps_num=FPS_NUM,
        fps_den=FPS_DEN,
    )
    source = {
        "schema_version": 1,
        "material_id": f"test-{name}",
        "sample_rate": sample_rate,
        "wav": wav_path.name,
        "frame_table": frame_path.name,
        "params_version": "test-v1",
        "max_iterations": max_iterations,
        "speech": speech,
        "descriptions": descriptions or [],
    }
    source_path.write_text(json.dumps(source, ensure_ascii=False, sort_keys=True), encoding="utf-8")
    return {
        "root": root,
        "source_path": source_path,
        "wav_path": wav_path,
        "frame_path": frame_path,
        "source": source,
        "sample_count": sample_count,
        "frame_count": frame_count,
    }


def item(
    item_id: str,
    start: float,
    end: float,
    text: str,
    *,
    speaker: str = "SPEAKER_1",
    priority: int = 50,
    max_cps: float = 12.0,
    min_gap_ms: int = 0,
) -> dict[str, Any]:
    return {
        "id": item_id,
        "text": text,
        "start": samples_to_timecode(round(start * SAMPLE_RATE), SAMPLE_RATE),
        "end": samples_to_timecode(round(end * SAMPLE_RATE), SAMPLE_RATE),
        "speaker": speaker,
        "priority": priority,
        "max_reading_rate_cps": max_cps,
        "estimated_reading_rate_cps": max_cps * 0.7,
        "min_gap_ms": min_gap_ms,
    }


def load_fixture(fixture: dict[str, Any]):
    bundle, wav_info, frame_table = load_source_bundle(fixture["source_path"])
    coverage = min(wav_info.sample_count, frame_table.duration_samples)
    return bundle, frame_table, coverage, wav_info


def conflict_fixture(root: Path, *, max_iterations: int = 64, desc_priority: int = 90):
    speech = [
        item("sp001", 1.0, 2.5, "Morning river.", priority=40),
        item("sp002", 2.5, 4.0, "Boats pass.", priority=41),
        item("sp003", 5.0, 6.5, "Workers repair.", priority=42),
        item("sp004", 8.0, 9.2, "Quiet water.", priority=43),
    ]
    descriptions = [
        item(
            "ds001",
            2.0,
            3.0,
            "Boat turns.",
            speaker="AUDIO_DESCRIPTION",
            priority=desc_priority,
            max_cps=12.0,
            min_gap_ms=120,
        )
    ]
    return write_fixture(
        root,
        name=f"conflict-{desc_priority}-{max_iterations}",
        duration_seconds=18.0,
        speech=speech,
        descriptions=descriptions,
        max_iterations=max_iterations,
    )


def incremental_fixture(root: Path):
    speech = [
        item("sp001", 0.4, 1.3, "Valley.", priority=50),
        item("sp002", 1.3, 2.1, "Train.", priority=41),
        item("sp003", 2.1, 2.9, "Crates.", priority=42),
        item("sp004", 13.0, 13.8, "Dusk.", priority=43),
    ]
    descriptions = [
        item(
            "ds001",
            1.5,
            2.3,
            "Signal.",
            speaker="AUDIO_DESCRIPTION",
            priority=90,
            max_cps=12.0,
            min_gap_ms=120,
        )
    ]
    return write_fixture(
        root,
        name="incremental",
        duration_seconds=16.0,
        speech=speech,
        descriptions=descriptions,
        max_iterations=64,
    )
