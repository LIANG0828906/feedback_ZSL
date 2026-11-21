from __future__ import annotations

import hashlib
import wave
from pathlib import Path
from typing import Any

from .engine import TranscodeEngine
from .util import read_json


def _check_video_document(path: Path, start_ticks: int, end_ticks: int) -> list[str]:
    problems: list[str] = []
    try:
        value = read_json(path)
    except Exception:
        return ["video_file"]
    if not isinstance(value, dict):
        return ["video_document"]
    if value.get("start_ticks") != start_ticks:
        problems.append("video_start_ticks")
    if value.get("end_ticks") != end_ticks:
        problems.append("video_end_ticks")
    cursor = start_ticks
    frames = value.get("frames")
    if not isinstance(frames, list) or not frames:
        return problems + ["video_frames"]
    for frame in frames:
        if frame.get("pts_ticks") != cursor:
            problems.append("video_frame_gap_or_overlap")
            break
        duration = frame.get("duration_ticks")
        if not isinstance(duration, int) or duration <= 0:
            problems.append("video_frame_duration")
            break
        cursor += duration
        try:
            payload = bytes.fromhex(frame.get("payload_hex", ""))
        except (TypeError, ValueError):
            problems.append("video_payload")
            break
        if hashlib.sha256(payload).hexdigest() != frame.get("payload_sha256"):
            problems.append("video_payload_hash")
            break
    if cursor != end_ticks:
        problems.append("video_frame_end")
    return problems


def verify_workspace(workspace: str | Path) -> dict[str, Any]:
    engine = TranscodeEngine.load_workspace(workspace)
    timeline = engine.timeline
    problems: list[dict[str, Any]] = []
    boundaries = timeline.boundaries
    if boundaries[0].start_ticks != 0:
        problems.append({"scope": "timebase", "field": "first_start_ticks"})
    if boundaries[-1].end_ticks != timeline.source_end_ticks:
        problems.append({"scope": "timebase", "field": "last_end_ticks"})
    for index, boundary in enumerate(boundaries):
        if boundary.start_source_sample != boundary.start_ticks * timeline.source_sample_rate // timeline.ticks_per_second:
            problems.append({"scope": "timebase", "slice_index": index, "field": "start_source_sample"})
        if boundary.end_source_sample != boundary.end_ticks * timeline.source_sample_rate // timeline.ticks_per_second:
            problems.append({"scope": "timebase", "slice_index": index, "field": "end_source_sample"})
        if index:
            previous = boundaries[index - 1]
            if boundary.start_ticks != previous.end_ticks:
                problems.append({"scope": "timebase", "slice_index": index, "field": "tick_continuity"})
            if boundary.start_source_sample != previous.end_source_sample:
                problems.append({"scope": "timebase", "slice_index": index, "field": "sample_continuity"})
        for track in engine.config.tracks:
            output_count = boundary.output_samples_by_track[track.track_id]
            expected = boundary.ticks * track.resample_rate // timeline.ticks_per_second
            if output_count != expected:
                problems.append(
                    {
                        "scope": "timebase",
                        "slice_index": index,
                        "track_id": track.track_id,
                        "field": "output_samples",
                    }
                )

    track_summaries: dict[str, Any] = {}
    for track in engine.config.tracks:
        index_value = engine._indexes[track.track_id]
        entries = index_value["entries"]
        track_problems: list[dict[str, Any]] = []
        previous_output_end = 0
        for slice_index, entry in enumerate(entries):
            boundary = boundaries[slice_index]
            expected_start = boundary.start_ticks * track.resample_rate // timeline.ticks_per_second
            expected_end = boundary.end_ticks * track.resample_rate // timeline.ticks_per_second
            if entry.get("start_ticks") != boundary.start_ticks or entry.get("end_ticks") != boundary.end_ticks:
                track_problems.append({"slice_index": slice_index, "field": "boundary_ticks"})
            if entry.get("audio_start_ticks") != boundary.start_ticks:
                track_problems.append({"slice_index": slice_index, "field": "audio_start_ticks"})
            if entry.get("video_start_ticks") != boundary.start_ticks:
                track_problems.append({"slice_index": slice_index, "field": "video_start_ticks"})
            if entry.get("start_output_sample") != expected_start:
                track_problems.append({"slice_index": slice_index, "field": "start_output_sample"})
            if entry.get("end_output_sample") != expected_end:
                track_problems.append({"slice_index": slice_index, "field": "end_output_sample"})
            if entry.get("output_sample_count") != expected_end - expected_start:
                track_problems.append({"slice_index": slice_index, "field": "output_sample_count"})
            if entry.get("start_output_sample") != previous_output_end:
                track_problems.append({"slice_index": slice_index, "field": "output_sample_continuity"})
            previous_output_end = entry.get("end_output_sample", previous_output_end)
            slice_dir = engine.slice_roots[track.track_id] / str(entry["slice_relpath"])
            try:
                with wave.open(str(slice_dir / "audio.wav"), "rb") as handle:
                    if handle.getnframes() != expected_end - expected_start:
                        track_problems.append({"slice_index": slice_index, "field": "wav_frame_count"})
                    if handle.getframerate() != track.resample_rate:
                        track_problems.append({"slice_index": slice_index, "field": "wav_sample_rate"})
                    if handle.getnchannels() != engine.audio.channels:
                        track_problems.append({"slice_index": slice_index, "field": "wav_channels"})
            except Exception:
                track_problems.append({"slice_index": slice_index, "field": "wav_file"})
            for field in _check_video_document(
                slice_dir / "video.json", boundary.start_ticks, boundary.end_ticks
            ):
                track_problems.append({"slice_index": slice_index, "field": field})
        if track_problems:
            problems.extend(
                {"scope": "track", "track_id": track.track_id, **problem} for problem in track_problems
            )
        track_summaries[track.track_id] = {
            "status": index_value.get("status"),
            "committed_slices": len(entries),
            "slice_count": len(boundaries),
            "chain_head": index_value.get("chain_head"),
            "problems": len(track_problems),
        }

    snapshot = read_json(engine.snapshot_path) if engine.snapshot_path.exists() else None
    snapshot_last = snapshot.get("last_committed_slice_index", {}) if isinstance(snapshot, dict) else {}
    for track in engine.config.tracks:
        disk_last = len(engine._indexes[track.track_id]["entries"]) - 1
        if snapshot_last.get(track.track_id, -1) < disk_last:
            # A crash between index commit and snapshot refresh is valid; disk index wins.
            pass
    return {
        "ok": not problems,
        "workspace": str(Path(workspace).resolve()),
        "config_version": engine.config_version,
        "boundaries_sha256": engine.boundaries_sha256,
        "slice_count": len(boundaries),
        "source_sample_count": timeline.source_sample_count,
        "ticks_per_second": timeline.ticks_per_second,
        "tracks": dict(sorted(track_summaries.items())),
        "problems": problems,
    }

