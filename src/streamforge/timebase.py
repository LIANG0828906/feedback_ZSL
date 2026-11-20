from __future__ import annotations

import json
import math
import wave
from fractions import Fraction
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ErrorCode, fail
from .models import (
    AudioSource,
    SliceBoundary,
    SourceTimeline,
    VideoFrame,
    ns_to_sample,
    sha256_hex,
    sha256_json,
)


def _read_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise fail(ErrorCode.IO, "file not found", path=str(path)) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "invalid JSON media description",
            path=str(path),
            error=str(exc),
        ) from exc


def read_pcm16_wav(path: Path) -> AudioSource:
    try:
        with wave.open(str(path), "rb") as handle:
            channels = handle.getnchannels()
            sample_width = handle.getsampwidth()
            sample_rate = handle.getframerate()
            frame_count = handle.getnframes()
            compression = handle.getcomptype()
            raw = handle.readframes(frame_count)
    except FileNotFoundError as exc:
        raise fail(ErrorCode.IO, "WAV file not found", path=str(path)) from exc
    except (wave.Error, EOFError, OSError) as exc:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "cannot read PCM WAV",
            path=str(path),
            error=str(exc),
        ) from exc

    if channels not in (1, 2):
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "only mono or stereo audio is supported",
            channels=channels,
            path=str(path),
        )
    if sample_width != 2:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "only 16-bit PCM audio is supported",
            sample_width_bits=sample_width * 8,
            path=str(path),
        )
    if not 16000 <= sample_rate <= 48000:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "source sample rate must be between 16000 and 48000 Hz",
            sample_rate=sample_rate,
            path=str(path),
        )
    if compression != "NONE":
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "compressed WAV is not supported",
            compression=compression,
            path=str(path),
        )
    expected_bytes = frame_count * channels * 2
    if len(raw) != expected_bytes:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "truncated PCM payload",
            expected_bytes=expected_bytes,
            actual_bytes=len(raw),
            sample_position=len(raw) // (channels * 2),
        )
    if frame_count <= 0:
        raise fail(ErrorCode.TIMEBASE_INVALID, "audio contains no samples")
    samples = np.frombuffer(raw, dtype="<i2").reshape(-1, channels).copy()
    return AudioSource(
        path=path,
        sample_rate=sample_rate,
        channels=channels,
        samples=samples,
        raw_digest=sha256_hex(raw),
    )


def _unit_to_ns(value: Any, unit: str, *, field: str, frame_index: int) -> int:
    try:
        number = Fraction(str(value))
    except (ValueError, ZeroDivisionError) as exc:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            f"{field} is not numeric",
            frame_index=frame_index,
            position=frame_index,
        ) from exc
    if number < 0:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            f"{field} must not be negative",
            frame_index=frame_index,
            position=frame_index,
        )
    scale = {
        "ns": 1,
        "us": 1_000,
        "ms": 1_000_000,
        "s": 1_000_000_000,
    }.get(unit)
    if scale is None:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "unsupported time unit",
            frame_index=frame_index,
            position=frame_index,
            unit=unit,
        )
    return int(number * scale)


def _frame_time(
    frame: dict[str, Any],
    base_field: str,
    frame_index: int,
    default_unit: str,
) -> int:
    for unit in ("ns", "us", "ms", "s"):
        key = f"{base_field}_{unit}"
        if key in frame:
            return _unit_to_ns(
                frame[key], unit, field=key, frame_index=frame_index
            )
    if base_field in frame:
        unit = str(frame.get(f"{base_field}_unit", default_unit))
        return _unit_to_ns(
            frame[base_field], unit, field=base_field, frame_index=frame_index
        )
    raise fail(
        ErrorCode.TIMEBASE_INVALID,
        f"missing {base_field}",
        frame_index=frame_index,
        position=frame_index,
    )


