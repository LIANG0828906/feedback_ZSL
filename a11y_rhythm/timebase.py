"""WAV, timecode and frame-table normalization to one integer sample base."""

from __future__ import annotations

import bisect
import json
import re
import struct
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

from .errors import E_TIMEBASE_INVALID, RhythmError
from .models import SourceBundle, SourceItem


TIMECODE_RE = re.compile(r"^(\d+):([0-5]\d):([0-5]\d)(?:[.,](\d{1,9}))?$")
FRAME_MAGIC = b"FTB1"
FRAME_VERSION = 1
FRAME_HEADER = struct.Struct("<4sHIIIIHH")
FRAME_ENTRY = struct.Struct("<QII")


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    sample_width: int
    sample_count: int
    data_offset: int
    data_size: int


@dataclass(frozen=True)
class FrameTable:
    sample_rate: int
    fps_num: int
    fps_den: int
    samples: tuple[int, ...]
    flags: tuple[int, ...]

    @property
    def frame_count(self) -> int:
        return len(self.samples)

    @property
    def duration_samples(self) -> int:
        return round_fraction(Fraction(self.frame_count * self.sample_rate * self.fps_den, self.fps_num))

    def frame_range(self, start: int, end: int) -> tuple[int, int]:
        if end <= start:
            return (0, 0)
        frame_start = bisect.bisect_right(self.samples, start) - 1
        if frame_start < 0:
            frame_start = 0
        frame_end = bisect.bisect_left(self.samples, end)
        if frame_end <= frame_start:
            frame_end = min(frame_start + 1, len(self.samples))
        return frame_start, frame_end


def round_fraction(value: Fraction) -> int:
    if value >= 0:
        return (value.numerator * 2 + value.denominator) // (value.denominator * 2)
    positive = -value
    return -((positive.numerator * 2 + positive.denominator) // (positive.denominator * 2))


def parse_timecode(value: Any, sample_rate: int, item_id: str, field: str) -> int:
    if not isinstance(value, str):
        raise RhythmError(
            E_TIMEBASE_INVALID,
            f"{field} must be an HH:MM:SS.mmm string",
            item_id,
            {"field": field, "value": value},
        )
    match = TIMECODE_RE.match(value)
    if not match:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            f"{field} is not a valid timecode",
            item_id,
            {"field": field, "value": value},
        )
    hours, minutes, seconds, fraction = match.groups()
    fraction_value = Fraction(int(fraction), 10 ** len(fraction)) if fraction else Fraction(0)
    total_seconds = int(hours) * 3600 + int(minutes) * 60 + int(seconds) + fraction_value
    return round_fraction(total_seconds * sample_rate)


def parse_wav_info(path: str | Path) -> WavInfo:
    file_path = Path(path)
    try:
        data = file_path.read_bytes()
    except OSError as exc:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "WAV file cannot be read",
            "wav",
            {"path": str(file_path), "error": str(exc)},
        ) from exc
    if len(data) < 12 or data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        raise RhythmError(E_TIMEBASE_INVALID, "not a RIFF/WAVE file", "wav", {"path": str(file_path)})
    offset = 12
    fmt: tuple[int, int, int, int] | None = None
    data_offset = -1
    data_size = -1
    while offset + 8 <= len(data):
        chunk_id = data[offset : offset + 4]
        chunk_size = struct.unpack_from("<I", data, offset + 4)[0]
        body = offset + 8
        if body + chunk_size > len(data):
            raise RhythmError(
                E_TIMEBASE_INVALID,
                "truncated WAV chunk",
                "wav",
                {"chunk": chunk_id.decode("latin1"), "declared": chunk_size, "available": len(data) - body},
            )
        if chunk_id == b"fmt ":
            if chunk_size < 16:
                raise RhythmError(E_TIMEBASE_INVALID, "short WAV fmt chunk", "wav", {"size": chunk_size})
            audio_format, channels, sample_rate, _byte_rate, block_align, bits = struct.unpack_from(
                "<HHIIHH", data, body
            )
            if audio_format == 0xFFFE:
                if chunk_size < 40:
                    raise RhythmError(E_TIMEBASE_INVALID, "short WAVE_FORMAT_EXTENSIBLE", "wav")
                subformat = data[body + 24 : body + 40]
                if subformat[:2] != b"\x01\x00":
                    raise RhythmError(
                        E_TIMEBASE_INVALID,
                        "WAV extensible subformat is not PCM",
                        "wav",
                        {"subformat": subformat.hex()},
                    )
            elif audio_format != 1:
                raise RhythmError(
                    E_TIMEBASE_INVALID,
                    "WAV is not integer PCM",
                    "wav",
                    {"audio_format": audio_format},
                )
            fmt = (channels, sample_rate, bits, block_align)
        elif chunk_id == b"data":
            data_offset = body
            data_size = chunk_size
        offset = body + chunk_size + (chunk_size & 1)
    if fmt is None or data_offset < 0:
        raise RhythmError(E_TIMEBASE_INVALID, "WAV is missing fmt or data chunk", "wav")
    channels, sample_rate, bits, block_align = fmt
    if bits != 16:
        raise RhythmError(E_TIMEBASE_INVALID, "WAV must be 16-bit PCM", "wav", {"bits": bits})
    if channels not in (1, 6):
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "WAV must be mono or 5.1 (six channels)",
            "wav",
            {"channels": channels},
        )
    if sample_rate != 48_000:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "WAV sample rate must be 48000 Hz",
            "wav",
            {"sample_rate": sample_rate},
        )
    if block_align != channels * 2 or data_size % block_align:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "WAV data size does not align to PCM frames",
            "wav",
            {"block_align": block_align, "data_size": data_size},
        )
    return WavInfo(
        sample_rate=sample_rate,
        channels=channels,
        sample_width=2,
        sample_count=data_size // block_align,
        data_offset=data_offset,
        data_size=data_size,
    )


