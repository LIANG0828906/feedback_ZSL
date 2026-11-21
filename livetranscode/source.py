from __future__ import annotations

import math
import wave
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any

import numpy as np

from .errors import ErrorCode, build_error
from .models import (
    AppConfig,
    SliceBoundary,
    SourceAudio,
    Timeline,
    VideoDescription,
    VideoFrame,
)
from .util import read_json, sha256_file

DEFAULT_VIDEO_TIMESCALE = 1_000_000


def read_source_audio(path: str | Path) -> SourceAudio:
    source_path = Path(path)
    try:
        with wave.open(str(source_path), "rb") as handle:
            channels = handle.getnchannels()
            sample_width = handle.getsampwidth()
            sample_rate = handle.getframerate()
            frame_count = handle.getnframes()
            comptype = handle.getcomptype()
            raw = handle.readframes(frame_count)
    except (OSError, wave.Error) as exc:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "cannot parse PCM WAV source",
            path=str(source_path),
            detail=str(exc),
        ) from exc

    if channels not in (1, 2):
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "only mono or stereo WAV is supported",
            path=str(source_path),
            channels=channels,
        )
    if sample_width != 2:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "WAV must use 16-bit PCM",
            path=str(source_path),
            sample_width_bytes=sample_width,
        )
    if comptype != "NONE":
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "compressed WAV is not supported",
            path=str(source_path),
            compression=comptype,
        )
    if not 16000 <= sample_rate <= 48000:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "sample rate must be between 16000 and 48000 Hz",
            path=str(source_path),
            sample_rate=sample_rate,
        )
    if frame_count <= 0:
        raise build_error(ErrorCode.TIMEBASE_INVALID, "WAV must contain at least one sample frame", path=str(source_path))
    expected_bytes = frame_count * channels * sample_width
    if len(raw) != expected_bytes:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "WAV payload length does not match its header",
            path=str(source_path),
            expected_bytes=expected_bytes,
            actual_bytes=len(raw),
        )

    samples = np.frombuffer(raw, dtype="<i2").reshape(frame_count, channels).copy()
    return SourceAudio(
        path=source_path.resolve(),
        sha256=sha256_file(source_path),
        sample_rate=sample_rate,
        channels=channels,
        samples=samples,
    )


def _decimal_to_ticks(value: Any, timescale: int, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        raise build_error(ErrorCode.TIMEBASE_INVALID, f"{field} must be numeric", field=field, value=value)
    try:
        decimal_value = Decimal(str(value))
    except InvalidOperation as exc:
        raise build_error(ErrorCode.TIMEBASE_INVALID, f"{field} is not a valid number", field=field, value=value) from exc
    ticks = decimal_value * Decimal(timescale)
    if ticks != ticks.to_integral_value():
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "timestamp has a non-integral remainder in the declared timescale",
            field=field,
            value=value,
            timescale=timescale,
            remainder=str(ticks - ticks.to_integral_value()),
        )
    return int(ticks)


def _parse_seconds(value: Any, timescale: int, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        raise build_error(ErrorCode.TIMEBASE_INVALID, f"{field} must be numeric", field=field, value=value)
    try:
        decimal_value = Decimal(str(value))
    except InvalidOperation as exc:
        raise build_error(ErrorCode.TIMEBASE_INVALID, f"{field} is not a valid number", field=field, value=value) from exc
    ticks = decimal_value * Decimal(timescale)
    if ticks != ticks.to_integral_value():
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "seconds value has a non-integral remainder in the declared timescale",
            field=field,
            value=value,
            timescale=timescale,
            remainder=str(ticks - ticks.to_integral_value()),
        )
    return int(ticks)


def _get_value(raw: dict[str, Any], primary: str, seconds_name: str) -> Any:
    if primary in raw:
        return raw[primary]
    if seconds_name in raw:
        return ("seconds", raw[seconds_name])
    return None


