"""Integer time-base normalization for subtitles, samples and frame tables."""

from __future__ import annotations

import bisect
import re
import struct
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

from .canonical import sha256_bytes
from .errors import LCFTError, timebase_error
from .wav import WavInfo, inspect_wav

FRAME_MAGIC = b"LCFT"
FRAME_VERSION_LEGACY = 1
FRAME_VERSION = 2


def _round_half_up(value: Fraction) -> int:
    if value < 0:
        return -_round_half_up(-value)
    return (value.numerator * 2 + value.denominator) // (value.denominator * 2)


def _ceil_fraction(value: Fraction) -> int:
    return -((-value.numerator) // value.denominator)


@dataclass(frozen=True)
class FrameTable:
    path: str
    sample_rate: int
    frame_rate: Fraction
    declared_frame_count: int
    timestamps: tuple[int, ...]
    version: int
    sha256: str

    @property
    def frame_count(self) -> int:
        return len(self.timestamps)

    def frame_index_at_or_before(self, sample: int) -> int:
        if not self.timestamps:
            return 0
        return max(0, bisect.bisect_right(self.timestamps, sample) - 1)

    def frame_interval(self, start_sample: int, end_sample: int) -> tuple[int, int]:
        """Map [start,end) samples to [start_frame,end_frame)."""

        start_frame = bisect.bisect_right(self.timestamps, start_sample) - 1
        if start_frame < 0:
            start_frame = 0
        end_frame = bisect.bisect_left(self.timestamps, end_sample)
        if end_frame < start_frame:
            end_frame = start_frame
        return start_frame, end_frame

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "sample_rate": self.sample_rate,
            "frame_rate": format_fraction(self.frame_rate),
            "frame_count": self.frame_count,
            "declared_frame_count": self.declared_frame_count,
            "version": self.version,
            "sha256": self.sha256,
        }


@dataclass(frozen=True)
class Timebase:
    """Canonical tick is one audio sample at 48 kHz."""

    sample_rate: int = 48_000
    ticks_per_second: int = 48_000

    def seconds_to_ticks(self, value: str | int | Fraction, *, rounding: str = "round") -> int:
        fraction = Fraction(value) * self.ticks_per_second
        if rounding == "floor":
            return fraction.numerator // fraction.denominator
        if rounding == "ceil":
            return _ceil_fraction(fraction)
        return _round_half_up(fraction)

    def milliseconds_to_ticks(self, value: str | int | Fraction) -> int:
        return _round_half_up(Fraction(value) * self.ticks_per_second / 1000)

    def source_ticks_to_samples(
        self, value: int, source_ticks_per_second: int, *, field: str
    ) -> int:
        if source_ticks_per_second <= 0:
            raise timebase_error(
                field,
                "source ticks_per_second must be positive",
                ticks_per_second=source_ticks_per_second,
            )
        if source_ticks_per_second == self.ticks_per_second:
            return value
        return _round_half_up(
            Fraction(value) * self.ticks_per_second / source_ticks_per_second
        )

    def to_timecode(self, sample: int) -> str:
        if sample < 0:
            sign = "-"
            sample = -sample
        else:
            sign = ""
        hours, remainder = divmod(sample, self.sample_rate * 3600)
        minutes, remainder = divmod(remainder, self.sample_rate * 60)
        seconds, samples = divmod(remainder, self.sample_rate)
        millis = _round_half_up(Fraction(samples * 1000, self.sample_rate))
        if millis == 1000:
            seconds += 1
            millis = 0
        if seconds == 60:
            minutes += 1
            seconds = 0
        if minutes == 60:
            hours += 1
            minutes = 0
        return f"{sign}{hours:02d}:{minutes:02d}:{seconds:02d}.{millis:03d}"


@dataclass(frozen=True)
class NormalizedSubtitle:
    id: str
    text: str
    speaker: str
    start_sample: int
    end_sample: int
    frame_start: int
    frame_end: int
    max_read_speed: str | None
    priority: int | None

    def to_dict(self, timebase: Timebase) -> dict[str, Any]:
        result: dict[str, Any] = {
            "id": self.id,
            "text": self.text,
            "speaker": self.speaker,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "start_ticks": self.start_sample,
            "end_ticks": self.end_sample,
            "start_timecode": timebase.to_timecode(self.start_sample),
            "end_timecode": timebase.to_timecode(self.end_sample),
            "frame_start": self.frame_start,
            "frame_end": self.frame_end,
            "frame_interval": [self.frame_start, self.frame_end],
        }
        if self.max_read_speed is not None:
            result["max_read_speed"] = self.max_read_speed
        if self.priority is not None:
            result["priority"] = self.priority
        return result