def read_video_description(path: Path) -> tuple[list[VideoFrame], str]:
    document = _read_json(path)
    if not isinstance(document, dict):
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "video description must be a JSON object",
            path=str(path),
        )
    raw_frames = document.get("frames")
    if not isinstance(raw_frames, list) or not raw_frames:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "video description requires a non-empty frames array",
            path=str(path),
        )

    frames: list[VideoFrame] = []
    pts_seen: set[int] = set()
    display_seen: set[int] = set()
    previous_end: int | None = None
    for source_index, raw in enumerate(raw_frames):
        if not isinstance(raw, dict):
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "frame entry must be an object",
                frame_index=source_index,
                position=source_index,
            )
        pts_ns = _frame_time(raw, "pts", source_index, "ms")
        duration_ns = _frame_time(raw, "duration", source_index, "ms")
        display_raw = raw.get(
            "display_index", raw.get("display_order", raw.get("index", source_index))
        )
        try:
            display_index = int(display_raw)
        except (TypeError, ValueError) as exc:
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "display index must be an integer",
                frame_index=source_index,
                position=source_index,
            ) from exc
        if display_index < 0:
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "display index must not be negative",
                frame_index=source_index,
                position=source_index,
                display_index=display_index,
            )

        if source_index == 0 and pts_ns != 0:
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "first frame pts must be zero",
                frame_index=source_index,
                position=source_index,
                pts_ns=pts_ns,
            )
        if pts_ns in pts_seen:
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "duplicate frame pts",
                frame_index=source_index,
                position=source_index,
                pts_ns=pts_ns,
            )
        if display_index in display_seen:
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "duplicate display index",
                frame_index=source_index,
                position=source_index,
                display_index=display_index,
            )
        if duration_ns == 0:
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "frame duration must be positive",
                frame_index=source_index,
                position=source_index,
                pts_ns=pts_ns,
            )
        if frames and pts_ns <= frames[-1].pts_ns:
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "frame pts is not strictly increasing",
                frame_index=source_index,
                position=source_index,
                previous_pts_ns=frames[-1].pts_ns,
                pts_ns=pts_ns,
            )
        if previous_end is not None and pts_ns != previous_end:
            kind = "gap" if pts_ns > previous_end else "overlap"
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                f"video timeline contains a {kind}",
                frame_index=source_index,
                position=source_index,
                expected_pts_ns=previous_end,
                pts_ns=pts_ns,
            )
        pts_seen.add(pts_ns)
        display_seen.add(display_index)
        frames.append(
            VideoFrame(
                display_index=display_index,
                pts_ns=pts_ns,
                duration_ns=duration_ns,
                source_index=source_index,
            )
        )
        previous_end = frames[-1].end_ns

    digest = sha256_json(
        {
            "frames": [frame.to_dict() for frame in frames],
            "declared_duration_ns": document.get("duration_ns"),
        }
    )
    return frames, digest


def build_timeline(
    *,
    audio_path: Path,
    video_path: Path,
    slice_duration_ms: int,
    tolerance_ms: int,
) -> SourceTimeline:
    if slice_duration_ms <= 0:
        raise fail(
            ErrorCode.TIMEBASE_INVALID, "slice duration must be positive"
        )
    if tolerance_ms < 0:
        raise fail(
            ErrorCode.TIMEBASE_INVALID, "timebase tolerance must not be negative"
        )
    audio = read_pcm16_wav(audio_path)
    frames, video_digest = read_video_description(video_path)
    video_total_ns = frames[-1].end_ns
    tolerance_ns = tolerance_ms * 1_000_000
    delta_ns = abs(audio.duration_ns - video_total_ns)
    if delta_ns > tolerance_ns:
        raise fail(
            ErrorCode.TIMEBASE_INVALID,
            "audio and video durations differ beyond tolerance",
            audio_duration_ns=audio.duration_ns,
            audio_sample_position=audio.sample_count,
            delta_ns=delta_ns,
            frame_index=len(frames) - 1,
            position=len(frames) - 1,
            tolerance_ns=tolerance_ns,
            video_duration_ns=video_total_ns,
        )
    if video_total_ns <= 0:
        raise fail(ErrorCode.TIMEBASE_INVALID, "video timeline is empty")

    slice_ns = slice_duration_ms * 1_000_000
    count = math.ceil(video_total_ns / slice_ns)
    frames_by_pts = {frame.pts_ns: frame for frame in frames}
    boundaries: list[SliceBoundary] = []
    for index in range(count):
        start_ns = index * slice_ns
        end_ns = min(start_ns + slice_ns, video_total_ns)
        if start_ns not in frames_by_pts:
            next_frame = next(
                (frame for frame in frames if frame.pts_ns >= start_ns), None
            )
            raise fail(
                ErrorCode.TIMEBASE_INVALID,
                "slice boundary does not align with a video frame pts",
                boundary_index=index,
                frame_index=next_frame.source_index if next_frame else None,
                position=next_frame.source_index if next_frame else len(frames),
                slice_start_ns=start_ns,
            )
        boundaries.append(
            SliceBoundary(
                index=index,
                start_ns=start_ns,
                end_ns=end_ns,
                source_start_sample=ns_to_sample(start_ns, audio.sample_rate),
                source_end_sample=min(
                    audio.sample_count,
                    ns_to_sample(end_ns, audio.sample_rate),
                ),
            )
        )

    timeline = SourceTimeline(
        audio=audio,
        video_frames=frames,
        video_frames_by_pts=frames_by_pts,
        boundaries=boundaries,
        total_ns=video_total_ns,
        slice_duration_ns=slice_ns,
        tolerance_ns=tolerance_ns,
        video_total_ns=video_total_ns,
        video_digest=video_digest,
    )
    return timeline
