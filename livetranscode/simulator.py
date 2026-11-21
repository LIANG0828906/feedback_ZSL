from __future__ import annotations

import wave
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any

from .errors import BusinessError, ErrorCode, build_error
from .models import AppConfig, SliceBoundary, Timeline, TrackConfig
from .util import read_json, write_json_atomic, write_jsonl_atomic


def _parse_bandwidth_samples(raw: Any, timeline: Timeline) -> list[tuple[int, float]]:
    if isinstance(raw, dict):
        raw_samples = raw.get("samples", raw.get("bandwidth"))
    else:
        raw_samples = raw
    if not isinstance(raw_samples, list) or not raw_samples:
        raise build_error(ErrorCode.CONFIG_INVALID, "bandwidth timeline must be a non-empty list")
    samples: list[tuple[int, float]] = []
    for index, item in enumerate(raw_samples):
        if isinstance(item, (int, float)) and not isinstance(item, bool):
            if index >= len(timeline.boundaries):
                break
            ticks = timeline.boundaries[index].start_ticks
            bandwidth = float(item)
        elif isinstance(item, dict):
            bandwidth = item.get("bandwidth_kbps", item.get("kbps"))
            if "time_ticks" in item:
                ticks = item["time_ticks"]
            elif "time_ms" in item:
                value = item["time_ms"]
                if isinstance(value, bool):
                    raise build_error(ErrorCode.CONFIG_INVALID, "bandwidth time_ms must be numeric")
                try:
                    exact = Decimal(str(value)) * Decimal(timeline.ticks_per_second) / Decimal(1000)
                except InvalidOperation as exc:
                    raise build_error(ErrorCode.CONFIG_INVALID, "bandwidth time_ms is invalid") from exc
                if exact != exact.to_integral_value():
                    raise build_error(
                        ErrorCode.CONFIG_INVALID,
                        "bandwidth time_ms has a non-integral timebase remainder",
                        index=index,
                    )
                ticks = int(exact)
            elif "boundary_index" in item:
                boundary_index = item["boundary_index"]
                if (
                    isinstance(boundary_index, bool)
                    or not isinstance(boundary_index, int)
                    or not 0 <= boundary_index < len(timeline.boundaries)
                ):
                    raise build_error(ErrorCode.CONFIG_INVALID, "invalid bandwidth boundary_index", index=index)
                ticks = timeline.boundaries[boundary_index].start_ticks
            else:
                raise build_error(
                    ErrorCode.CONFIG_INVALID,
                    "bandwidth sample needs time_ticks, time_ms or boundary_index",
                    index=index,
                )
        else:
            raise build_error(ErrorCode.CONFIG_INVALID, "invalid bandwidth sample", index=index)
        if isinstance(bandwidth, bool) or not isinstance(bandwidth, (int, float)) or bandwidth <= 0:
            raise build_error(ErrorCode.CONFIG_INVALID, "bandwidth_kbps must be positive", index=index)
        if not isinstance(ticks, int) or isinstance(ticks, bool) or ticks < 0:
            raise build_error(ErrorCode.CONFIG_INVALID, "bandwidth time must be a non-negative integer", index=index)
        if samples and ticks < samples[-1][0]:
            raise build_error(ErrorCode.CONFIG_INVALID, "bandwidth timeline must be monotonic", index=index)
        samples.append((ticks, float(bandwidth)))
    if not samples:
        raise build_error(ErrorCode.CONFIG_INVALID, "bandwidth timeline has no usable samples")
    return samples


def _bandwidth_at(samples: list[tuple[int, float]], ticks: int) -> float:
    selected = samples[0][1]
    for sample_ticks, bandwidth in samples:
        if sample_ticks > ticks:
            break
        selected = bandwidth
    return selected


def _slice_path(workspace: Path, track_id: str, entry: dict[str, Any]) -> Path:
    return workspace / "tracks" / track_id / "slices" / str(entry["slice_relpath"])