@dataclass(frozen=True)
class TimebaseResult:
    audio: WavInfo
    frames: FrameTable
    timebase: Timebase
    tracks: dict[str, tuple[NormalizedSubtitle, ...]]
    descriptions: tuple[NormalizedSubtitle, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "timebase": {
                "unit": "sample",
                "sample_rate": self.timebase.sample_rate,
                "ticks_per_second": self.timebase.ticks_per_second,
            },
            "audio": self.audio.to_dict(),
            "frame_table": self.frames.to_dict(),
            "tracks": {
                language: [item.to_dict(self.timebase) for item in sentences]
                for language, sentences in sorted(self.tracks.items())
            },
            "descriptions": [
                item.to_dict(self.timebase) for item in self.descriptions
            ],
        }


def format_fraction(value: Fraction) -> str:
    if value.denominator == 1:
        return str(value.numerator)
    return f"{value.numerator}/{value.denominator}"


def parse_fraction(value: Any, *, field: str) -> Fraction:
    try:
        if isinstance(value, bool):
            raise ValueError
        if isinstance(value, Fraction):
            return value
        if isinstance(value, (int, str)):
            return Fraction(value)
        if isinstance(value, float):
            return Fraction(str(value))
    except (ValueError, ZeroDivisionError) as exc:
        raise timebase_error(field, "invalid rational value", value=repr(value)) from exc
    raise timebase_error(field, "invalid rational value", value=repr(value))


def parse_timecode(
    value: str,
    timebase: Timebase,
    frame_rate: Fraction,
    *,
    field: str,
) -> int:
    text = value.strip()
    if not text:
        raise timebase_error(field, "empty timecode", value=value)
    if text.startswith("-"):
        raise timebase_error(field, "negative timecode is not allowed", value=value)

    frame_style = bool(re.fullmatch(r"\d+:\d{1,2}:\d{1,2}:\d{1,3}", text))
    try:
        if frame_style:
            hours_text, minutes_text, seconds_text, frames_text = text.split(":")
            if frame_rate.denominator != 1:
                raise timebase_error(
                    field,
                    "frame timecode requires an integer frame rate",
                    frame_rate=format_fraction(frame_rate),
                )
            fps = frame_rate.numerator
            hours, minutes, seconds, frames = (
                int(hours_text),
                int(minutes_text),
                int(seconds_text),
                int(frames_text),
            )
            if minutes >= 60 or seconds >= 60 or frames >= fps:
                raise timebase_error(
                    field,
                    "timecode component out of range",
                    value=value,
                    frame_rate=fps,
                )
            total_seconds = Fraction(hours * 3600 + minutes * 60 + seconds) + Fraction(
                frames, fps
            )
            return _round_half_up(total_seconds * timebase.ticks_per_second)

        parts = text.split(":")
        if len(parts) == 1:
            seconds_value = Fraction(parts[0])
        elif len(parts) == 2:
            minutes = Fraction(parts[0])
            seconds = Fraction(parts[1])
            if minutes < 0 or seconds < 0 or seconds >= 60:
                raise timebase_error(
                    field,
                    "timecode component out of range",
                    value=value,
                )
            seconds_value = minutes * 60 + seconds
        elif len(parts) == 3:
            hours = Fraction(parts[0])
            minutes = Fraction(parts[1])
            seconds = Fraction(parts[2])
            if (
                hours < 0
                or minutes < 0
                or seconds < 0
                or minutes >= 60
                or seconds >= 60
            ):
                raise timebase_error(
                    field,
                    "timecode component out of range",
                    value=value,
                )
            seconds_value = hours * 3600 + minutes * 60 + seconds
        else:
            raise ValueError
        if seconds_value < 0:
            raise ValueError
        return _round_half_up(seconds_value * timebase.ticks_per_second)
    except LCFTError:
        raise
    except (ValueError, ZeroDivisionError) as exc:
        raise timebase_error(field, "invalid timecode", value=value) from exc


