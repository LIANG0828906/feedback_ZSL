from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import struct
import sys
import zlib
from array import array
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any

from .jsonutil import canonical_json_bytes, encode_base64, sha256_bytes
from .model import Frame
from .timebase import Timebase
from .truth import compute_truth_fingerprint


GENERATOR_VERSION = "samples-v1"
DEFAULT_SEED = 20261123
MODEL_TIMEBASE = Timebase(1, 90000)


@dataclass(frozen=True)
class GroupSpec:
    sample_id: str
    title: str
    duration_seconds: Fraction
    video_timebase: Timebase
    frame_interval_ticks: int
    width: int
    height: int
    audio_rate: int
    tone_a: int
    tone_b: int
    palette_offset: int

    @property
    def model_duration_ticks(self) -> int:
        return MODEL_TIMEBASE.ticks_from_seconds(self.duration_seconds, "$.duration")

    @property
    def frame_count(self) -> int:
        return MODEL_TIMEBASE.convert_ticks(
            self.model_duration_ticks,
            self.video_timebase,
            "$.duration",
        ) // self.frame_interval_ticks

    @property
    def video_duration_ticks(self) -> int:
        return self.frame_count * self.frame_interval_ticks

    @property
    def audio_sample_count(self) -> int:
        return int(self.duration_seconds * self.audio_rate)

    @property
    def audio_duration_ticks(self) -> int:
        return self.audio_sample_count


GROUP_SPECS = (
    GroupSpec(
        sample_id="g01_neon_city",
        title="霓虹城市 · Neon City",
        duration_seconds=Fraction(3, 2),
        video_timebase=Timebase(1, 600),
        frame_interval_ticks=10,
        width=12,
        height=8,
        audio_rate=48000,
        tone_a=440,
        tone_b=660,
        palette_offset=11,
    ),
    GroupSpec(
        sample_id="g02_clockwork",
        title="Clockwork Garden",
        duration_seconds=Fraction(6, 5),
        video_timebase=Timebase(1, 1000),
        frame_interval_ticks=20,
        width=10,
        height=10,
        audio_rate=44100,
        tone_a=330,
        tone_b=495,
        palette_offset=47,
    ),
    GroupSpec(
        sample_id="g03_tidal",
        title="潮汐档案 · Tidal Archive",
        duration_seconds=Fraction(9, 5),
        video_timebase=Timebase(1, 600),
        frame_interval_ticks=10,
        width=14,
        height=8,
        audio_rate=48000,
        tone_a=392,
        tone_b=587,
        palette_offset=83,
    ),
)


CHAPTER_TITLES = (
    ("ch_intro", "开场 · Opening"),
    ("ch_develop", "发展 · Development"),
    ("ch_finale", "终章 · Finale"),
)


def _chapters(spec: GroupSpec) -> list[dict[str, Any]]:
    duration = spec.model_duration_ticks
    boundaries = (0, duration // 3, (duration * 2) // 3, duration)
    return [
        {
            "chapter_id": CHAPTER_TITLES[index][0],
            "number": index + 1,
            "start_ticks": boundaries[index],
            "end_ticks": boundaries[index + 1],
            "title": CHAPTER_TITLES[index][1],
            "tags": {"cue": f"act-{index + 1}", "energy": 20 + index * 15},
        }
        for index in range(3)
    ]


def _container_tags(spec: GroupSpec) -> dict[str, Any]:
    return {
        "studio": "North Grid Interactive",
        "rating": "E10",
        "locale": "zh-CN",
        "sample": spec.sample_id,
        "sequence": 1,
    }


def _make_png(spec: GroupSpec) -> bytes:
    width = 24
    height = 24
    rows = bytearray()
    for y in range(height):
        rows.append(0)
        for x in range(width):
            digest = hashlib.blake2s(
                f"{spec.sample_id}:{x}:{y}:{spec.palette_offset}".encode("utf-8"),
                digest_size=4,
            ).digest()
            rows.extend((digest[0], digest[1], digest[2]))

    def chunk(kind: bytes, payload: bytes) -> bytes:
        return (
            struct.pack(">I", len(payload))
            + kind
            + payload
            + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
        )

    def adler32(data: bytes) -> int:
        first = 1
        second = 0
        for byte in data:
            first = (first + byte) % 65521
            second = (second + first) % 65521
        return (second << 16) | first

    def stored_deflate(data: bytes) -> bytes:
        output = bytearray(b"\x78\x01")
        position = 0
        while position < len(data):
            block = data[position : position + 65535]
            final = 1 if position + len(block) == len(data) else 0
            output.append(final)
            output.extend(struct.pack("<H", len(block)))
            output.extend(struct.pack("<H", (~len(block)) & 0xFFFF))
            output.extend(block)
            position += len(block)
        output.extend(struct.pack(">I", adler32(data)))
        return bytes(output)

    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", stored_deflate(bytes(rows)))
        + chunk(b"IEND", b"")
    )


