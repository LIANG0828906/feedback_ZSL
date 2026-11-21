from __future__ import annotations

import struct
import sys
import wave
from array import array
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

from .hashing import content_hash, pretty_json_bytes, write_bytes_atomic
from .layout import ZERO_HASH, compute_layout
from .models import (
    DescriptionInput,
    LayoutParams,
    SCHEMA_ASSET,
    SentenceInput,
    SubtitleAsset,
)
from .timebase import (
    FRAME_HEADER_SIZE,
    FRAME_MAGIC,
    SAMPLE_RATE,
    format_timecode,
    read_frame_table,
    read_wav_info,
    samples_to_ticks,
)


TEXT_POOL = tuple("字幕时间轴音频窗口描述轨道预览刷新说话人版本哈希本地交付帧率样本一致增量排版朗读无障碍优先裁决")
NARRATION_POOL = tuple("画面中人物缓缓走过窗边字幕与声音同步出现镜头转向远处城市灯光")


class DeterministicRng:
    """Small xorshift64* generator with stable output across Python versions."""

    def __init__(self, seed: int) -> None:
        self.state = (seed ^ 0x9E3779B97F4A7C15) & 0xFFFFFFFFFFFFFFFF
        if self.state == 0:
            self.state = 0x123456789ABCDEF

    def next_u64(self) -> int:
        value = self.state
        value ^= (value >> 12) & 0xFFFFFFFFFFFFFFFF
        value ^= (value << 25) & 0xFFFFFFFFFFFFFFFF
        value ^= (value >> 27) & 0xFFFFFFFFFFFFFFFF
        value &= 0xFFFFFFFFFFFFFFFF
        self.state = value
        return (value * 0x2545F4914F6CDD1D) & 0xFFFFFFFFFFFFFFFF

    def randrange(self, low: int, high: int) -> int:
        if high <= low:
            return low
        return low + self.next_u64() % (high - low)

    def choice(self, values: Iterable[Any]) -> Any:
        sequence = tuple(values)
        return sequence[self.next_u64() % len(sequence)]


@dataclass(frozen=True)
class CaseSpec:
    directory: str
    sentence_count: int
    default_cps: str
    description_density: int
    channels: int
    recovery: bool = False


DEFAULT_SPECS = (
    CaseSpec("case_01_s06_c10_d25_mono", 6, "10", 25, 1),
    CaseSpec("case_02_s12_c14_d50_51", 12, "14", 50, 6),
    CaseSpec("case_03_s20_c12_d00_51", 20, "12", 0, 6),
    CaseSpec("case_04_s08_c10_d25_recovery", 8, "10", 25, 1, recovery=True),
)


def _text(rng: DeterministicRng, length: int, *, narration: bool = False) -> str:
    pool = NARRATION_POOL if narration else TEXT_POOL
    return "".join(rng.choice(pool) for _ in range(length))


def _asset_id(seed: int, directory: str) -> str:
    return content_hash({"directory": directory, "kind": "asset", "seed": seed})[:24]


def generate_asset(spec: CaseSpec, seed: int) -> SubtitleAsset:
    rng = DeterministicRng(seed ^ int(content_hash({"case": spec.directory})[:16], 16))
    speakers = ("SPEAKER_A", "SPEAKER_B", "SPEAKER_C")
    cps_values = ("8", "10", "11", "12", "13", "14", "15", "19", "20")
    sentences: list[SentenceInput] = []
    cursor_ms = 1000
    for index in range(spec.sentence_count):
        length = rng.randrange(6, 22)
        duration_ms = rng.randrange(850, 2900)
        gap_ms = rng.randrange(100, 950)
        start_ms = cursor_ms + gap_ms
        end_ms = start_ms + duration_ms
        cps: str | None = None
        if rng.randrange(0, 100) < 38:
            cps = rng.choice(cps_values)
        sentences.append(
            SentenceInput(
                id=f"S{index + 1:04d}",
                text=_text(rng, length),
                start=format_timecode(start_ms * 6_000),
                end=format_timecode(end_ms * 6_000),
                speaker=speakers[rng.randrange(0, len(speakers))],
                cps=cps,
            )
        )
        cursor_ms = end_ms
    descriptions: list[DescriptionInput] = []
    if spec.description_density > 0 and spec.sentence_count:
        count = max(1, round(spec.sentence_count * spec.description_density / 100))
        count = min(count, max(1, spec.sentence_count - 1))
        anchors: list[int] = []
        while len(anchors) < count:
            candidate = rng.randrange(0, spec.sentence_count - 1)
            if candidate not in anchors:
                anchors.append(candidate)
        priorities = (40, 60, 70, 80)
        for desc_index, anchor_index in enumerate(sorted(anchors), 1):
            descriptions.append(
                DescriptionInput(
                    id=f"D{desc_index:04d}",
                    after_sentence_id=sentences[anchor_index].id,
                    text=_text(rng, rng.randrange(8, 18), narration=True),
                    speaker="NARRATOR",
                    max_cps=rng.choice(("8", "10", "11", "12", "14")),
                    min_duration_ms=400,
                    priority=priorities[(desc_index - 1) % len(priorities)],
                )
            )
    params = LayoutParams(
        default_cps=spec.default_cps,
        min_duration_ms=800,
        min_gap_ms=200,
        sentence_priority=50,
        default_channel=0,
        channel_map={},
    )
    return SubtitleAsset(
        asset_id=_asset_id(seed, spec.directory),
        sentences=tuple(sentences),
        descriptions=tuple(descriptions),
        parameters=params,
    )


