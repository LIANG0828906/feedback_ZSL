from __future__ import annotations

import base64
import json
import struct
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Any

from .errors import E_CORRUPT, E_IO, E_META, E_VALIDATE, E_VER, fail
from .jsonutil import canonical_json_bytes, decode_base64, sha256_bytes
from .timebase import Timebase


MAGIC = b"MFZ1"
DIGEST_SIZE = 32


@dataclass
class ValidationResult:
    ok: bool
    target_version: str
    artifact_sha256: str
    truth_fingerprint: str
    checks: list[dict[str, Any]] = field(default_factory=list)
    error: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": self.ok,
            "target_version": self.target_version,
            "artifact_sha256": self.artifact_sha256,
            "truth_fingerprint": self.truth_fingerprint,
            "check_count": len(self.checks),
            "error": self.error,
        }


def _truth_fingerprint(value: dict[str, Any]) -> str:
    body = dict(value)
    body.pop("fingerprint", None)
    return sha256_bytes(canonical_json_bytes(body))


def _load_truth(path: str | Path) -> dict[str, Any]:
    try:
        value = json.loads(Path(path).read_text(encoding="utf-8"))
    except OSError as exc:
        fail(E_IO, "$.truth", f"cannot read truth manifest: {exc}")
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(E_CORRUPT, "$.truth", f"truth manifest is invalid JSON: {exc}")
    if not isinstance(value, dict):
        fail(E_META, "$.truth", "truth manifest must be an object")
    declared = value.get("fingerprint")
    if not isinstance(declared, str):
        fail(E_META, "$.truth.fingerprint", "truth fingerprint is missing")
    measured = _truth_fingerprint(value)
    if measured != declared:
        fail(
            E_CORRUPT,
            "$.truth.fingerprint",
            "truth fingerprint mismatch",
            measured=measured,
            declared=declared,
        )
    return value


def _mismatch(
    path: str,
    measured: Any,
    declared: Any,
    message: str,
) -> None:
    fail(
        E_VALIDATE,
        path,
        message,
        measured=measured,
        declared=declared,
    )


def _parse_timebase(value: Any, path: str) -> Timebase:
    if not isinstance(value, dict):
        fail(E_CORRUPT, path, "timebase must be an object")
    try:
        return Timebase(int(value["num"]), int(value["den"]))
    except (KeyError, TypeError, ValueError) as exc:
        fail(E_CORRUPT, path, f"invalid timebase: {exc}")


def _decode_payload_tracks(
    declaration: dict[str, Any],
    payload_region: bytes,
) -> dict[str, dict[str, Any]]:
    raw_declarations = declaration.get("tracks")
    if not isinstance(raw_declarations, list):
        fail(E_CORRUPT, "$.declaration.tracks", "track declarations are missing")
    tracks: dict[str, dict[str, Any]] = {}
    intervals: list[tuple[int, int, str]] = []
    for index, declared in enumerate(raw_declarations):
        path = f"$.declaration.tracks[{index}]"
        if not isinstance(declared, dict):
            fail(E_CORRUPT, path, "track declaration must be an object")
        track_id = declared.get("track_id")
        if not isinstance(track_id, str) or not track_id:
            fail(E_CORRUPT, f"{path}.track_id", "track id is missing")
        if track_id in tracks:
            fail(E_CORRUPT, f"{path}.track_id", "track id is duplicated", measured=track_id)
        offset = declared.get("payload_offset")
        length = declared.get("payload_length")
        if not isinstance(offset, int) or not isinstance(length, int) or offset < 0 or length <= 0:
            fail(E_CORRUPT, path, "payload bounds are invalid")
        if offset + length > len(payload_region):
            fail(E_CORRUPT, f"{path}.payload_length", "payload exceeds package body")
        payload = payload_region[offset : offset + length]
        measured_hash = sha256_bytes(payload)
        if measured_hash != declared.get("payload_sha256"):
            fail(
                E_CORRUPT,
                f"{path}.payload_sha256",
                "payload hash mismatch",
                measured=measured_hash,
                declared=declared.get("payload_sha256"),
            )
        kind = declared.get("kind")
        timebase = _parse_timebase(declared.get("timebase"), f"{path}.timebase")
        if kind == "video":
            actual_count, duration = _decode_video(payload, declared, path, timebase)
        elif kind == "audio":
            actual_count, duration = _decode_audio(payload, declared, path)
        else:
            fail(E_CORRUPT, f"{path}.kind", "unsupported track kind", measured=kind)
        tracks[track_id] = {
            "kind": kind,
            "sample_count": actual_count,
            "duration_seconds": duration,
            "timebase": timebase,
            "language": str(declared.get("language", "")),
            "payload_sha256": measured_hash,
            "declared": declared,
        }
        intervals.append((offset, offset + length, track_id))
    cursor = 0
    for start, end, track_id in sorted(intervals):
        if start != cursor:
            fail(
                E_CORRUPT,
                "$.declaration.tracks",
                "payload region contains gaps or overlaps",
                measured={"start": start, "expected_start": cursor, "track_id": track_id},
            )
        cursor = end
    if cursor != len(payload_region):
        fail(
            E_CORRUPT,
            "$.declaration.tracks",
            "payload region contains gaps or overlapping tracks",
            measured=cursor,
            declared=len(payload_region),
        )
    return tracks