def read_video_description(path: str | Path) -> VideoDescription:
    source_path = Path(path)
    raw = read_json(source_path)
    if isinstance(raw, list):
        timescale = DEFAULT_VIDEO_TIMESCALE
        raw_frames = raw
        total_duration: Any = None
    elif isinstance(raw, dict):
        timescale = raw.get("timescale", raw.get("timebase", DEFAULT_VIDEO_TIMESCALE))
        raw_frames = raw.get("frames", raw.get("video_frames"))
        total_duration = raw.get("duration")
        if total_duration is None:
            total_duration = raw.get("duration_ticks")
    else:
        raise build_error(ErrorCode.TIMEBASE_INVALID, "video description root must be an object or list")
    if isinstance(timescale, bool) or not isinstance(timescale, int) or timescale <= 0:
        raise build_error(ErrorCode.TIMEBASE_INVALID, "video timescale must be a positive integer", timescale=timescale)
    if not isinstance(raw_frames, list) or not raw_frames:
        raise build_error(ErrorCode.TIMEBASE_INVALID, "video description must contain a non-empty frames list")

    pts_values: list[int] = []
    display_indices: list[int] = []
    explicit_durations: list[int | None] = []
    for position, item in enumerate(raw_frames):
        field = f"frames[{position}]"
        if not isinstance(item, dict):
            raise build_error(ErrorCode.TIMEBASE_INVALID, "each frame must be an object", field=field)
        pts_value = _get_value(item, "pts", "pts_seconds")
        if pts_value is None:
            raise build_error(ErrorCode.TIMEBASE_INVALID, "frame pts is required", field=f"{field}.pts")
        if isinstance(pts_value, tuple):
            pts = _parse_seconds(pts_value[1], timescale, f"{field}.pts_seconds")
        else:
            pts = _decimal_to_ticks(pts_value, timescale, f"{field}.pts")
        display_index = item.get("index", item.get("display_index", position))
        if isinstance(display_index, bool) or not isinstance(display_index, int) or display_index < 0:
            raise build_error(
                ErrorCode.TIMEBASE_INVALID,
                "display index must be a non-negative integer",
                field=f"{field}.index",
                value=display_index,
            )
        duration_value = _get_value(item, "duration", "duration_seconds")
        if duration_value is None and "duration_ticks" in item:
            duration_value = item["duration_ticks"]
        duration: int | None
        if duration_value is None:
            duration = None
        elif isinstance(duration_value, tuple):
            duration = _parse_seconds(duration_value[1], timescale, f"{field}.duration_seconds")
        else:
            duration = _decimal_to_ticks(duration_value, timescale, f"{field}.duration")
        if duration is not None and duration <= 0:
            raise build_error(
                ErrorCode.TIMEBASE_INVALID,
                "frame duration must be positive",
                field=f"{field}.duration",
                value=duration,
            )
        pts_values.append(pts)
        display_indices.append(display_index)
        explicit_durations.append(duration)

    total_duration_ticks: int | None = None
    if total_duration is not None:
        total_duration_ticks = _decimal_to_ticks(total_duration, timescale, "duration")
        if total_duration_ticks <= 0:
            raise build_error(ErrorCode.TIMEBASE_INVALID, "video duration must be positive", duration=total_duration_ticks)

    if pts_values[0] != 0:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "first video frame must start at pts 0",
            first_pts=pts_values[0],
            timescale=timescale,
        )
    frames: list[VideoFrame] = []
    for position, (pts, display_index, explicit_duration) in enumerate(
        zip(pts_values, display_indices, explicit_durations, strict=True)
    ):
        if position and pts < pts_values[position - 1]:
            raise build_error(
                ErrorCode.TIMEBASE_INVALID,
                "frame pts is not monotonic",
                frame_position=position,
                previous_pts=pts_values[position - 1],
                pts=pts,
            )
        if position and display_index <= display_indices[position - 1]:
            raise build_error(
                ErrorCode.TIMEBASE_INVALID,
                "display index must be strictly increasing",
                frame_position=position,
                previous_display_index=display_indices[position - 1],
                display_index=display_index,
            )
        if position:
            previous_end = frames[-1].end_ticks
            if pts < previous_end:
                raise build_error(
                    ErrorCode.TIMEBASE_INVALID,
                    "video frames overlap",
                    frame_position=position,
                    previous_end_ticks=previous_end,
                    pts_ticks=pts,
                )
            if pts > previous_end:
                raise build_error(
                    ErrorCode.TIMEBASE_INVALID,
                    "video frame timeline contains a gap",
                    frame_position=position,
                    previous_end_ticks=previous_end,
                    pts_ticks=pts,
                )
        if explicit_duration is None:
            if position + 1 < len(pts_values):
                duration = pts_values[position + 1] - pts
            elif total_duration_ticks is not None:
                duration = total_duration_ticks - pts
            else:
                raise build_error(
                    ErrorCode.TIMEBASE_INVALID,
                    "last frame duration is required when no video duration is declared",
                    frame_position=position,
                )
            if duration <= 0:
                raise build_error(
                    ErrorCode.TIMEBASE_INVALID,
                    "derived frame duration must be positive",
                    frame_position=position,
                    duration=duration,
                )
        else:
            duration = explicit_duration
        frames.append(VideoFrame(display_index=display_index, pts_ticks=pts, duration_ticks=duration))

    for position in range(1, len(frames)):
        if frames[position - 1].end_ticks != frames[position].pts_ticks:
            raise build_error(
                ErrorCode.TIMEBASE_INVALID,
                "video frames must be contiguous after duration resolution",
                frame_position=position,
            )
    return VideoDescription(
        path=source_path.resolve(),
        sha256=sha256_file(source_path),
        timescale=timescale,
        frames=tuple(frames),
    )