def _validate_switch_entry(
    workspace: Path,
    track: TrackConfig,
    boundary: SliceBoundary,
    entry: dict[str, Any],
    timeline: Timeline,
) -> list[str]:
    conflicts: list[str] = []
    expected_audio_start = boundary.start_ticks * track.resample_rate // timeline.ticks_per_second
    expected_audio_end = boundary.end_ticks * track.resample_rate // timeline.ticks_per_second
    if entry.get("track_id") != track.track_id:
        conflicts.append("track_id")
    if entry.get("start_ticks") != boundary.start_ticks:
        conflicts.append("start_ticks")
    if entry.get("end_ticks") != boundary.end_ticks:
        conflicts.append("end_ticks")
    if entry.get("audio_start_ticks") != boundary.start_ticks:
        conflicts.append("audio_start_ticks")
    if entry.get("audio_end_ticks") != boundary.end_ticks:
        conflicts.append("audio_end_ticks")
    if entry.get("video_start_ticks") != boundary.start_ticks:
        conflicts.append("video_start_ticks")
    if entry.get("video_end_ticks") != boundary.end_ticks:
        conflicts.append("video_end_ticks")
    if entry.get("start_output_sample") != expected_audio_start:
        conflicts.append("start_output_sample")
    if entry.get("end_output_sample") != expected_audio_end:
        conflicts.append("end_output_sample")
    if entry.get("output_sample_count") != expected_audio_end - expected_audio_start:
        conflicts.append("output_sample_count")
    if entry.get("track_params_version") != track.params_version:
        conflicts.append("track_params_version")
    if conflicts:
        return conflicts

    slice_dir = _slice_path(workspace, track.track_id, entry)
    audio_path = slice_dir / "audio.wav"
    video_path = slice_dir / "video.json"
    try:
        with wave.open(str(audio_path), "rb") as handle:
            if handle.getframerate() != track.resample_rate:
                conflicts.append("audio_sample_rate")
            if handle.getnchannels() not in (1, 2):
                conflicts.append("audio_channels")
            if handle.getsampwidth() != 2:
                conflicts.append("audio_sample_width")
            if handle.getnframes() != expected_audio_end - expected_audio_start:
                conflicts.append("audio_frame_count")
    except (OSError, wave.Error):
        conflicts.append("audio_file")
    try:
        video_value = read_json(video_path)
        if not isinstance(video_value, dict):
            return conflicts + ["video_document"]
        frames = video_value.get("frames", [])
        if video_value.get("start_ticks") != boundary.start_ticks:
            conflicts.append("video_document_start_ticks")
        if video_value.get("end_ticks") != boundary.end_ticks:
            conflicts.append("video_document_end_ticks")
        if not frames:
            conflicts.append("video_frames")
        else:
            cursor = boundary.start_ticks
            for frame in frames:
                if frame.get("pts_ticks") != cursor:
                    conflicts.append("video_frame_continuity")
                    break
                cursor = frame.get("pts_ticks", 0) + frame.get("duration_ticks", 0)
            if cursor != boundary.end_ticks:
                conflicts.append("video_frame_end")
    except BusinessError:
        conflicts.append("video_file")
    return conflicts