def parse_time_value(
    value: Any,
    timebase: Timebase,
    frame_rate: Fraction,
    *,
    field: str,
    default_unit: str = "samples",
) -> int:
    """Parse an integer source time value into canonical samples.

    Integer values use ``default_unit``.  Strings may be a timecode, a decimal
    second value, or a number followed by ``s``/``ms``/``samples``/``ticks``.
    Floats are rejected so JSON round trips cannot introduce platform-dependent
    decimal behavior.
    """

    unit = default_unit
    raw = value
    if isinstance(value, dict):
        if "value" not in value:
            raise timebase_error(field, "time object lacks value", value=repr(value))
        raw = value["value"]
        unit = str(value.get("unit", default_unit))

    if isinstance(raw, bool) or isinstance(raw, float):
        raise timebase_error(
            field,
            "time values must be integers, rational strings or timecodes",
            value=repr(raw),
        )
    if isinstance(raw, int):
        if unit in ("sample", "samples", "tick", "ticks"):
            return raw
        if unit in ("s", "second", "seconds"):
            return _round_half_up(Fraction(raw) * timebase.ticks_per_second)
        if unit in ("ms", "millisecond", "milliseconds"):
            return _round_half_up(Fraction(raw) * timebase.ticks_per_second / 1000)
        if unit in ("us", "microsecond", "microseconds"):
            return _round_half_up(Fraction(raw) * timebase.ticks_per_second / 1_000_000)
        raise timebase_error(field, "unknown time unit", unit=unit)
    if not isinstance(raw, str):
        raise timebase_error(field, "unsupported time value", value=repr(raw))

    stripped = raw.strip()
    lower = stripped.lower()
    for suffix in ("samples", "sample", "ticks", "tick", "ms", "us", "s"):
        if lower.endswith(suffix):
            number = stripped[: -len(suffix)].strip()
            try:
                integer = int(number)
            except ValueError:
                break
            if suffix.startswith("tick") or suffix.startswith("sample"):
                return integer
            if suffix == "ms":
                return _round_half_up(Fraction(integer) * timebase.ticks_per_second / 1000)
            if suffix == "us":
                return _round_half_up(
                    Fraction(integer) * timebase.ticks_per_second / 1_000_000
                )
            return _round_half_up(Fraction(integer) * timebase.ticks_per_second)

    if ":" in stripped or "." in stripped or "/" in stripped:
        return parse_timecode(stripped, timebase, frame_rate, field=field)
    try:
        integer = int(stripped)
    except ValueError as exc:
        raise timebase_error(field, "invalid time value", value=raw) from exc
    return parse_time_value(
        integer, timebase, frame_rate, field=field, default_unit=unit
    )


