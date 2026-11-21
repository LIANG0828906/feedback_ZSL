from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from subtitle_localizer.generator import generate_case, write_frame_table, write_wav
from subtitle_localizer.hashing import pretty_json_bytes, write_bytes_atomic
from subtitle_localizer.models import (
    DescriptionInput,
    LayoutParams,
    SentenceInput,
    SubtitleAsset,
)


def make_case(
    root: Path,
    *,
    name: str = "case",
    sentences: list[SentenceInput] | None = None,
    descriptions: list[DescriptionInput] | None = None,
    params: LayoutParams | None = None,
    channels: int = 1,
    duration_samples: int = 480_000,
) -> Path:
    case_dir = root / name
    case_dir.mkdir(parents=True, exist_ok=True)
    asset = SubtitleAsset(
        asset_id=f"asset-{name}",
        sentences=tuple(
            sentences
            or [
                SentenceInput("S1", "第一句字幕", "00:00:01.000000", "00:00:02.000000", "A"),
                SentenceInput("S2", "第二句字幕", "00:00:03.000000", "00:00:04.000000", "B"),
                SentenceInput("S3", "第三句字幕", "00:00:05.000000", "00:00:06.000000", "B"),
            ]
        ),
        descriptions=tuple(descriptions or []),
        parameters=params or LayoutParams(),
    )
    write_bytes_atomic(case_dir / "subtitles.json", pretty_json_bytes(asset.to_dict()))
    write_wav(case_dir / "program.wav", duration_samples=duration_samples, channels=channels, seed=3)
    write_frame_table(case_dir / "frames.ftbl", audio_samples=duration_samples)
    return case_dir


def make_generated_case(root: Path, *, index: int = 0) -> Path:
    from subtitle_localizer.generator import DEFAULT_SPECS

    spec = DEFAULT_SPECS[index]
    generate_case(root, spec, 7)
    return root / spec.directory


def load_json(path: Path) -> Any:
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)