def parse_frame_table(path: str | Path) -> FrameTable:
    file_path = Path(path)
    try:
        data = file_path.read_bytes()
    except OSError as exc:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "frame table cannot be read",
            "frame_table",
            {"path": str(file_path), "error": str(exc)},
        ) from exc
    if len(data) < FRAME_HEADER.size:
        raise RhythmError(E_TIMEBASE_INVALID, "frame table header is truncated", "frame_table")
    magic, version, sample_rate, frame_count, fps_num, fps_den, stride, _flags = FRAME_HEADER.unpack_from(data)
    if magic != FRAME_MAGIC or version != FRAME_VERSION:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "frame table magic/version is invalid",
            "frame_table",
            {"magic": magic.decode("latin1"), "version": version},
        )
    if stride != FRAME_ENTRY.size:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "frame table entry stride is invalid",
            "frame_table",
            {"header_stride": stride, "expected": FRAME_ENTRY.size},
        )
    if fps_num <= 0 or fps_den <= 0 or sample_rate <= 0 or frame_count <= 0:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "frame table header has non-positive values",
            "frame_table",
            {
                "sample_rate": sample_rate,
                "fps_num": fps_num,
                "fps_den": fps_den,
                "frame_count": frame_count,
            },
        )
    expected_size = FRAME_HEADER.size + frame_count * stride
    if len(data) != expected_size:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "frame table body length contradicts header frame count",
            "frame_table",
            {
                "header_frame_count": frame_count,
                "expected_bytes": expected_size,
                "actual_bytes": len(data),
                "first_conflict": "frame_table",
            },
        )
    samples: list[int] = []
    flags: list[int] = []
    previous = -1
    for index in range(frame_count):
        sample, frame_index, frame_flags = FRAME_ENTRY.unpack_from(
            data, FRAME_HEADER.size + index * stride
        )
        if frame_index != index:
            raise RhythmError(
                E_TIMEBASE_INVALID,
                "frame index is not contiguous",
                "frame_table",
                {"offset": index, "frame_index": frame_index},
            )
        if sample <= previous:
            raise RhythmError(
                E_TIMEBASE_INVALID,
                "frame sample offsets are not strictly increasing",
                "frame_table",
                {"offset": index, "sample": sample, "previous": previous},
            )
        expected_sample = round_fraction(
            Fraction(index * sample_rate * fps_den, fps_num)
        )
        if sample != expected_sample:
            raise RhythmError(
                E_TIMEBASE_INVALID,
                "frame sample offset contradicts the header timebase",
                "frame_table",
                {
                    "offset": index,
                    "sample": sample,
                    "expected_sample": expected_sample,
                    "fps_num": fps_num,
                    "fps_den": fps_den,
                },
            )
        samples.append(sample)
        flags.append(frame_flags)
        previous = sample
    if samples and samples[0] != 0:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "first frame must start at sample 0",
            "frame_table",
            {"first_sample": samples[0]},
        )
    return FrameTable(
        sample_rate=sample_rate,
        fps_num=fps_num,
        fps_den=fps_den,
        samples=tuple(samples),
        flags=tuple(flags),
    )