def build_timeline(audio: SourceAudio, video: VideoDescription, config: AppConfig) -> Timeline:
    rates = [audio.sample_rate, video.timescale, 1000]
    rates.extend(track.resample_rate for track in config.tracks)
    ticks_per_second = 1
    for rate in rates:
        ticks_per_second = math.lcm(ticks_per_second, rate)

    sample_ticks = ticks_per_second // audio.sample_rate
    if ticks_per_second % audio.sample_rate:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "sample rate cannot be represented exactly in the unified integer timebase",
            sample_rate=audio.sample_rate,
            ticks_per_second=ticks_per_second,
        )
    video_ticks = ticks_per_second // video.timescale
    if ticks_per_second % video.timescale:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "video timescale cannot be represented exactly in the unified integer timebase",
            video_timescale=video.timescale,
            ticks_per_second=ticks_per_second,
        )
    slice_ticks_numerator = config.slice_duration_ms * ticks_per_second
    if slice_ticks_numerator % 1000:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "slice duration has a non-integral remainder in the unified integer timebase",
            slice_duration_ms=config.slice_duration_ms,
            ticks_per_second=ticks_per_second,
        )
    slice_ticks = slice_ticks_numerator // 1000

    source_end_ticks = audio.sample_count * sample_ticks
    video_end_ticks = video.end_ticks * video_ticks
    if video_end_ticks < source_end_ticks:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "video description does not cover the complete audio timeline",
            audio_end_ticks=source_end_ticks,
            video_end_ticks=video_end_ticks,
        )

    boundaries = []
    start_ticks = 0
    index = 0
    while start_ticks < source_end_ticks:
        end_ticks = min(source_end_ticks, start_ticks + slice_ticks)
        source_start = start_ticks * audio.sample_rate // ticks_per_second
        source_end = end_ticks * audio.sample_rate // ticks_per_second
        if start_ticks * audio.sample_rate % ticks_per_second or end_ticks * audio.sample_rate % ticks_per_second:
            raise build_error(
                ErrorCode.TIMEBASE_INVALID,
                "slice boundary is not sample-aligned",
                boundary_ticks=start_ticks,
                sample_rate=audio.sample_rate,
                ticks_per_second=ticks_per_second,
            )
        output_counts: dict[str, int] = {}
        for track in config.tracks:
            numerator = (end_ticks - start_ticks) * track.resample_rate
            if numerator % ticks_per_second:
                raise build_error(
                    ErrorCode.TIMEBASE_INVALID,
                    "slice duration is not an integral number of output samples for a track",
                    track_id=track.track_id,
                    resample_rate=track.resample_rate,
                    ticks_per_second=ticks_per_second,
                )
            output_counts[track.track_id] = numerator // ticks_per_second
        boundaries.append(
            SliceBoundary(
                index=index,
                start_ticks=start_ticks,
                end_ticks=end_ticks,
                start_source_sample=source_start,
                end_source_sample=source_end,
                output_samples_by_track=output_counts,
            )
        )
        start_ticks = end_ticks
        index += 1

    if not boundaries:
        raise build_error(ErrorCode.TIMEBASE_INVALID, "source produced an empty slice boundary table")
    return Timeline(
        ticks_per_second=ticks_per_second,
        source_sample_rate=audio.sample_rate,
        source_channels=audio.channels,
        source_sample_count=audio.sample_count,
        source_end_ticks=source_end_ticks,
        video_timescale=video.timescale,
        video_end_ticks=video_end_ticks,
        slice_duration_ticks=slice_ticks,
        boundaries=tuple(boundaries),
        track_sample_rates={track.track_id: track.resample_rate for track in config.tracks},
        source_audio_sha256=audio.sha256,
        source_video_sha256=video.sha256,
    )