def simulate_adaptive_playback(
    workspace: str | Path,
    config: AppConfig,
    timeline: Timeline,
    track_indexes: dict[str, dict[str, Any]],
    bandwidth_raw: Any,
) -> dict[str, Any]:
    target = Path(workspace).resolve()
    bandwidth_samples = _parse_bandwidth_samples(bandwidth_raw, timeline)
    decisions: list[dict[str, Any]] = []
    switches: list[dict[str, Any]] = []
    current_track_id: str | None = None
    buffer_ms = float(min(config.target_buffer_ms, config.decode_buffer_capacity_ms))
    valid = True
    error_value: dict[str, Any] | None = None

    def selected_entry(track_id: str, boundary_index: int) -> dict[str, Any] | None:
        entries = track_indexes.get(track_id, {}).get("entries", [])
        if boundary_index >= len(entries):
            return None
        entry = entries[boundary_index]
        if entry.get("slice_index") != boundary_index:
            return None
        return entry

    for boundary in timeline.boundaries:
        bandwidth = _bandwidth_at(bandwidth_samples, boundary.start_ticks)
        candidates: list[TrackConfig] = []
        candidate_entries: dict[str, dict[str, Any]] = {}
        conflicts_by_track: dict[str, list[str]] = {}
        for track in config.tracks:
            entry = selected_entry(track.track_id, boundary.index)
            if entry is None:
                continue
            conflicts = _validate_switch_entry(target, track, boundary, entry, timeline)
            if conflicts:
                conflicts_by_track[track.track_id] = conflicts
                continue
            candidates.append(track)
            candidate_entries[track.track_id] = entry
        if not candidates:
            valid = False
            error_value = {
                "code": ErrorCode.SWITCH_INVALID,
                "message": "no track has a valid committed slice at this boundary",
                "context": {
                    "boundary_ticks": boundary.start_ticks,
                    "slice_index": boundary.index,
                    "track_id": current_track_id,
                    "conflict_fields": sorted(
                        {field for fields in conflicts_by_track.values() for field in fields}
                    )
                    or ["committed_slice"],
                    "track_conflicts": conflicts_by_track,
                },
            }
            decisions.append(
                {
                    "boundary_index": boundary.index,
                    "boundary_ticks": boundary.start_ticks,
                    "valid": False,
                    "error": error_value,
                }
            )
            break

        current = next((track for track in candidates if track.track_id == current_track_id), None)
        sorted_tracks = sorted(candidates, key=lambda item: (item.target_bitrate_kbps, item.track_id))

        def memory_capacity_ms(track: TrackConfig) -> float:
            return config.max_decode_buffer_bytes * 8.0 / float(track.target_bitrate_kbps)

        def memory_fits(track: TrackConfig, level_ms: float) -> bool:
            return level_ms * track.target_bitrate_kbps / 8.0 <= config.max_decode_buffer_bytes + 1e-9

        reason = "continue"
        selected: TrackConfig | None = None
        if current is None:
            affordable = [track for track in sorted_tracks if track.target_bitrate_kbps <= bandwidth]
            selected = affordable[-1] if affordable else sorted_tracks[0]
            reason = "initial" if current_track_id is None else "gap_avoidance"
        elif buffer_ms < config.low_buffer_ms:
            selected = sorted_tracks[0]
            reason = "low_buffer_downshift" if selected.track_id != current.track_id else "low_buffer_hold"
        elif current.target_bitrate_kbps > bandwidth:
            lower = [track for track in sorted_tracks if track.target_bitrate_kbps <= bandwidth]
            selected = lower[-1] if lower else sorted_tracks[0]
            reason = "bandwidth_downshift"
        elif buffer_ms >= config.target_buffer_ms and bandwidth > current.target_bitrate_kbps * 1.15:
            affordable = [track for track in sorted_tracks if track.target_bitrate_kbps <= bandwidth * 0.85]
            selected = affordable[-1] if affordable else current
            reason = "upgrade" if selected.track_id != current.track_id else "hold"
        else:
            selected = current
        if selected is None:
            selected = sorted_tracks[0]

        if not memory_fits(selected, buffer_ms) and current is not None and memory_fits(current, buffer_ms):
            selected = current
            reason += "_memory_hold"
        effective_capacity = min(config.decode_buffer_capacity_ms, memory_capacity_ms(selected))
        if effective_capacity < config.low_buffer_ms or not memory_fits(selected, buffer_ms):
            valid = False
            error_value = {
                "code": ErrorCode.SWITCH_INVALID,
                "message": "selected track cannot fit the decode buffer budget",
                "context": {
                    "track_id": selected.track_id,
                    "boundary_ticks": boundary.start_ticks,
                    "slice_index": boundary.index,
                    "conflict_fields": ["decode_buffer_bytes"],
                    "buffer_ms": buffer_ms,
                    "max_decode_buffer_bytes": config.max_decode_buffer_bytes,
                },
            }
            decisions.append(
                {
                    "boundary_index": boundary.index,
                    "boundary_ticks": boundary.start_ticks,
                    "valid": False,
                    "error": error_value,
                }
            )
            break

        duration_ms = boundary.ticks * 1000.0 / timeline.ticks_per_second
        download_playback_ms = duration_ms * bandwidth / selected.target_bitrate_kbps
        raw_buffer = buffer_ms + download_playback_ms - duration_ms
        throttled = raw_buffer > effective_capacity
        buffer_ms = min(effective_capacity, max(0.0, raw_buffer))
        buffer_bytes = buffer_ms * selected.target_bitrate_kbps / 8.0
        if buffer_bytes > config.max_decode_buffer_bytes + 1e-9:
            valid = False
            error_value = {
                "code": ErrorCode.SWITCH_INVALID,
                "message": "decode buffer exceeds its configured byte budget",
                "context": {
                    "track_id": selected.track_id,
                    "boundary_ticks": boundary.start_ticks,
                    "slice_index": boundary.index,
                    "conflict_fields": ["decode_buffer_bytes"],
                    "buffer_bytes": buffer_bytes,
                    "max_decode_buffer_bytes": config.max_decode_buffer_bytes,
                },
            }
            break
        if buffer_ms <= 0 and boundary.index + 1 < len(timeline.boundaries):
            valid = False
            error_value = {
                "code": ErrorCode.SWITCH_INVALID,
                "message": "decode buffer underrun at a slice boundary",
                "context": {
                    "track_id": selected.track_id,
                    "boundary_ticks": boundary.start_ticks,
                    "slice_index": boundary.index,
                    "conflict_fields": ["decode_buffer_underrun"],
                    "buffer_ms": buffer_ms,
                },
            }
            break

        switched = current_track_id is not None and selected.track_id != current_track_id
        decision = {
            "boundary_index": boundary.index,
            "boundary_ticks": boundary.start_ticks,
            "bandwidth_kbps": bandwidth,
            "track_id": selected.track_id,
            "target_bitrate_kbps": selected.target_bitrate_kbps,
            "switched": switched,
            "reason": reason,
            "buffer_ms_after": buffer_ms,
            "buffer_bytes_after": buffer_bytes,
            "buffer_capacity_ms": effective_capacity,
            "download_throttled_at_capacity": throttled,
            "audio_start_ticks": candidate_entries[selected.track_id]["audio_start_ticks"],
            "video_start_ticks": candidate_entries[selected.track_id]["video_start_ticks"],
        }
        decisions.append(decision)
        if switched:
            switches.append(
                {
                    "boundary_index": boundary.index,
                    "boundary_ticks": boundary.start_ticks,
                    "from_track_id": current_track_id,
                    "to_track_id": selected.track_id,
                    "from_bitrate_kbps": next(
                        (
                            track.target_bitrate_kbps
                            for track in config.tracks
                            if track.track_id == current_track_id
                        ),
                        None,
                    ),
                    "to_bitrate_kbps": selected.target_bitrate_kbps,
                    "bandwidth_kbps": bandwidth,
                    "reason": reason,
                    "buffer_ms_before": (
                        decisions[-2]["buffer_ms_after"] if len(decisions) >= 2 else config.target_buffer_ms
                    ),
                    "buffer_ms_after": buffer_ms,
                    "buffer_effect": (
                        "higher bitrate drains the buffer faster for a fixed bandwidth; "
                        "lower bitrate refills it faster"
                    ),
                }
            )
        current_track_id = selected.track_id

    trace = {
        "schema_version": 1,
        "valid": valid,
        "final_track_id": current_track_id,
        "bandwidth_samples": [
            {"time_ticks": ticks, "bandwidth_kbps": bandwidth}
            for ticks, bandwidth in bandwidth_samples
        ],
        "decision_count": len(decisions),
        "switch_count": len(switches),
        "decisions": decisions,
        "switches": switches,
        "error": error_value,
    }
    write_json_atomic(target / "switches.json", trace)
    write_jsonl_atomic(target / "switches.jsonl", decisions)
    if not valid and error_value is not None:
        context = dict(error_value["context"])
        raise build_error(error_value["code"], error_value["message"], **context)
    return trace