def _decode_video(
    payload: bytes,
    declared: dict[str, Any],
    path: str,
    timebase: Timebase,
) -> tuple[int, Fraction]:
    try:
        frames = json.loads(payload.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(E_CORRUPT, f"{path}.payload", f"video payload is invalid: {exc}")
    if not isinstance(frames, list):
        fail(E_CORRUPT, f"{path}.payload", "video payload must be a frame array")
    interval = declared.get("frame_interval_ticks")
    if not isinstance(interval, int) or interval <= 0:
        fail(E_CORRUPT, f"{path}.frame_interval_ticks", "frame interval is invalid")
    previous: int | None = None
    for index, frame in enumerate(frames):
        if not isinstance(frame, dict):
            fail(E_CORRUPT, f"{path}.frames[{index}]", "frame must be an object")
        pts = frame.get("pts_ticks")
        if not isinstance(pts, int):
            fail(E_CORRUPT, f"{path}.frames[{index}].pts_ticks", "frame timestamp is invalid")
        body = {
            "pts_ticks": pts,
            "rows": frame.get("rows"),
            "cols": frame.get("cols"),
            "cells": frame.get("cells"),
        }
        expected_hash = sha256_bytes(canonical_json_bytes(body))
        if frame.get("sha256") != expected_hash:
            fail(
                E_CORRUPT,
                f"{path}.frames[{index}].sha256",
                "frame hash mismatch",
                measured=expected_hash,
                declared=frame.get("sha256"),
            )
        if previous is not None and (pts <= previous or (pts - previous) % interval != 0):
            fail(
                E_CORRUPT,
                f"{path}.frames[{index}].pts_ticks",
                "frame timestamps are not monotonic and interval-aligned",
                measured=pts,
                declared={"previous_pts_ticks": previous, "interval": interval},
            )
        previous = pts
    duration = Fraction(len(frames) * interval * timebase.num, timebase.den)
    return len(frames), duration


def _decode_audio(
    payload: bytes,
    declared: dict[str, Any],
    path: str,
) -> tuple[int, Fraction]:
    channels = declared.get("channels")
    sample_rate = declared.get("sample_rate")
    if not isinstance(channels, int) or not isinstance(sample_rate, int) or channels <= 0 or sample_rate <= 0:
        fail(E_CORRUPT, path, "audio channels or sample rate is invalid")
    frame_size = channels * 2
    if len(payload) % frame_size != 0:
        fail(
            E_CORRUPT,
            f"{path}.payload",
            "audio payload is not sample-frame aligned",
            measured=len(payload),
            declared={"bytes_per_frame": frame_size},
        )
    count = len(payload) // frame_size
    return count, Fraction(count, sample_rate)


def _parse_artifact(data: bytes) -> tuple[dict[str, Any], dict[str, Any], bytes]:
    if len(data) < len(MAGIC) + 8 + DIGEST_SIZE or data[: len(MAGIC)] != MAGIC:
        fail(E_CORRUPT, "$.header", "package magic or length is invalid")
    stored = data[-DIGEST_SIZE:].hex()
    body = data[:-DIGEST_SIZE]
    measured = sha256_bytes(body)
    if measured != stored:
        fail(
            E_CORRUPT,
            "$.integrity.sha256",
            "package digest mismatch",
            measured=measured,
            declared=stored,
        )
    header_len = struct.unpack("<Q", data[len(MAGIC) : len(MAGIC) + 8])[0]
    payload_start = len(MAGIC) + 8 + header_len
    if header_len <= 0 or payload_start > len(body):
        fail(E_CORRUPT, "$.header.length", "package header length is invalid")
    try:
        header = json.loads(body[len(MAGIC) + 8 : payload_start].decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(E_CORRUPT, "$.header", f"package header is invalid: {exc}")
    if not isinstance(header, dict) or header.get("format") != "mediaforge-container":
        fail(E_CORRUPT, "$.header.format", "unsupported container")
    declaration = header.get("declaration")
    if not isinstance(declaration, dict):
        fail(E_CORRUPT, "$.declaration", "declaration is missing")
    return header, declaration, body[payload_start:]


def _metadata_tracks(metadata: dict[str, Any], version: str) -> dict[str, dict[str, Any]]:
    result: dict[str, dict[str, Any]] = {}
    if version == "v1":
        for index, track in enumerate(metadata.get("tracks", [])):
            if not isinstance(track, dict):
                fail(E_CORRUPT, f"$.metadata.tracks[{index}]", "track metadata is invalid")
            result[str(track.get("track_id"))] = {
                "sample_count": track.get("sample_count"),
                "timebase": _parse_timebase(track.get("timebase"), f"$.metadata.tracks[{index}].timebase"),
                "language": track.get("language"),
            }
        return result
    if version == "v2":
        for index, track in enumerate(metadata.get("tracks", [])):
            if not isinstance(track, dict):
                fail(E_CORRUPT, f"$.metadata.tracks[{index}]", "track metadata is invalid")
            result[str(track.get("track_id"))] = {
                "sample_count": track.get("sample_count"),
                "timebase": _parse_timebase(track.get("timebase"), f"$.metadata.tracks[{index}].timebase"),
                "language": track.get("language_tag"),
            }
        return result
    if version == "v3":
        for index, track in enumerate(metadata.get("tracks", [])):
            if not isinstance(track, dict):
                fail(E_CORRUPT, f"$.metadata.tracks[{index}]", "track metadata is invalid")
            result[str(track.get("track_id"))] = {
                "sample_count": track.get("sample_total"),
                "timebase": _parse_timebase(track.get("time_base"), f"$.metadata.tracks[{index}].time_base"),
                "language": track.get("primary_language"),
            }
        return result
    fail(E_VER, "$.metadata.metadata_version", "unknown embedded metadata version", measured=version)


def _metadata_duration(metadata: dict[str, Any], version: str) -> tuple[Timebase, Fraction, int]:
    if version == "v1":
        timebase = _parse_timebase(metadata.get("timeline_timebase"), "$.metadata.timeline_timebase")
        duration_ms = metadata.get("duration_ms")
        if not isinstance(duration_ms, int):
            fail(E_CORRUPT, "$.metadata.duration_ms", "duration is invalid")
        return timebase, Fraction(duration_ms, 1000), 0
    if version == "v2":
        timeline = metadata.get("timeline")
        if not isinstance(timeline, dict):
            fail(E_CORRUPT, "$.metadata.timeline", "timeline is missing")
        timebase = _parse_timebase(timeline.get("timebase"), "$.metadata.timeline.timebase")
        ticks = timeline.get("duration_ticks")
        if not isinstance(ticks, int):
            fail(E_CORRUPT, "$.metadata.timeline.duration_ticks", "duration is invalid")
        return timebase, timebase.seconds(ticks), ticks
    if version == "v3":
        timeline = metadata.get("timeline")
        if not isinstance(timeline, dict):
            fail(E_CORRUPT, "$.metadata.timeline", "timeline is missing")
        timebase = _parse_timebase(timeline.get("timebase"), "$.metadata.timeline.timebase")
        duration_us = timeline.get("duration_us")
        if not isinstance(duration_us, int):
            fail(E_CORRUPT, "$.metadata.timeline.duration_us", "duration is invalid")
        return timebase, Fraction(duration_us, 1_000_000), 0
    fail(E_VER, "$.metadata.metadata_version", "unknown embedded metadata version", measured=version)


def _metadata_chapters(
    metadata: dict[str, Any],
    version: str,
    model_timebase: Timebase,
) -> list[dict[str, Any]]:
    chapters: list[dict[str, Any]] = []
    if version == "v1":
        timebase = _parse_timebase(metadata.get("timeline_timebase"), "$.metadata.timeline_timebase")
        for item in metadata.get("chapters", []):
            chapters.append(
                {
                    "chapter_id": item.get("chapter_id"),
                    "start_ticks": model_timebase.ticks_from_seconds(
                        Fraction(int(item["start_ms"]), 1000),
                        "$.metadata.chapters",
                    ),
                    "end_ticks": model_timebase.ticks_from_seconds(
                        Fraction(int(item["end_ms"]), 1000),
                        "$.metadata.chapters",
                    ),
                }
            )
        return chapters
    if version == "v2":
        timeline = metadata.get("timeline", {})
        timebase = _parse_timebase(timeline.get("timebase"), "$.metadata.timeline.timebase")
        records = metadata.get("chapters", {})
        for key in sorted(records):
            item = records[key]
            chapters.append(
                {
                    "chapter_id": item.get("chapter_id", key),
                    "start_ticks": timebase.convert_ticks(int(item["start_ticks"]), model_timebase, "$.metadata.chapters"),
                    "end_ticks": timebase.convert_ticks(int(item["end_ticks"]), model_timebase, "$.metadata.chapters"),
                }
            )
        return sorted(
            chapters,
            key=lambda item: (item["start_ticks"], item["end_ticks"], str(item["chapter_id"])),
        )
    if version == "v3":
        chapter_tb = _parse_timebase(metadata.get("chapter_timebase"), "$.metadata.chapter_timebase")
        for item in metadata.get("chapter_table", []):
            chapters.append(
                {
                    "chapter_id": item.get("chapter_id"),
                    "start_ticks": chapter_tb.convert_ticks(int(item["from_frame"]), model_timebase, "$.metadata.chapter_table"),
                    "end_ticks": chapter_tb.convert_ticks(int(item["to_frame"]), model_timebase, "$.metadata.chapter_table"),
                }
            )
        return chapters
    fail(E_VER, "$.metadata.metadata_version", "unknown embedded metadata version", measured=version)


def _metadata_tags(metadata: dict[str, Any], version: str) -> dict[str, Any]:
    if version == "v1":
        tags = metadata.get("tags", {})
        return dict(tags) if isinstance(tags, dict) else {}
    if version == "v2":
        section = metadata.get("metadata", {})
        tags = section.get("tags", {}) if isinstance(section, dict) else {}
        return dict(tags) if isinstance(tags, dict) else {}
    if version == "v3":
        tags: dict[str, Any] = {}
        for item in metadata.get("custom_tags", []):
            if isinstance(item, dict) and isinstance(item.get("key"), str):
                tags[item["key"]] = item.get("value")
        return tags
    fail(E_VER, "$.metadata.metadata_version", "unknown embedded metadata version", measured=version)


def _metadata_cover(metadata: dict[str, Any], version: str) -> bytes:
    if version == "v1":
        encoded = metadata.get("cover", {}).get("data", "")
    elif version == "v2":
        encoded = metadata.get("assets", {}).get("cover", {}).get("base64", "")
    elif version == "v3":
        encoded = metadata.get("resources", {}).get("cover", {}).get("base64", "")
    else:
        fail(E_VER, "$.metadata.metadata_version", "unknown embedded metadata version", measured=version)
    if not encoded:
        return b""
    try:
        return base64.b64decode(str(encoded), validate=True)
    except (ValueError, TypeError) as exc:
        fail(E_CORRUPT, "$.metadata.cover", f"cover is invalid base64: {exc}")


def _compare(a: Any, b: Any) -> bool:
    return canonical_json_bytes(a) == canonical_json_bytes(b)


def validate_artifact(
    artifact_path: str | Path,
    truth_path: str | Path,
    *,
    include_checks: bool = True,
) -> ValidationResult:
    try:
        data = Path(artifact_path).read_bytes()
    except OSError as exc:
        fail(E_IO, "$.artifact", f"cannot read package: {exc}")
    truth = _load_truth(truth_path)
    return validate_artifact_bytes(data, truth, include_checks=include_checks)


def validate_artifact_bytes(
    data: bytes,
    truth: dict[str, Any],
    *,
    include_checks: bool = True,
) -> ValidationResult:
    artifact_sha256 = sha256_bytes(data)
    truth_fingerprint = str(truth.get("fingerprint", ""))
    target_version = ""
    checks: list[dict[str, Any]] = []
    try:
        header, declaration, payload_region = _parse_artifact(data)
        target_version = str(header.get("target_metadata_version", ""))
        if target_version not in {"v1", "v2", "v3"}:
            fail(E_VER, "$.metadata_version", "unknown package target version", measured=target_version)
        if header.get("metadata_version") != target_version:
            _mismatch(
                "$.metadata_version",
                header.get("metadata_version"),
                target_version,
                "embedded metadata version differs from the target version",
            )
        metadata = header.get("metadata")
        if not isinstance(metadata, dict):
            fail(E_CORRUPT, "$.metadata", "embedded metadata is missing")
        model_timebase = _parse_timebase(declaration.get("timebase"), "$.declaration.timebase")
        actual_tracks = _decode_payload_tracks(declaration, payload_region)
        checks.append({"check": "payload_tracks", "track_count": len(actual_tracks)})

        declared_track_count = declaration.get("track_count")
        if declared_track_count != len(actual_tracks):
            _mismatch(
                "$.declaration.track_count",
                len(actual_tracks),
                declared_track_count,
                "decoded track count differs from the manifest declaration",
            )
        metadata_tracks = _metadata_tracks(metadata, target_version)
        if set(metadata_tracks) != set(actual_tracks):
            missing = sorted(set(actual_tracks) - set(metadata_tracks))
            extra = sorted(set(metadata_tracks) - set(actual_tracks))
            _mismatch(
                "$.metadata.tracks",
                {"missing": missing, "extra": extra},
                sorted(actual_tracks),
                "embedded metadata track set differs from the payload tracks",
            )
        for index, (track_id, actual) in enumerate(actual_tracks.items()):
            declared = actual["declared"]
            if actual["sample_count"] != declared.get("sample_count"):
                _mismatch(
                    f"$.tracks[{index}].sample_count",
                    actual["sample_count"],
                    declared.get("sample_count"),
                    "decoded sample/frame count differs from the manifest declaration",
                )
            meta_track = metadata_tracks[track_id]
            if meta_track["sample_count"] != actual["sample_count"]:
                _mismatch(
                    f"$.metadata.tracks[{index}].sample_count",
                    meta_track["sample_count"],
                    actual["sample_count"],
                    "embedded metadata sample/frame count differs from the payload",
                )
            if meta_track["timebase"] != actual["timebase"]:
                _mismatch(
                    f"$.metadata.tracks[{index}].timebase",
                    meta_track["timebase"].to_dict(),
                    actual["timebase"].to_dict(),
                    "embedded metadata timebase differs from the payload declaration",
                )
            if meta_track["language"] != actual["language"]:
                _mismatch(
                    f"$.metadata.tracks[{index}].language",
                    meta_track["language"],
                    actual["language"],
                    "embedded metadata language differs from the payload declaration",
                )
        checks.append({"check": "payload_sample_counts", "tracks": len(actual_tracks)})

        actual_duration = max(track["duration_seconds"] for track in actual_tracks.values())
        declared_duration = model_timebase.seconds(int(declaration.get("duration_ticks", -1)))
        if actual_duration != declared_duration:
            _mismatch(
                "$.declaration.duration_ticks",
                {"numerator": actual_duration.numerator, "denominator": actual_duration.denominator},
                {"ticks": declaration.get("duration_ticks"), "timebase": model_timebase.to_dict()},
                "decoded duration differs from the manifest declaration",
            )
        meta_tb, meta_duration, meta_ticks = _metadata_duration(metadata, target_version)
        if meta_duration != actual_duration:
            _mismatch(
                "$.metadata.duration",
                {"numerator": actual_duration.numerator, "denominator": actual_duration.denominator},
                {"numerator": meta_duration.numerator, "denominator": meta_duration.denominator},
                "embedded metadata duration differs from the payload",
            )
        if meta_tb != model_timebase:
            _mismatch(
                "$.metadata.timebase",
                meta_tb.to_dict(),
                model_timebase.to_dict(),
                "embedded metadata timebase differs from the declaration",
            )
        checks.append({"check": "duration", "seconds": str(actual_duration)})

        actual_chapters = _metadata_chapters(metadata, target_version, model_timebase)
        declared_chapters = [
            {
                "chapter_id": item.get("chapter_id"),
                "start_ticks": item.get("start_ticks"),
                "end_ticks": item.get("end_ticks"),
            }
            for item in declaration.get("chapters", [])
        ]
        if len(actual_chapters) != len(declared_chapters):
            _mismatch(
                "$.chapters.length",
                len(actual_chapters),
                len(declared_chapters),
                "decoded chapter count differs from the manifest declaration",
            )
        for index, (actual, declared) in enumerate(zip(actual_chapters, declared_chapters)):
            for field_name in ("chapter_id", "start_ticks", "end_ticks"):
                if actual.get(field_name) != declared.get(field_name):
                    _mismatch(
                        f"$.chapters[{index}].{field_name}",
                        actual.get(field_name),
                        declared.get(field_name),
                        "decoded chapter value differs from the manifest declaration",
                    )
        checks.append({"check": "chapters", "count": len(actual_chapters)})

        actual_tags = _metadata_tags(metadata, target_version)
        declared_tags = declaration.get("tags", {})
        if not _compare(actual_tags, declared_tags):
            first_key = next(
                key
                for key in sorted(set(actual_tags) | set(declared_tags))
                if actual_tags.get(key) != declared_tags.get(key)
            )
            _mismatch(
                f"$.tags.{first_key}",
                actual_tags.get(first_key),
                declared_tags.get(first_key),
                "decoded tag set differs from the manifest declaration",
            )
        checks.append({"check": "tags", "count": len(actual_tags)})

        cover = _metadata_cover(metadata, target_version)
        actual_cover_hash = sha256_bytes(cover) if cover else ""
        declared_cover_hash = declaration.get("cover_sha256", "")
        if actual_cover_hash != declared_cover_hash:
            _mismatch(
                "$.cover_sha256",
                actual_cover_hash,
                declared_cover_hash,
                "decoded cover hash differs from the manifest declaration",
            )
        checks.append({"check": "cover", "sha256": actual_cover_hash})

        truth_tracks = {str(item["track_id"]): item for item in truth.get("tracks", [])}
        if len(truth_tracks) != len(actual_tracks):
            _mismatch(
                "$.truth.track_count",
                len(actual_tracks),
                len(truth_tracks),
                "decoded track count differs from the truth manifest",
            )
        for index, (track_id, actual) in enumerate(actual_tracks.items()):
            expected = truth_tracks.get(track_id)
            if expected is None:
                _mismatch(f"$.tracks[{index}].track_id", track_id, None, "track is absent from truth")
            if actual["sample_count"] != expected.get("sample_count"):
                _mismatch(
                    f"$.tracks[{index}].sample_count",
                    actual["sample_count"],
                    expected.get("sample_count"),
                    "decoded sample/frame count differs from truth",
                )
            expected_tb = _parse_timebase(expected.get("timebase"), f"$.truth.tracks[{index}].timebase")
            if actual["timebase"] != expected_tb:
                _mismatch(
                    f"$.tracks[{index}].timebase",
                    actual["timebase"].to_dict(),
                    expected_tb.to_dict(),
                    "decoded timebase differs from truth",
                )
            if expected.get("language") and actual["language"] != expected.get("language"):
                _mismatch(
                    f"$.tracks[{index}].language",
                    actual["language"],
                    expected.get("language"),
                    "decoded language differs from truth",
                )
            if actual["payload_sha256"] != expected.get("payload_sha256"):
                _mismatch(
                    f"$.tracks[{index}].payload_sha256",
                    actual["payload_sha256"],
                    expected.get("payload_sha256"),
                    "decoded payload hash differs from truth",
                )
        checks.append({"check": "truth_tracks", "count": len(actual_tracks)})

        truth_tb = _parse_timebase(truth.get("timebase"), "$.truth.timebase")
        truth_duration = truth_tb.seconds(int(truth.get("duration_ticks", -1)))
        if actual_duration != truth_duration:
            _mismatch(
                "$.duration_seconds",
                {"numerator": actual_duration.numerator, "denominator": actual_duration.denominator},
                {"numerator": truth_duration.numerator, "denominator": truth_duration.denominator},
                "decoded duration differs from truth",
            )

        truth_chapters = {str(item["chapter_id"]): item for item in truth.get("chapters", [])}
        if len(truth_chapters) != len(actual_chapters):
            _mismatch(
                "$.chapters.length",
                len(actual_chapters),
                len(truth_chapters),
                "decoded chapter count differs from truth",
            )
        for index, actual in enumerate(actual_chapters):
            chapter_id = str(actual["chapter_id"])
            expected = truth_chapters.get(chapter_id)
            if expected is None:
                _mismatch(f"$.chapters[{index}].chapter_id", chapter_id, None, "chapter is absent from truth")
            if (
                actual["start_ticks"] != expected.get("start_ticks")
                or actual["end_ticks"] != expected.get("end_ticks")
            ):
                _mismatch(
                    f"$.chapters[{index}]",
                    {"start_ticks": actual["start_ticks"], "end_ticks": actual["end_ticks"]},
                    {"start_ticks": expected.get("start_ticks"), "end_ticks": expected.get("end_ticks")},
                    "decoded chapter interval differs from truth",
                )
        checks.append({"check": "truth_chapters", "count": len(actual_chapters)})

        truth_tags = dict(truth.get("tags", {}))
        if not _compare(actual_tags, truth_tags):
            first_key = next(
                key
                for key in sorted(set(actual_tags) | set(truth_tags))
                if actual_tags.get(key) != truth_tags.get(key)
            )
            _mismatch(
                f"$.tags.{first_key}",
                actual_tags.get(first_key),
                truth_tags.get(first_key),
                "decoded tag set differs from truth",
            )
        checks.append({"check": "truth_tags", "count": len(actual_tags)})

        truth_cover_hash = str(truth.get("cover_sha256", ""))
        if actual_cover_hash != truth_cover_hash:
            _mismatch(
                "$.cover_sha256",
                actual_cover_hash,
                truth_cover_hash,
                "decoded cover hash differs from truth",
            )
        return ValidationResult(
            ok=True,
            target_version=target_version,
            artifact_sha256=artifact_sha256,
            truth_fingerprint=truth_fingerprint,
            checks=checks if include_checks else [],
            error=None,
        )
    except Exception as exc:
        if hasattr(exc, "to_dict"):
            error = exc.to_dict()
        else:
            error = {
                "code": E_VALIDATE,
                "field_path": "$",
                "message": str(exc),
            }
        return ValidationResult(
            ok=False,
            target_version=target_version,
            artifact_sha256=artifact_sha256,
            truth_fingerprint=truth_fingerprint,
            checks=checks if include_checks else [],
            error=error,
        )