def _rate_fraction(value: Any, item_id: str, field: str) -> Fraction:
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        raise RhythmError(
            E_TIMEBASE_INVALID,
            f"{field} must be a positive reading rate",
            item_id,
            {"field": field, "value": value},
        )
    try:
        result = Fraction(str(value))
    except (ValueError, ZeroDivisionError) as exc:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            f"{field} is not numeric",
            item_id,
            {"field": field, "value": value},
        ) from exc
    if result <= 0:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            f"{field} must be positive",
            item_id,
            {"field": field, "value": value},
        )
    return result


def _coerce_int(value: Any, item_id: str, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise RhythmError(
            E_TIMEBASE_INVALID,
            f"{field} must be an integer sample value",
            item_id,
            {"field": field, "value": value},
        )
    return value


def _load_json(path: str | Path) -> dict[str, Any]:
    try:
        with open(path, "r", encoding="utf-8") as fh:
            value = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "subtitle JSON cannot be parsed",
            "json",
            {"path": str(path), "error": str(exc)},
        ) from exc
    if not isinstance(value, dict):
        raise RhythmError(E_TIMEBASE_INVALID, "subtitle JSON root must be an object", "json")
    return value


def load_source_bundle(
    source_path: str | Path,
    wav_path: str | Path | None = None,
    frame_table_path: str | Path | None = None,
) -> tuple[SourceBundle, WavInfo, FrameTable]:
    document = _load_json(source_path)
    wav_name = str(wav_path or document.get("wav", ""))
    frame_name = str(frame_table_path or document.get("frame_table", ""))
    if not wav_name:
        raise RhythmError(E_TIMEBASE_INVALID, "WAV path is missing", "wav")
    if not frame_name:
        raise RhythmError(E_TIMEBASE_INVALID, "frame table path is missing", "frame_table")
    source_dir = Path(source_path).resolve().parent

    def resolve_media_path(value: str) -> str:
        candidate = Path(value)
        if not candidate.is_absolute():
            candidate = source_dir / candidate
        return str(candidate.resolve())

    wav_name = resolve_media_path(wav_name)
    frame_name = resolve_media_path(frame_name)
    wav_info = parse_wav_info(wav_name)
    frame_table = parse_frame_table(frame_name)
    sample_rate = document.get("sample_rate")
    if not isinstance(sample_rate, int) or sample_rate != wav_info.sample_rate:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "JSON and WAV sample rates disagree",
            "json",
            {
                "json_sample_rate": sample_rate,
                "wav_sample_rate": wav_info.sample_rate,
                "first_conflict": "sample_rate",
            },
        )
    if frame_table.sample_rate != sample_rate:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "frame table and WAV sample rates disagree",
            "frame_table",
            {
                "frame_table_sample_rate": frame_table.sample_rate,
                "wav_sample_rate": sample_rate,
                "first_conflict": "frame_table.sample_rate",
            },
        )
    material_id = document.get("material_id")
    if not isinstance(material_id, str) or not material_id:
        raise RhythmError(E_TIMEBASE_INVALID, "material_id is missing", "json")
    params_version = document.get("params_version", "1")
    if not isinstance(params_version, str):
        raise RhythmError(E_TIMEBASE_INVALID, "params_version must be a string", "json")
    max_iterations = document.get("max_iterations", 256)
    if not isinstance(max_iterations, int) or max_iterations < 0:
        raise RhythmError(E_TIMEBASE_INVALID, "max_iterations must be non-negative", "json")
    coverage_limit = min(wav_info.sample_count, frame_table.duration_samples)
    items: list[SourceItem] = []
    seen: set[str] = set()
    for kind, key in (("speech", "speech"), ("description", "descriptions")):
        entries = document.get(key, [])
        if not isinstance(entries, list):
            raise RhythmError(E_TIMEBASE_INVALID, f"{key} must be a list", key)
        for index, entry in enumerate(entries):
            fallback_id = f"{kind}-{index + 1}"
            if not isinstance(entry, dict):
                raise RhythmError(E_TIMEBASE_INVALID, "sentence entry must be an object", fallback_id)
            item_id = entry.get("id", fallback_id)
            if not isinstance(item_id, str) or not item_id:
                raise RhythmError(E_TIMEBASE_INVALID, "sentence id is missing", fallback_id)
            if item_id in seen:
                raise RhythmError(E_TIMEBASE_INVALID, "duplicate sentence id", item_id, {"id": item_id})
            seen.add(item_id)
            text = entry.get("text")
            if not isinstance(text, str) or not text.strip():
                raise RhythmError(E_TIMEBASE_INVALID, "text is missing", item_id, {"field": "text"})
            speaker = entry.get("speaker", "DESCRIPTION" if kind == "description" else "")
            if not isinstance(speaker, str) or not speaker.strip():
                raise RhythmError(E_TIMEBASE_INVALID, "speaker is missing", item_id, {"field": "speaker"})
            if "start_sample" in entry or "end_sample" in entry:
                if "start_sample" not in entry or "end_sample" not in entry:
                    raise RhythmError(
                        E_TIMEBASE_INVALID,
                        "both start_sample and end_sample are required",
                        item_id,
                    )
                start = _coerce_int(entry["start_sample"], item_id, "start_sample")
                end = _coerce_int(entry["end_sample"], item_id, "end_sample")
            else:
                start = parse_timecode(entry.get("start"), sample_rate, item_id, "start")
                end = parse_timecode(entry.get("end"), sample_rate, item_id, "end")
            if start < 0 or end <= start:
                raise RhythmError(
                    E_TIMEBASE_INVALID,
                    "sentence sample interval is empty or negative",
                    item_id,
                    {"start_sample": start, "end_sample": end},
                )
            if end > coverage_limit:
                raise RhythmError(
                    E_TIMEBASE_INVALID,
                    "sentence timecode exceeds WAV/frame-table coverage",
                    item_id,
                    {
                        "start_sample": start,
                        "end_sample": end,
                        "coverage_limit": coverage_limit,
                        "first_conflict": item_id,
                    },
                )
            max_cps = _rate_fraction(
                entry.get("max_reading_rate_cps", entry.get("reading_rate_cps")),
                item_id,
                "max_reading_rate_cps",
            )
            source_rate = _rate_fraction(
                entry.get("estimated_reading_rate_cps", entry.get("reading_rate_cps", max_cps)),
                item_id,
                "estimated_reading_rate_cps",
            )
            priority = entry.get("priority", 50)
            if isinstance(priority, bool) or not isinstance(priority, int):
                raise RhythmError(E_TIMEBASE_INVALID, "priority must be an integer", item_id)
            min_gap_ms = entry.get("min_gap_ms", 0)
            if isinstance(min_gap_ms, bool) or not isinstance(min_gap_ms, (int, float)) or min_gap_ms < 0:
                raise RhythmError(E_TIMEBASE_INVALID, "min_gap_ms must be non-negative", item_id)
            min_gap_samples = round_fraction(Fraction(str(min_gap_ms)) * sample_rate / 1000)
            items.append(
                SourceItem(
                    item_id=item_id,
                    kind=kind,  # type: ignore[arg-type]
                    text=text,
                    start_sample=start,
                    end_sample=end,
                    speaker=speaker,
                    priority=priority,
                    max_cps=max_cps,
                    source_rate_cps=source_rate,
                    min_gap_samples=min_gap_samples,
                )
            )
    if not any(item.kind == "speech" for item in items):
        raise RhythmError(E_TIMEBASE_INVALID, "at least one speech sentence is required", "speech")
    bundle = SourceBundle(
        material_id=material_id,
        sample_rate=sample_rate,
        frame_table_path=frame_name,
        wav_path=wav_name,
        items=tuple(items),
        params_version=params_version,
        min_readable_ratio=Fraction(str(document.get("min_readable_ratio", "1"))),
        max_iterations=max_iterations,
    )
    return bundle, wav_info, frame_table


def validate_timebase(bundle: SourceBundle) -> tuple[WavInfo, FrameTable]:
    wav_info = parse_wav_info(bundle.wav_path)
    frame_table = parse_frame_table(bundle.frame_table_path)
    if bundle.sample_rate != wav_info.sample_rate:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "bundle and WAV sample rates disagree",
            "wav",
            {"bundle": bundle.sample_rate, "wav": wav_info.sample_rate},
        )
    if frame_table.sample_rate != bundle.sample_rate:
        raise RhythmError(
            E_TIMEBASE_INVALID,
            "bundle and frame table sample rates disagree",
            "frame_table",
            {"bundle": bundle.sample_rate, "frame_table": frame_table.sample_rate},
        )
    coverage_limit = min(wav_info.sample_count, frame_table.duration_samples)
    for item in bundle.items:
        if item.end_sample > coverage_limit:
            raise RhythmError(
                E_TIMEBASE_INVALID,
                "sentence is out of timebase coverage",
                item.item_id,
                {"end_sample": item.end_sample, "coverage_limit": coverage_limit},
            )
    return wav_info, frame_table