def load_frame_table(
    path: str | Path,
    *,
    total_samples: int | None = None,
    expected_sample_rate: int | None = None,
    expected_frame_rate: Any = None,
    expected_frame_count: int | None = None,
) -> FrameTable:
    target = Path(path)
    try:
        data = target.read_bytes()
    except OSError as exc:
        raise timebase_error(
            "frame_table.path",
            "cannot read frame timetable",
            path=str(target),
            reason=str(exc),
        ) from exc
    if len(data) < 6:
        raise timebase_error(
            "frame_table.header",
            "frame timetable is shorter than its magic/version header",
            path=str(target),
            size=len(data),
        )
    if data[:4] != FRAME_MAGIC:
        raise timebase_error(
            "frame_table.magic",
            "frame timetable magic must be LCFT",
            actual=data[:4].hex(),
        )
    version = struct.unpack_from("<H", data, 4)[0]
    if version == FRAME_VERSION_LEGACY:
        header = struct.Struct("<4sHIIHH")
        header_size = header.size
        if len(data) < header_size:
            raise timebase_error("frame_table.header", "truncated v1 frame header")
        _, _, sample_rate, fps_num, frame_count, _flags = header.unpack_from(data)
        frame_rate = Fraction(fps_num, 1)
    elif version == FRAME_VERSION:
        header = struct.Struct("<4sHIIII")
        header_size = header.size
        if len(data) < header_size:
            raise timebase_error("frame_table.header", "truncated v2 frame header")
        (
            _,
            _,
            sample_rate,
            fps_num,
            fps_den,
            frame_count,
        ) = header.unpack_from(data)
        if fps_num <= 0 or fps_den <= 0:
            raise timebase_error(
                "frame_table.frame_rate",
                "frame rate numerator and denominator must be positive",
                numerator=fps_num,
                denominator=fps_den,
            )
        frame_rate = Fraction(fps_num, fps_den)
    else:
        raise timebase_error(
            "frame_table.version",
            "unsupported frame timetable version",
            version=version,
            supported=[FRAME_VERSION_LEGACY, FRAME_VERSION],
        )

    payload = len(data) - header_size
    if payload != frame_count * 8:
        raise timebase_error(
            "frame_table.frame_count",
            "frame count in header contradicts payload length",
            declared_frame_count=frame_count,
            payload_bytes=payload,
            expected_payload_bytes=frame_count * 8,
        )
    if expected_frame_count is not None and expected_frame_count != frame_count:
        raise timebase_error(
            "frame_table.frame_count",
            "declared bundle frame count contradicts frame header",
            declared_in_bundle=expected_frame_count,
            declared_in_header=frame_count,
        )
    if expected_sample_rate is not None and expected_sample_rate != sample_rate:
        raise timebase_error(
            "frame_table.sample_rate",
            "frame table sample rate contradicts audio",
            frame_table_sample_rate=sample_rate,
            audio_sample_rate=expected_sample_rate,
        )
    if expected_frame_rate is not None:
        expected = parse_fraction(expected_frame_rate, field="frame_table.frame_rate")
        if expected != frame_rate:
            raise timebase_error(
                "frame_table.frame_rate",
                "frame table frame rate contradicts bundle declaration",
                declared=format_fraction(expected),
                header=format_fraction(frame_rate),
            )

    timestamps = struct.unpack_from(f"<{frame_count}Q", data, header_size)
    previous = -1
    for index, timestamp in enumerate(timestamps):
        if timestamp <= previous and index > 0:
            raise timebase_error(
                "frame_table.timestamps",
                "frame timestamps must be strictly increasing",
                index=index,
                previous=previous,
                actual=timestamp,
            )
        if total_samples is not None and timestamp >= total_samples:
            raise timebase_error(
                "frame_table.timestamps",
                "frame timestamp exceeds audio duration",
                index=index,
                timestamp=timestamp,
                total_samples=total_samples,
            )
        previous = timestamp

    if frame_count == 0:
        raise timebase_error(
            "frame_table.timestamps", "frame timetable must contain at least one frame"
        )
    if sample_rate != 48_000:
        raise timebase_error(
            "frame_table.sample_rate",
            "frame table sample rate must be 48000 Hz",
            actual=sample_rate,
        )
    return FrameTable(
        path=str(target),
        sample_rate=sample_rate,
        frame_rate=frame_rate,
        declared_frame_count=frame_count,
        timestamps=tuple(timestamps),
        version=version,
        sha256=sha256_bytes(data),
    )


def write_frame_table(
    path: str | Path,
    timestamps: Iterable[int],
    *,
    sample_rate: int = 48_000,
    frame_rate: Fraction | int = 30,
) -> FrameTable:
    values = tuple(int(value) for value in timestamps)
    if not values:
        raise ValueError("at least one frame timestamp is required")
    if any(value < 0 for value in values):
        raise ValueError("frame timestamps must be non-negative")
    if any(values[index] >= values[index + 1] for index in range(len(values) - 1)):
        raise ValueError("frame timestamps must be strictly increasing")
    rate = Fraction(frame_rate)
    header = struct.pack(
        "<4sHIIII",
        FRAME_MAGIC,
        FRAME_VERSION,
        sample_rate,
        rate.numerator,
        rate.denominator,
        len(values),
    )
    payload = struct.pack(f"<{len(values)}Q", *values)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(header + payload)
    return load_frame_table(
        target,
        expected_sample_rate=sample_rate,
        expected_frame_rate=rate,
        expected_frame_count=len(values),
    )


