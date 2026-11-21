from __future__ import annotations

import bisect
import hashlib
from dataclasses import dataclass
from typing import Any

from .errors import ErrorCode, build_error
from .models import SliceBoundary, TrackConfig, VideoDescription
from .util import canonical_json_bytes, sha256_bytes


@dataclass(frozen=True)
class VideoSlice:
    data: bytes
    sha256: str
    records: tuple[dict[str, Any], ...]


def _frame_payload(display_index: int, start_ticks: int, duration_ticks: int, quantization: int) -> bytes:
    material = f"{quantization}|{display_index}|{start_ticks}|{duration_ticks}".encode("ascii")
    return hashlib.sha256(material).digest()


def deterministic_video_slice(
    video: VideoDescription,
    boundary: SliceBoundary,
    track: TrackConfig,
    ticks_per_second: int,
) -> VideoSlice:
    video_tick_factor = ticks_per_second // video.timescale
    frame_starts = [frame.pts_ticks * video_tick_factor for frame in video.frames]
    first = max(0, bisect.bisect_right(frame_starts, boundary.start_ticks) - 1)
    if first >= len(video.frames):
        first = len(video.frames) - 1
    records: list[dict[str, Any]] = []
    cursor = boundary.start_ticks
    for frame in video.frames[first:]:
        frame_start = frame.pts_ticks * video_tick_factor
        frame_end = frame.end_ticks * video_tick_factor
        if frame_end <= boundary.start_ticks:
            continue
        if frame_start >= boundary.end_ticks:
            break
        clipped_start = max(frame_start, boundary.start_ticks)
        clipped_end = min(frame_end, boundary.end_ticks)
        if clipped_start != cursor:
            raise build_error(
                ErrorCode.TIMEBASE_INVALID,
                "video coverage is not contiguous inside a slice",
                slice_index=boundary.index,
                expected_start_ticks=cursor,
                actual_start_ticks=clipped_start,
                frame_index=frame.display_index,
            )
        duration = clipped_end - clipped_start
        payload = _frame_payload(frame.display_index, clipped_start, duration, track.video_quantization)
        records.append(
            {
                "display_index": frame.display_index,
                "source_pts_ticks": frame_start,
                "source_end_ticks": frame_end,
                "pts_ticks": clipped_start,
                "duration_ticks": duration,
                "quantization": track.video_quantization,
                "payload_sha256": sha256_bytes(payload),
                "payload_hex": payload.hex(),
            }
        )
        cursor = clipped_end
        if cursor >= boundary.end_ticks:
            break
    if cursor != boundary.end_ticks:
        raise build_error(
            ErrorCode.TIMEBASE_INVALID,
            "video description does not cover a slice",
            slice_index=boundary.index,
            expected_end_ticks=boundary.end_ticks,
            actual_end_ticks=cursor,
        )
    value = {
        "schema_version": 1,
        "track_id": track.track_id,
        "params_version": track.params_version,
        "ticks_per_second": ticks_per_second,
        "start_ticks": boundary.start_ticks,
        "end_ticks": boundary.end_ticks,
        "quantization": track.video_quantization,
        "frames": records,
    }
    data = canonical_json_bytes(value)
    return VideoSlice(data=data, sha256=sha256_bytes(data), records=tuple(records))