def write_wav(path: Path, *, duration_samples: int, channels: int, seed: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    rng = DeterministicRng(seed ^ 0xA5A5A5A5)
    offsets = tuple(rng.randrange(0, 4001) for _ in range(channels))
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(2)
        handle.setframerate(SAMPLE_RATE)
        handle.setnframes(duration_samples)
        chunk_frames = 4096
        for offset in range(0, duration_samples, chunk_frames):
            count = min(chunk_frames, duration_samples - offset)
            values = array("h")
            for index in range(offset, offset + count):
                for channel in range(channels):
                    # Integer-only sawtooth + low-frequency square component:
                    # byte-identical on every supported desktop platform.
                    saw = (
                        (index * (37 + channel * 11) + offsets[channel] + channel * 1009)
                        % 4001
                    ) - 2000
                    square = ((index // 13 + channel * 7) % 101 - 50) * 8
                    values.append(max(-32768, min(32767, saw + square)))
            if sys.byteorder != "little":
                values.byteswap()
            handle.writeframesraw(values.tobytes())


def write_frame_table(path: Path, *, audio_samples: int, frame_size: int = 2000) -> None:
    count = (audio_samples + frame_size - 1) // frame_size
    positions = [index * frame_size for index in range(count) if index * frame_size <= audio_samples]
    header = FRAME_MAGIC + struct.pack("<IIIII", 1, 24, 1, len(positions), 0)
    payload = bytearray(header)
    for position in positions:
        payload.extend(struct.pack("<Q", position))
    write_bytes_atomic(path, bytes(payload))


def generate_case(root: Path, spec: CaseSpec, seed: int) -> dict[str, Any]:
    case_dir = root / spec.directory
    case_dir.mkdir(parents=True, exist_ok=True)
    for stale_name in (
        "layout_state.json",
        "layout_state.json.lock",
        "final_layout.json",
        "trace.jsonl",
    ):
        (case_dir / stale_name).unlink(missing_ok=True)
    asset = generate_asset(spec, seed)
    write_bytes_atomic(case_dir / "subtitles.json", pretty_json_bytes(asset.to_dict()))
    last_end_ticks = 0
    for sentence in asset.sentences:
        last_end_ticks = max(last_end_ticks, _timecode_ticks(sentence.end))
    duration_samples = (last_end_ticks + 6_000_000 * 4) // 125
    duration_samples = (duration_samples + 1999) // 2000 * 2000
    write_wav(
        case_dir / "program.wav",
        duration_samples=duration_samples,
        channels=spec.channels,
        seed=seed,
    )
    write_frame_table(case_dir / "frames.ftbl", audio_samples=duration_samples)
    wav = read_wav_info(case_dir / "program.wav")
    frames = read_frame_table(case_dir / "frames.ftbl")
    baseline = compute_layout(asset, wav, frames, revision=1, parent_hash=ZERO_HASH)
    write_bytes_atomic(case_dir / "baseline.json", pretty_json_bytes(baseline))
    return {
        "asset_id": asset.asset_id,
        "baseline_hash": baseline["commit_hash"],
        "channels": spec.channels,
        "default_cps": spec.default_cps,
        "description_density": spec.description_density,
        "directory": spec.directory,
        "recovery": spec.recovery,
        "sentence_count": spec.sentence_count,
    }


def _timecode_ticks(value: str) -> int:
    from .timebase import parse_timecode

    return parse_timecode(value)


def generate_corpus(root: Path | str, *, seed: int = 7, specs: Iterable[CaseSpec] = DEFAULT_SPECS) -> dict[str, Any]:
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    cases = [generate_case(root, spec, seed) for spec in specs]
    manifest = {
        "cases": cases,
        "schema": "subtitle-demo-manifest/v1",
        "seed": seed,
    }
    write_bytes_atomic(root / "manifest.json", pretty_json_bytes(manifest))
    return manifest