def _normalize_item(
    raw: dict[str, Any],
    *,
    timebase: Timebase,
    frames: FrameTable,
    total_samples: int,
    field_prefix: str,
    default_speed: Any = None,
    default_priority: int | None = None,
    is_description: bool = False,
) -> NormalizedSubtitle:
    if not isinstance(raw, dict):
        raise timebase_error(field_prefix, "subtitle entry must be an object")
    identifier = raw.get("id")
    if not isinstance(identifier, str) or not identifier:
        raise timebase_error(f"{field_prefix}.id", "subtitle id must be a non-empty string")
    text = raw.get("text", "")
    if not isinstance(text, str):
        raise timebase_error(f"{field_prefix}.text", "subtitle text must be a string")
    speaker = raw.get("speaker", "")
    if speaker is None:
        speaker = ""
    if not isinstance(speaker, str):
        raise timebase_error(
            f"{field_prefix}.speaker", "speaker must be a string"
        )

    source_tb = raw.get("timebase")
    source_ticks_per_second = timebase.ticks_per_second
    if source_tb is not None:
        if not isinstance(source_tb, dict):
            raise timebase_error(
                f"{field_prefix}.timebase", "timebase must be an object"
            )
        if "ticks_per_second" in source_tb:
            source_ticks_per_second = parse_fraction(
                source_tb["ticks_per_second"],
                field=f"{field_prefix}.timebase.ticks_per_second",
            )
            if source_ticks_per_second.denominator != 1:
                raise timebase_error(
                    f"{field_prefix}.timebase.ticks_per_second",
                    "ticks per second must be an integer",
                )
            source_ticks_per_second = source_ticks_per_second.numerator
        if "sample_rate" in source_tb:
            source_rate = parse_fraction(
                source_tb["sample_rate"], field=f"{field_prefix}.timebase.sample_rate"
            )
            if source_rate != frames.sample_rate:
                raise timebase_error(
                    f"{field_prefix}.timebase.sample_rate",
                    "subtitle time base contradicts audio/frame sample rate",
                    subtitle_sample_rate=format_fraction(source_rate),
                    audio_sample_rate=frames.sample_rate,
                )
        if "frame_rate" in source_tb:
            source_fps = parse_fraction(
                source_tb["frame_rate"], field=f"{field_prefix}.timebase.frame_rate"
            )
            if source_fps != frames.frame_rate:
                raise timebase_error(
                    f"{field_prefix}.timebase.frame_rate",
                    "subtitle frame rate contradicts frame timetable",
                    subtitle_frame_rate=format_fraction(source_fps),
                    frame_table_frame_rate=format_fraction(frames.frame_rate),
                )

    unit = raw.get("time_unit", "samples")
    if raw.get("start") is not None and raw.get("start_timecode") is not None:
        start_a = parse_time_value(
            raw["start"],
            timebase,
            frames.frame_rate,
            field=f"{field_prefix}.start",
            default_unit=unit,
        )
        start_b = parse_time_value(
            raw["start_timecode"],
            timebase,
            frames.frame_rate,
            field=f"{field_prefix}.start_timecode",
            default_unit=unit,
        )
        if start_a != start_b:
            raise timebase_error(
                f"{field_prefix}.start",
                "start and start_timecode identify different instants",
                start=start_a,
                start_timecode=start_b,
            )
        start = start_a
    elif raw.get("start") is not None:
        start = parse_time_value(
            raw["start"],
            timebase,
            frames.frame_rate,
            field=f"{field_prefix}.start",
            default_unit=unit,
        )
    elif raw.get("start_timecode") is not None:
        start = parse_time_value(
            raw["start_timecode"],
            timebase,
            frames.frame_rate,
            field=f"{field_prefix}.start_timecode",
            default_unit=unit,
        )
    else:
        raise timebase_error(
            f"{field_prefix}.start",
            "subtitle entry lacks start time",
        )

    if raw.get("end") is not None and raw.get("end_timecode") is not None:
        end_a = parse_time_value(
            raw["end"],
            timebase,
            frames.frame_rate,
            field=f"{field_prefix}.end",
            default_unit=unit,
        )
        end_b = parse_time_value(
            raw["end_timecode"],
            timebase,
            frames.frame_rate,
            field=f"{field_prefix}.end_timecode",
            default_unit=unit,
        )
        if end_a != end_b:
            raise timebase_error(
                f"{field_prefix}.end",
                "end and end_timecode identify different instants",
                end=end_a,
                end_timecode=end_b,
            )
        end = end_a
    elif raw.get("end") is not None:
        end = parse_time_value(
            raw["end"],
            timebase,
            frames.frame_rate,
            field=f"{field_prefix}.end",
            default_unit=unit,
        )
    elif raw.get("end_timecode") is not None:
        end = parse_time_value(
            raw["end_timecode"],
            timebase,
            frames.frame_rate,
            field=f"{field_prefix}.end_timecode",
            default_unit=unit,
        )
    else:
        raise timebase_error(f"{field_prefix}.end", "subtitle entry lacks end time")

    start = timebase.source_ticks_to_samples(
        start, source_ticks_per_second, field=f"{field_prefix}.start"
    )
    end = timebase.source_ticks_to_samples(
        end, source_ticks_per_second, field=f"{field_prefix}.end"
    )
    if start < 0:
        raise timebase_error(
            f"{field_prefix}.start", "start time is negative", start=start
        )
    if end <= start:
        raise timebase_error(
            f"{field_prefix}.end",
            "end time must be after start time",
            start=start,
            end=end,
        )
    if end > total_samples:
        raise timebase_error(
            f"{field_prefix}.end",
            "subtitle end time exceeds audio duration",
            end=end,
            total_samples=total_samples,
        )
    frame_start, frame_end = frames.frame_interval(start, end)
    speed_value = raw.get("max_read_speed", default_speed)
    if speed_value is not None:
        speed = format_fraction(parse_fraction(speed_value, field=f"{field_prefix}.max_read_speed"))
    else:
        speed = None
    priority_value = raw.get("priority", default_priority)
    if priority_value is not None:
        if isinstance(priority_value, bool) or not isinstance(priority_value, int):
            raise timebase_error(
                f"{field_prefix}.priority", "priority must be an integer"
            )
        priority = priority_value
    else:
        priority = None
    return NormalizedSubtitle(
        id=identifier,
        text=text,
        speaker=speaker,
        start_sample=start,
        end_sample=end,
        frame_start=frame_start,
        frame_end=frame_end,
        max_read_speed=speed,
        priority=priority,
    )