def _video_frames(spec: GroupSpec, seed: int) -> list[Frame]:
    frames: list[Frame] = []
    for frame_index in range(spec.frame_count):
        cells: list[str] = []
        for y in range(spec.height):
            for x in range(spec.width):
                digest = hashlib.blake2s(
                    (
                        f"{seed}:{spec.sample_id}:{frame_index}:{x}:{y}:"
                        f"{spec.palette_offset}"
                    ).encode("utf-8"),
                    digest_size=3,
                ).digest()
                cells.append(f"#{digest[0]:02x}{digest[1]:02x}{digest[2]:02x}")
        frames.append(
            Frame.from_values(
                pts_ticks=frame_index * spec.frame_interval_ticks,
                rows=spec.height,
                cols=spec.width,
                cells=cells,
            )
        )
    return frames


def _triangle(index: int, frequency: int, sample_rate: int, amplitude: int) -> int:
    phase = (index * frequency) % sample_rate
    half = sample_rate // 2
    if phase < half:
        return (4 * phase * amplitude) // sample_rate - amplitude
    return 3 * amplitude - (4 * phase * amplitude) // sample_rate


def _audio_bytes(spec: GroupSpec) -> bytes:
    samples = array("h")
    for index in range(spec.audio_sample_count):
        first = _triangle(index, spec.tone_a, spec.audio_rate, 12000)
        second = _triangle(index, spec.tone_b, spec.audio_rate, 6000)
        envelope = 1 if (index // 256) % 2 == 0 else 3
        value = (first * 2 + second) // 3
        if envelope == 3:
            value = (value * 3) // 4
        samples.append(max(-32768, min(32767, value)))
    if sys.byteorder != "little":
        samples.byteswap()
    return samples.tobytes()


def _video_stream(spec: GroupSpec, seed: int) -> tuple[dict[str, Any], bytes]:
    frames = _video_frames(spec, seed)
    descriptors = [frame.descriptor() for frame in frames]
    payload = canonical_json_bytes(descriptors)
    value = {
        "track_id": "video_main",
        "kind": "video",
        "language": "zh-CN",
        "codec": "grid-rgb-v1",
        "timebase": spec.video_timebase.to_dict(),
        "frame_interval_ticks": spec.frame_interval_ticks,
        "width": spec.width,
        "height": spec.height,
        "rows": spec.height,
        "cols": spec.width,
        "frames": descriptors,
    }
    return value, payload


def _audio_stream(spec: GroupSpec) -> tuple[dict[str, Any], bytes]:
    payload = _audio_bytes(spec)
    value = {
        "track_id": "audio_main",
        "kind": "audio",
        "language": "zh-CN",
        "codec": "pcm-s16le",
        "timebase": Timebase(1, spec.audio_rate).to_dict(),
        "sample_rate": spec.audio_rate,
        "channels": 1,
        "sample_format": "s16le",
        "sample_count": spec.audio_sample_count,
        "samples_sha256": sha256_bytes(payload),
        "samples_base64": encode_base64(payload),
    }
    return value, payload


def _track_core(
    spec: GroupSpec,
    track_id: str,
    kind: str,
    timebase: Timebase,
    sample_count: int,
    duration_ticks: int,
    payload_sha256: str,
    width: int | None = None,
    height: int | None = None,
    sample_rate: int | None = None,
) -> dict[str, Any]:
    value: dict[str, Any] = {
        "track_id": track_id,
        "kind": kind,
        "language": "zh-CN",
        "timebase": timebase.to_dict(),
        "sample_count": sample_count,
        "duration_ticks": duration_ticks,
        "payload_sha256": payload_sha256,
    }
    if kind == "video":
        value.update(
            {
                "width": width,
                "height": height,
                "frame_interval_ticks": spec.frame_interval_ticks,
            }
        )
    else:
        value.update({"sample_rate": sample_rate, "channels": 1})
    return value


def _metadata_v1(
    spec: GroupSpec,
    cover: bytes,
    tags: dict[str, Any],
    chapters: list[dict[str, Any]],
    video_payload_hash: str,
    audio_payload_hash: str,
) -> dict[str, Any]:
    video_duration_ms = spec.video_timebase.to_units(spec.video_duration_ticks, 1000, "$.video")
    audio_tb = Timebase(1, spec.audio_rate)
    audio_duration_ms = audio_tb.to_units(spec.audio_duration_ticks, 1000, "$.audio")
    return {
        "metadata_version": 1,
        "asset_id": spec.sample_id,
        "title": spec.title,
        "timeline_timebase": MODEL_TIMEBASE.to_dict(),
        "duration_ms": MODEL_TIMEBASE.to_units(
            spec.model_duration_ticks,
            1000,
            "$.duration_ms",
        ),
        "tracks": [
            {
                "track_id": "video_main",
                "type": "video",
                "language": "zh-CN",
                "timebase": spec.video_timebase.to_dict(),
                "duration_ms": video_duration_ms,
                "sample_count": spec.frame_count,
                "codec": "grid-rgb-v1",
                "video": {
                    "width": spec.width,
                    "height": spec.height,
                    "frame_interval_ticks": spec.frame_interval_ticks,
                },
            },
            {
                "track_id": "audio_main",
                "type": "audio",
                "language": "zh-CN",
                "timebase": audio_tb.to_dict(),
                "duration_ms": audio_duration_ms,
                "sample_count": spec.audio_sample_count,
                "codec": "pcm-s16le",
                "audio": {
                    "sample_rate": spec.audio_rate,
                    "channels": 1,
                    "sample_format": "s16le",
                },
            },
        ],
        "chapters": [
            {
                "chapter_id": chapter["chapter_id"],
                "number": chapter["number"],
                "start_ms": MODEL_TIMEBASE.to_units(chapter["start_ticks"], 1000, "$.chapters"),
                "end_ms": MODEL_TIMEBASE.to_units(chapter["end_ticks"], 1000, "$.chapters"),
                "label": chapter["title"],
            }
            for chapter in chapters
        ],
        "cover": {
            "mime": "image/png",
            "encoding": "base64",
            "data": encode_base64(cover),
        },
        "tags": dict(sorted(tags.items())),
        "_raw_payload_hashes": {
            "video_main": video_payload_hash,
            "audio_main": audio_payload_hash,
        },
    }


def _metadata_v2(
    spec: GroupSpec,
    cover: bytes,
    tags: dict[str, Any],
    chapters: list[dict[str, Any]],
) -> dict[str, Any]:
    return {
        "metadata_version": 2,
        "asset": {
            "asset_id": spec.sample_id,
            "title": spec.title,
        },
        "timeline": {
            "timebase": MODEL_TIMEBASE.to_dict(),
            "duration_ticks": spec.model_duration_ticks,
        },
        "tracks": [
            {
                "track_id": "video_main",
                "kind": "video",
                "language_tag": "zh-CN",
                "timebase": spec.video_timebase.to_dict(),
                "duration_ticks": spec.video_duration_ticks,
                "sample_count": spec.frame_count,
                "codec": "grid-rgb-v1",
                "roles": ["main", "picture"],
                "video": {
                    "width": spec.width,
                    "height": spec.height,
                    "frame_interval_ticks": spec.frame_interval_ticks,
                },
            },
            {
                "track_id": "audio_main",
                "kind": "audio",
                "language_tag": "zh-CN",
                "timebase": Timebase(1, spec.audio_rate).to_dict(),
                "duration_ticks": spec.audio_duration_ticks,
                "sample_count": spec.audio_sample_count,
                "codec": "pcm-s16le",
                "roles": ["main", "sound"],
                "audio": {
                    "sample_rate": spec.audio_rate,
                    "channels": 1,
                    "sample_format": "s16le",
                },
            },
        ],
        "chapters": {
            chapter["chapter_id"]: {
                "chapter_id": chapter["chapter_id"],
                "index": chapter["number"],
                "start_ticks": chapter["start_ticks"],
                "end_ticks": chapter["end_ticks"],
                "name": chapter["title"],
            }
            for chapter in chapters
        },
        "assets": {
            "cover": {
                "mime": "image/png",
                "base64": encode_base64(cover),
            }
        },
        "metadata": {
            "tags": dict(sorted(tags.items())),
        },
    }


def _metadata_v3(
    spec: GroupSpec,
    cover: bytes,
    tags: dict[str, Any],
    chapters: list[dict[str, Any]],
) -> dict[str, Any]:
    chapter_tb = Timebase(1, 600)
    return {
        "metadata_version": 3,
        "asset_id": spec.sample_id,
        "display_title": spec.title,
        "timeline": {
            "timebase": MODEL_TIMEBASE.to_dict(),
            "duration_us": MODEL_TIMEBASE.to_units(
                spec.model_duration_ticks,
                1_000_000,
                "$.timeline.duration_us",
            ),
        },
        "chapter_timebase": chapter_tb.to_dict(),
        "tracks": [
            {
                "track_id": "video_main",
                "kind": "video",
                "primary_language": "zh-CN",
                "languages": ["zh-CN", "en-US"],
                "time_base": spec.video_timebase.to_dict(),
                "duration_us": spec.video_timebase.to_units(
                    spec.video_duration_ticks,
                    1_000_000,
                    "$.tracks[0].duration_us",
                ),
                "sample_total": spec.frame_count,
                "codec": "grid-rgb-v1",
                "roles": ["main", "picture"],
                "hdr": "HDR10",
                "custom": {
                    "source": "synthetic-grid",
                    "palette": spec.palette_offset,
                },
                "video": {
                    "width": spec.width,
                    "height": spec.height,
                    "frame_interval_ticks": spec.frame_interval_ticks,
                },
            },
            {
                "track_id": "audio_main",
                "kind": "audio",
                "primary_language": "zh-CN",
                "languages": ["zh-CN", "en-US"],
                "time_base": Timebase(1, spec.audio_rate).to_dict(),
                "duration_us": Timebase(1, spec.audio_rate).to_units(
                    spec.audio_duration_ticks,
                    1_000_000,
                    "$.tracks[1].duration_us",
                ),
                "sample_total": spec.audio_sample_count,
                "codec": "pcm-s16le",
                "roles": ["main", "sound"],
                "custom": {"synthesis": "integer-triangle"},
                "audio": {
                    "sample_rate": spec.audio_rate,
                    "channels": 1,
                    "sample_format": "s16le",
                },
            },
        ],
        "chapter_table": [
            {
                "chapter_id": chapter["chapter_id"],
                "number": chapter["number"],
                "from_frame": MODEL_TIMEBASE.convert_ticks(
                    chapter["start_ticks"],
                    chapter_tb,
                    "$.chapter_table",
                ),
                "to_frame": MODEL_TIMEBASE.convert_ticks(
                    chapter["end_ticks"],
                    chapter_tb,
                    "$.chapter_table",
                ),
                "title": chapter["title"],
                "tags": dict(sorted(chapter["tags"].items())),
            }
            for chapter in chapters
        ],
        "resources": {
            "cover": {
                "mime": "image/png",
                "base64": encode_base64(cover),
            }
        },
        "custom_tags": [
            {"key": key, "value": value, "scope": "public"}
            for key, value in sorted(tags.items())
        ],
        "editorial": {
            "director": "Lin Qiao",
            "producer": "North Grid Interactive",
            "license": "synthetic-only",
        },
    }


def _truth_manifest(
    spec: GroupSpec,
    seed: int,
    cover: bytes,
    tags: dict[str, Any],
    chapters: list[dict[str, Any]],
    video_hash: str,
    audio_hash: str,
    file_hashes: dict[str, str],
) -> dict[str, Any]:
    value: dict[str, Any] = {
        "sample_id": spec.sample_id,
        "title": spec.title,
        "seed": seed,
        "generator_version": GENERATOR_VERSION,
        "timebase": MODEL_TIMEBASE.to_dict(),
        "duration_ticks": spec.model_duration_ticks,
        "duration_seconds": {
            "numerator": spec.duration_seconds.numerator,
            "denominator": spec.duration_seconds.denominator,
        },
        "tracks": [
            _track_core(
                spec,
                "video_main",
                "video",
                spec.video_timebase,
                spec.frame_count,
                spec.video_duration_ticks,
                video_hash,
                width=spec.width,
                height=spec.height,
            ),
            _track_core(
                spec,
                "audio_main",
                "audio",
                Timebase(1, spec.audio_rate),
                spec.audio_sample_count,
                spec.audio_duration_ticks,
                audio_hash,
                sample_rate=spec.audio_rate,
            ),
        ],
        "chapters": [
            {
                "chapter_id": chapter["chapter_id"],
                "number": chapter["number"],
                "start_ticks": chapter["start_ticks"],
                "end_ticks": chapter["end_ticks"],
                "title": chapter["title"],
            }
            for chapter in chapters
        ],
        "tags": dict(sorted(tags.items())),
        "cover_sha256": sha256_bytes(cover),
        "metadata_versions": ["v1", "v2", "v3"],
        "files": dict(sorted(file_hashes.items())),
    }
    value["fingerprint"] = compute_truth_fingerprint(value)
    return value


def _write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(canonical_json_bytes(value) + b"\n")


def _file_hash(path: Path) -> str:
    return sha256_bytes(path.read_bytes())


def _injection_files(root: Path) -> None:
    values = {
        "conflict.json": {
            "metadata_conflict": {"track_id": "video_main", "language": "ja-JP"},
            "parameter_version": "inject-conflict-p1",
        },
        "order.json": {
            "timestamp_inversion": {"track_id": "video_main", "frame_index": 2},
            "parameter_version": "inject-order-p1",
        },
        "missing_track.json": {
            "missing_track": {"track_id": "audio_main"},
            "parameter_version": "inject-missing-p1",
        },
        "corrupt_segment.json": {
            "corrupt_segment": {"track_id": "audio_main", "byte_offset": 73},
            "parameter_version": "inject-corrupt-p1",
        },
    }
    for name, value in values.items():
        _write_json(root / "injections" / name, value)


def _corrupt_raw_fragment(spec: GroupSpec, root: Path) -> None:
    _, payload = _audio_stream(spec)
    raw_bytes = bytearray(encode_base64(payload).encode("ascii"))
    position = len(raw_bytes) // 2
    raw_bytes[position] = ord("A") if raw_bytes[position] != ord("A") else ord("B")
    value = dict(raw_bytes=raw_bytes.decode("ascii"), offset=position)
    _write_json(root / "anomalies" / "corrupt_audio_fragment.json", value)


def _anomaly_readme(root: Path) -> None:
    text = """# 损坏分片样本

`corrupt_audio_fragment.json` 是 `g01_neon_city` 音频样本 base64 片段的中位字节翻转副本，字段：

- `raw_bytes`：已损坏的 base64 字符片段；
- `offset`：第一次翻转的字符位置。

完整的四类可执行异常通过 `samples/injections/*.json` 参数注入：

- `conflict.json` -> `E_CONFLICT`
- `order.json` -> `E_ORDER`
- `missing_track.json` -> `E_TRACK`
- `corrupt_segment.json` -> 导入阶段 `E_CORRUPT`，分段封装阶段 `E_CORRUPT` 且仅目标段失败
"""
    path = root / "anomalies" / "README.md"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")


def generate_samples(
    output: str | Path,
    *,
    seed: int = DEFAULT_SEED,
    clean: bool = True,
) -> dict[str, Any]:
    root = Path(output)
    if clean and root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)
    _injection_files(root)
    index_groups: list[dict[str, Any]] = []
    for group_index, spec in enumerate(GROUP_SPECS):
        group_seed = seed + group_index * 1009
        group = root / spec.sample_id
        chapters = _chapters(spec)
        tags = _container_tags(spec)
        cover = _make_png(spec)
        video_value, video_payload = _video_stream(spec, group_seed)
        audio_value, audio_payload = _audio_stream(spec)
        video_hash = sha256_bytes(video_payload)
        audio_hash = sha256_bytes(audio_payload)
        _write_json(group / "metadata" / "meta_v1.json", _metadata_v1(spec, cover, tags, chapters, video_hash, audio_hash))
        _write_json(group / "metadata" / "meta_v2.json", _metadata_v2(spec, cover, tags, chapters))
        _write_json(group / "metadata" / "meta_v3.json", _metadata_v3(spec, cover, tags, chapters))
        _write_json(group / "raw" / "video_main.json", video_value)
        _write_json(group / "raw" / "audio_main.json", audio_value)
        metadata_hashes = {
            f"metadata/meta_v{version}.json": _file_hash(group / "metadata" / f"meta_v{version}.json")
            for version in (1, 2, 3)
        }
        raw_hashes = {
            "raw/video_main.json": _file_hash(group / "raw" / "video_main.json"),
            "raw/audio_main.json": _file_hash(group / "raw" / "audio_main.json"),
        }
        file_hashes = {**metadata_hashes, **raw_hashes}
        truth = _truth_manifest(
            spec,
            group_seed,
            cover,
            tags,
            chapters,
            video_hash,
            audio_hash,
            file_hashes,
        )
        _write_json(group / "truth.json", truth)
        index_groups.append(
            {
                "sample_id": spec.sample_id,
                "title": spec.title,
                "truth_fingerprint": truth["fingerprint"],
                "track_count": 2,
                "frame_count": spec.frame_count,
                "audio_sample_count": spec.audio_sample_count,
                "duration_ticks": spec.model_duration_ticks,
            }
        )
    _corrupt_raw_fragment(GROUP_SPECS[0], root)
    _anomaly_readme(root)
    index = {
        "generator_version": GENERATOR_VERSION,
        "seed": seed,
        "group_count": len(index_groups),
        "groups": index_groups,
        "metadata_versions": ["v1", "v2", "v3"],
    }
    index["index_fingerprint"] = sha256_bytes(canonical_json_bytes(index))
    _write_json(root / "index.json", index)
    return index


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic multimedia workbench samples")
    parser.add_argument("--out", default="samples", help="output samples directory")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED, help="deterministic seed")
    parser.add_argument("--keep", action="store_true", help="keep existing files before generation")
    args = parser.parse_args(argv)
    index = generate_samples(args.out, seed=args.seed, clean=not args.keep)
    print(json.dumps(index, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