def unify_timebase(bundle: dict[str, Any], *, base_dir: str | Path = ".") -> TimebaseResult:
    if not isinstance(bundle, dict):
        raise timebase_error("bundle", "bundle root must be an object")
    base = Path(base_dir)
    audio_entry = bundle.get("audio")
    if not isinstance(audio_entry, dict):
        raise timebase_error("audio", "bundle.audio must be an object")
    audio_path_raw = audio_entry.get("path")
    if not isinstance(audio_path_raw, str) or not audio_path_raw:
        raise timebase_error("audio.path", "audio path is required")
    audio_path = base / audio_path_raw
    audio = inspect_wav(audio_path)

    if audio_entry.get("sample_rate") is not None:
        declared_rate = parse_fraction(
            audio_entry["sample_rate"], field="audio.sample_rate"
        )
        if declared_rate != audio.sample_rate:
            raise timebase_error(
                "audio.sample_rate",
                "declared audio sample rate contradicts WAV header",
                declared=format_fraction(declared_rate),
                actual=audio.sample_rate,
            )
    if audio_entry.get("channels") is not None:
        declared_channels = audio_entry["channels"]
        if declared_channels != audio.channels:
            raise timebase_error(
                "audio.channels",
                "declared channel count contradicts WAV header",
                declared=declared_channels,
                actual=audio.channels,
            )
    if audio_entry.get("total_samples") is not None:
        declared_samples = audio_entry["total_samples"]
        if declared_samples != audio.total_samples:
            raise timebase_error(
                "audio.total_samples",
                "declared sample count contradicts WAV header",
                declared=declared_samples,
                actual=audio.total_samples,
            )

    frame_entry = bundle.get("frame_table")
    if not isinstance(frame_entry, dict):
        raise timebase_error(
            "frame_table", "bundle.frame_table must be an object"
        )
    frame_path_raw = frame_entry.get("path")
    if not isinstance(frame_path_raw, str) or not frame_path_raw:
        raise timebase_error("frame_table.path", "frame table path is required")
    frames = load_frame_table(
        base / frame_path_raw,
        total_samples=audio.total_samples,
        expected_sample_rate=audio.sample_rate,
        expected_frame_rate=frame_entry.get("frame_rate"),
        expected_frame_count=frame_entry.get("frame_count"),
    )

    timebase_entry = bundle.get("timebase", {})
    if timebase_entry is None:
        timebase_entry = {}
    if not isinstance(timebase_entry, dict):
        raise timebase_error("timebase", "bundle.timebase must be an object")
    expected_tps = timebase_entry.get("ticks_per_second", audio.sample_rate)
    tps_fraction = parse_fraction(
        expected_tps, field="timebase.ticks_per_second"
    )
    if tps_fraction.denominator != 1:
        raise timebase_error(
            "timebase.ticks_per_second", "ticks per second must be an integer"
        )
    ticks_per_second = tps_fraction.numerator
    if ticks_per_second <= 0:
        raise timebase_error(
            "timebase.ticks_per_second", "ticks per second must be positive"
        )
    if timebase_entry.get("sample_rate") is not None:
        tb_rate = parse_fraction(
            timebase_entry["sample_rate"], field="timebase.sample_rate"
        )
        if tb_rate != audio.sample_rate:
            raise timebase_error(
                "timebase.sample_rate",
                "timebase sample rate contradicts audio",
                timebase_sample_rate=format_fraction(tb_rate),
                audio_sample_rate=audio.sample_rate,
            )
    timebase = Timebase(
        sample_rate=audio.sample_rate, ticks_per_second=ticks_per_second
    )

    tracks_entry = bundle.get("tracks")
    if not isinstance(tracks_entry, dict):
        raise timebase_error("tracks", "bundle.tracks must be an object")
    tracks: dict[str, tuple[NormalizedSubtitle, ...]] = {}
    default_speed = bundle.get("default_max_read_speed")
    default_priority = bundle.get("default_speech_priority", 3)
    for language in sorted(tracks_entry):
        raw_track = tracks_entry[language]
        if isinstance(raw_track, dict):
            raw_sentences = raw_track.get("sentences")
            track_default_speed = raw_track.get("default_max_read_speed", default_speed)
        else:
            raw_sentences = raw_track
            track_default_speed = default_speed
        if not isinstance(raw_sentences, list):
            raise timebase_error(
                f"tracks.{language}.sentences",
                "track sentences must be an array",
            )
        normalized: list[NormalizedSubtitle] = []
        seen: set[str] = set()
        for index, raw in enumerate(raw_sentences):
            item = _normalize_item(
                raw,
                timebase=timebase,
                frames=frames,
                total_samples=audio.total_samples,
                field_prefix=f"tracks.{language}.sentences[{index}]",
                default_speed=track_default_speed,
                default_priority=default_priority,
            )
            if item.id in seen:
                raise timebase_error(
                    f"tracks.{language}.sentences[{index}].id",
                    "duplicate subtitle id in track",
                    id=item.id,
                )
            seen.add(item.id)
            normalized.append(item)
        normalized.sort(key=lambda item: (item.start_sample, item.id))
        for index in range(1, len(normalized)):
            if normalized[index].start_sample < normalized[index - 1].end_sample:
                raise timebase_error(
                    f"tracks.{language}.sentences[{index}].start",
                    "source speech sentences overlap",
                    previous_id=normalized[index - 1].id,
                    previous_end=normalized[index - 1].end_sample,
                    current_id=normalized[index].id,
                    current_start=normalized[index].start_sample,
                )
        tracks[language] = tuple(normalized)

    raw_descriptions = bundle.get("descriptions", [])
    if raw_descriptions is None:
        raw_descriptions = []
    if not isinstance(raw_descriptions, list):
        raise timebase_error("descriptions", "bundle.descriptions must be an array")
    default_desc_priority = bundle.get("default_description_priority", 2)
    descriptions: list[NormalizedSubtitle] = []
    seen_descriptions: set[str] = set()
    for index, raw in enumerate(raw_descriptions):
        item = _normalize_item(
            raw,
            timebase=timebase,
            frames=frames,
            total_samples=audio.total_samples,
            field_prefix=f"descriptions[{index}]",
            default_speed=None,
            default_priority=default_desc_priority,
            is_description=True,
        )
        if item.id in seen_descriptions:
            raise timebase_error(
                f"descriptions[{index}].id",
                "duplicate description id",
                id=item.id,
            )
        seen_descriptions.add(item.id)
        descriptions.append(item)
    descriptions.sort(key=lambda item: (item.start_sample, item.id))

    return TimebaseResult(
        audio=audio,
        frames=frames,
        timebase=timebase,
        tracks=tracks,
        descriptions=tuple(descriptions),
    )

