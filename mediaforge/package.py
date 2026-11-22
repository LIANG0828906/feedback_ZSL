from __future__ import annotations

import json
import struct
from dataclasses import dataclass, replace
from fractions import Fraction
from pathlib import Path
from typing import Any

from . import ALGORITHM_VERSION
from .errors import (
    E_CONFLICT,
    E_CORRUPT,
    E_IO,
    E_TRACK,
    LossyWarning,
    fail,
)
from .jsonutil import (
    atomic_write_bytes,
    canonical_json_bytes,
    sha256_bytes,
)
from .metadata import (
    MetadataExport,
    export_metadata,
    load_metadata_dict,
    normalize_target_version,
)
from .model import Chapter, ContainerModel, MediaTrack, decode_frame_payload, validate_chapters, validate_model
from .timebase import Timebase


MAGIC = b"MFZ1"
SEGMENT_MAGIC = b"MFS1"
DIGEST_SIZE = 32
PREFIX_SIZE = len(MAGIC) + 8


@dataclass(frozen=True)
class PackageResult:
    artifact: bytes
    artifact_sha256: str
    target_version: str
    warnings: tuple[LossyWarning, ...]
    trace: tuple[dict[str, Any], ...]
    declaration: dict[str, Any]

    def warning_dicts(self) -> list[dict[str, Any]]:
        return [warning.to_dict() for warning in self.warnings]


@dataclass(frozen=True)
class ParsedPackage:
    header: dict[str, Any]
    declaration: dict[str, Any]
    metadata: dict[str, Any]
    payloads: dict[str, bytes]
    artifact_sha256: str
    target_version: str


@dataclass(frozen=True)
class ParsedSegment:
    track_id: str
    declaration: dict[str, Any]
    payload: bytes
    metadata_version: str
    source_version: str
    parameter_version: str
    algorithm_version: str
    segment_sha256: str


def _chapter_values(chapters: tuple[Chapter, ...]) -> list[dict[str, Any]]:
    return [
        {
            "chapter_id": chapter.chapter_id,
            "number": chapter.index,
            "start_ticks": chapter.start_ticks,
            "end_ticks": chapter.end_ticks,
            "title": chapter.title,
            "tags": dict(sorted(chapter.tags.items())),
        }
        for chapter in chapters
    ]


def validate_track_payload(track: MediaTrack, path: str = "$.track") -> None:
    if not track.payload:
        fail(E_TRACK, f"{path}.payload", "track payload is empty")
    if track.kind == "video":
        try:
            frames = decode_frame_payload(track.payload)
        except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
            fail(E_CORRUPT, f"{path}.payload", f"invalid video payload: {exc}")
        if len(frames) != track.sample_count:
            fail(
                E_TRACK,
                f"{path}.sample_count",
                "video payload frame count does not match the track declaration",
                measured=len(frames),
                declared=track.sample_count,
            )
        interval = track.frame_interval_ticks
        if interval is None or interval <= 0:
            fail(E_TRACK, f"{path}.frame_interval_ticks", "video frame interval is required")
        previous_pts: int | None = None
        for index, frame in enumerate(frames):
            body = {
                "pts_ticks": frame.pts_ticks,
                "rows": frame.rows,
                "cols": frame.cols,
                "cells": list(frame.cells),
            }
            expected_hash = sha256_bytes(canonical_json_bytes(body))
            if frame.sha256 != expected_hash:
                fail(
                    E_CORRUPT,
                    f"{path}.frames[{index}].sha256",
                    "video frame hash mismatch",
                    measured=expected_hash,
                    declared=frame.sha256,
                )
            if frame.rows <= 0 or frame.cols <= 0 or len(frame.cells) != frame.rows * frame.cols:
                fail(E_CORRUPT, f"{path}.frames[{index}]", "video frame grid is malformed")
            if track.width is not None and frame.cols != track.width:
                fail(
                    E_CONFLICT,
                    f"{path}.frames[{index}].cols",
                    "frame width conflicts with the track width",
                    measured=frame.cols,
                    declared=track.width,
                )
            if track.height is not None and frame.rows != track.height:
                fail(
                    E_CONFLICT,
                    f"{path}.frames[{index}].rows",
                    "frame height conflicts with the track height",
                    measured=frame.rows,
                    declared=track.height,
                )
            if previous_pts is not None:
                delta = frame.pts_ticks - previous_pts
                if delta <= 0:
                    fail(
                        E_CONFLICT,
                        f"{path}.frames[{index}].pts_ticks",
                        "video timestamps are not strictly increasing",
                        measured=frame.pts_ticks,
                        declared={"previous_pts_ticks": previous_pts},
                    )
                if delta % interval != 0:
                    fail(
                        E_CONFLICT,
                        f"{path}.frames[{index}].pts_ticks",
                        "video timestamps are not aligned to the frame interval",
                        measured=delta,
                        declared={"frame_interval_ticks": interval},
                    )
            previous_pts = frame.pts_ticks
        expected_duration = track.sample_count * interval
        if track.duration_ticks != expected_duration:
            fail(
                E_CONFLICT,
                f"{path}.duration_ticks",
                "video duration conflicts with frame count and frame interval",
                measured=track.duration_ticks,
                declared=expected_duration,
            )
    else:
        if track.channels is None or track.sample_rate is None:
            fail(E_TRACK, path, "audio track requires channels and sample_rate")
        expected_length = track.sample_count * track.channels * 2
        if len(track.payload) != expected_length:
            fail(
                E_TRACK,
                f"{path}.payload",
                "audio payload length does not match the track declaration",
                measured=len(track.payload),
                declared=expected_length,
            )
        expected_duration = track.timebase.ticks_from_seconds(
            Fraction(track.sample_count, track.sample_rate),
            f"{path}.duration_ticks",
        )
        if track.duration_ticks != expected_duration:
            fail(
                E_CONFLICT,
                f"{path}.duration_ticks",
                "audio duration conflicts with sample count and sample rate",
                measured=track.duration_ticks,
                declared=expected_duration,
            )


def _payload_track_declaration(
    track: MediaTrack,
    *,
    offset: int,
) -> dict[str, Any]:
    declaration = track.declaration()
    declaration.update(
        {
            "payload_encoding": track.payload_encoding,
            "payload_sha256": track.payload_sha256,
            "payload_length": len(track.payload),
            "payload_offset": offset,
        }
    )
    return declaration


def _assemble_artifact(
    model: ContainerModel,
    target_version: str,
    metadata_export: MetadataExport,
    payloads: dict[str, bytes],
    *,
    algorithm_version: str = ALGORITHM_VERSION,
) -> PackageResult:
    chapters = validate_chapters(model)
    tracks = sorted(model.tracks, key=lambda item: item.track_id)
    declarations: list[dict[str, Any]] = []
    payload_parts: list[bytes] = []
    offset = 0
    for track in tracks:
        payload = payloads.get(track.track_id)
        if payload is None:
            fail(E_TRACK, f"$.tracks.{track.track_id}", "segment payload is missing")
        if payload != track.payload:
            fail(
                E_CORRUPT,
                f"$.tracks.{track.track_id}.payload_sha256",
                "segment payload differs from the in-memory track",
                measured=sha256_bytes(payload),
                declared=track.payload_sha256,
            )
        declarations.append(_payload_track_declaration(track, offset=offset))
        payload_parts.append(payload)
        offset += len(payload)
    declaration = {
        "track_count": len(declarations),
        "timebase": model.timebase.to_dict(),
        "duration_ticks": model.duration_ticks,
        "tracks": declarations,
        "chapters": _chapter_values(chapters),
        "tags": dict(sorted(model.tags.items())),
        "cover_sha256": sha256_bytes(model.cover) if model.cover else "",
    }
    header = {
        "format": "mediaforge-container",
        "container_version": 1,
        "target_metadata_version": target_version,
        "metadata_version": target_version,
        "source_version": model.source_version,
        "algorithm_version": algorithm_version,
        "model_id": model.model_id,
        "title": model.title,
        "metadata": metadata_export.metadata,
        "declaration": declaration,
        "lossy_warnings": list(metadata_export.warning_dicts()),
        "mapping_trace": list(metadata_export.trace),
    }
    header_bytes = canonical_json_bytes(header)
    prefix = MAGIC + struct.pack("<Q", len(header_bytes))
    body = prefix + header_bytes + b"".join(payload_parts)
    artifact = body + bytes.fromhex(sha256_bytes(body))
    return PackageResult(
        artifact=artifact,
        artifact_sha256=sha256_bytes(artifact),
        target_version=target_version,
        warnings=metadata_export.warnings,
        trace=metadata_export.trace,
        declaration=declaration,
    )


def build_package(
    model: ContainerModel,
    target_version: str,
    *,
    algorithm_version: str = ALGORITHM_VERSION,
) -> PackageResult:
    target = normalize_target_version(target_version)
    validate_model(model, require_payload=True)
    metadata_export = export_metadata(model, target)
    payloads = {track.track_id: track.payload for track in model.tracks}
    return _assemble_artifact(
        model,
        target,
        metadata_export,
        payloads,
        algorithm_version=algorithm_version,
    )


def build_package_from_segments(
    model: ContainerModel,
    target_version: str,
    segments: dict[str, ParsedSegment],
    *,
    algorithm_version: str = ALGORITHM_VERSION,
) -> PackageResult:
    target = normalize_target_version(target_version)
    validate_model(model, require_payload=True)
    metadata_export = export_metadata(model, target)
    payloads: dict[str, bytes] = {}
    for track in model.tracks:
        segment = segments.get(track.track_id)
        if segment is None:
            fail(E_TRACK, f"$.tracks.{track.track_id}", "completed segment is missing")
        if segment.metadata_version != target:
            fail(
                E_CONFLICT,
                f"$.segments.{track.track_id}.metadata_version",
                "segment metadata version does not match the target",
                measured=segment.metadata_version,
                declared=target,
            )
        if segment.algorithm_version != algorithm_version:
            fail(
                E_CONFLICT,
                f"$.segments.{track.track_id}.algorithm_version",
                "segment algorithm version does not match",
                measured=segment.algorithm_version,
                declared=algorithm_version,
            )
        payloads[track.track_id] = segment.payload
    return _assemble_artifact(
        model,
        target,
        metadata_export,
        payloads,
        algorithm_version=algorithm_version,
    )


def write_package(result: PackageResult, path: str | Path) -> None:
    try:
        atomic_write_bytes(path, result.artifact)
    except OSError as exc:
        fail(E_IO, "$.artifact", f"cannot write package: {exc}")


def parse_package_bytes(data: bytes) -> ParsedPackage:
    if len(data) < PREFIX_SIZE + DIGEST_SIZE:
        fail(E_CORRUPT, "$", "package is truncated")
    if data[: len(MAGIC)] != MAGIC:
        fail(E_CORRUPT, "$.magic", "package magic is invalid", measured=data[: len(MAGIC)].hex())
    stored_digest = data[-DIGEST_SIZE:].hex()
    body = data[:-DIGEST_SIZE]
    measured_digest = sha256_bytes(body)
    if measured_digest != stored_digest:
        fail(
            E_CORRUPT,
            "$.integrity.sha256",
            "package digest mismatch",
            measured=measured_digest,
            declared=stored_digest,
        )
    header_length = struct.unpack("<Q", data[len(MAGIC) : PREFIX_SIZE])[0]
    payload_start = PREFIX_SIZE + header_length
    if header_length <= 0 or payload_start > len(body):
        fail(E_CORRUPT, "$.header.length", "package header length is invalid", measured=header_length)
    try:
        header = json.loads(body[PREFIX_SIZE:payload_start].decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(E_CORRUPT, "$.header", f"package header is invalid JSON: {exc}")
    if not isinstance(header, dict) or header.get("format") != "mediaforge-container":
        fail(E_CORRUPT, "$.header.format", "unsupported package header")
    declaration = header.get("declaration")
    if not isinstance(declaration, dict):
        fail(E_CORRUPT, "$.declaration", "package declaration is missing")
    payload_region = body[payload_start:]
    tracks = declaration.get("tracks")
    if not isinstance(tracks, list):
        fail(E_CORRUPT, "$.declaration.tracks", "track declarations are missing")
    payloads: dict[str, bytes] = {}
    intervals: list[tuple[int, int, str]] = []
    for index, track in enumerate(tracks):
        path = f"$.declaration.tracks[{index}]"
        if not isinstance(track, dict):
            fail(E_CORRUPT, path, "track declaration must be an object")
        track_id = track.get("track_id")
        offset = track.get("payload_offset")
        length = track.get("payload_length")
        declared_hash = track.get("payload_sha256")
        if not isinstance(track_id, str) or not track_id:
            fail(E_CORRUPT, f"{path}.track_id", "track id is invalid")
        if not isinstance(offset, int) or not isinstance(length, int) or offset < 0 or length <= 0:
            fail(E_CORRUPT, path, "track payload bounds are invalid")
        if offset + length > len(payload_region):
            fail(
                E_CORRUPT,
                f"{path}.payload_length",
                "track payload exceeds the package body",
                measured=offset + length,
                declared=len(payload_region),
            )
        payload = payload_region[offset : offset + length]
        measured_hash = sha256_bytes(payload)
        if measured_hash != declared_hash:
            fail(
                E_CORRUPT,
                f"{path}.payload_sha256",
                "track payload hash mismatch",
                measured=measured_hash,
                declared=declared_hash,
            )
        if track_id in payloads:
            fail(E_CONFLICT, f"{path}.track_id", "duplicate track payload", measured=track_id)
        payloads[track_id] = payload
        intervals.append((offset, offset + length, track_id))
    cursor = 0
    for start, end, track_id in sorted(intervals):
        if start != cursor:
            fail(
                E_CORRUPT,
                "$.declaration.tracks",
                "payload directory contains a gap or overlap",
                measured={"start": start, "expected_start": cursor, "track_id": track_id},
            )
        cursor = end
    if cursor != len(payload_region):
        fail(
            E_CORRUPT,
            "$.declaration.tracks",
            "payload directory does not cover the complete payload region",
            measured=cursor,
            declared=len(payload_region),
        )
    target_version = str(header.get("target_metadata_version", ""))
    return ParsedPackage(
        header=header,
        declaration=declaration,
        metadata=header.get("metadata", {}),
        payloads=payloads,
        artifact_sha256=sha256_bytes(data),
        target_version=target_version,
    )


def read_package(path: str | Path) -> ParsedPackage:
    try:
        data = Path(path).read_bytes()
    except OSError as exc:
        fail(E_IO, "$.artifact", f"cannot read package: {exc}")
    return parse_package_bytes(data)


def read_artifact_model(path: str | Path) -> ContainerModel:
    parsed = read_package(path)
    target = normalize_target_version(parsed.target_version, "$.target_metadata_version")
    metadata_model = load_metadata_dict(parsed.metadata)
    if metadata_model.source_version != target:
        fail(
            E_CONFLICT,
            "$.target_metadata_version",
            "package target version differs from the embedded metadata version",
            measured=parsed.target_version,
            declared=metadata_model.source_version,
        )
    declarations = {track["track_id"]: track for track in parsed.declaration.get("tracks", [])}
    updated_tracks: list[MediaTrack] = []
    for index, track in enumerate(metadata_model.tracks):
        declaration = declarations.get(track.track_id)
        if declaration is None:
            fail(E_TRACK, f"$.tracks[{index}]", "metadata track has no payload declaration")
        if int(declaration["sample_count"]) != track.sample_count:
            fail(
                E_CONFLICT,
                f"$.tracks[{index}].sample_count",
                "metadata and declaration sample counts conflict",
                measured=track.sample_count,
                declared=declaration["sample_count"],
            )
        if Timebase.from_dict(declaration["timebase"]) != track.timebase:
            fail(
                E_CONFLICT,
                f"$.tracks[{index}].timebase",
                "metadata and declaration timebases conflict",
                measured=track.timebase.to_dict(),
                declared=declaration["timebase"],
            )
        if int(declaration["duration_ticks"]) != track.duration_ticks:
            fail(
                E_CONFLICT,
                f"$.tracks[{index}].duration_ticks",
                "metadata and declaration durations conflict",
                measured=track.duration_ticks,
                declared=declaration["duration_ticks"],
            )
        payload = parsed.payloads[track.track_id]
        loaded_track = track.with_payload(payload, str(declaration.get("payload_encoding", "")))
        validate_track_payload(loaded_track, f"$.tracks[{index}]")
        updated_tracks.append(loaded_track)
    model = metadata_model.copy(tracks=tuple(updated_tracks))
    validate_model(model, require_payload=True)
    return model


def serialize_segment(
    track: MediaTrack,
    model: ContainerModel,
    metadata_version: str,
    parameter_version: str,
    *,
    algorithm_version: str = ALGORITHM_VERSION,
) -> bytes:
    target = normalize_target_version(metadata_version)
    validate_track_payload(track, f"$.tracks.{track.track_id}")
    track_declaration = track.declaration()
    track_declaration["payload_encoding"] = track.payload_encoding
    metadata = {
        "format": "mediaforge-segment",
        "track_id": track.track_id,
        "track": track_declaration,
        "payload_encoding": track.payload_encoding,
        "payload_sha256": track.payload_sha256,
        "payload_length": len(track.payload),
        "metadata_version": target,
        "source_version": model.source_version,
        "parameter_version": parameter_version,
        "algorithm_version": algorithm_version,
    }
    header = canonical_json_bytes(metadata)
    body = SEGMENT_MAGIC + struct.pack("<I", len(header)) + header + track.payload
    return body + bytes.fromhex(sha256_bytes(body))


def parse_segment_bytes(
    data: bytes,
    *,
    expected_track_id: str | None = None,
    expected_metadata_version: str | None = None,
    expected_source_version: str | None = None,
    expected_parameter_version: str | None = None,
    expected_algorithm_version: str | None = None,
) -> ParsedSegment:
    minimum = len(SEGMENT_MAGIC) + 4 + DIGEST_SIZE
    if len(data) < minimum or data[: len(SEGMENT_MAGIC)] != SEGMENT_MAGIC:
        fail(E_CORRUPT, "$.segment", "segment magic or length is invalid")
    stored_digest = data[-DIGEST_SIZE:].hex()
    body = data[:-DIGEST_SIZE]
    measured_digest = sha256_bytes(body)
    if measured_digest != stored_digest:
        fail(
            E_CORRUPT,
            "$.segment.sha256",
            "segment digest mismatch",
            measured=measured_digest,
            declared=stored_digest,
        )
    header_length = struct.unpack("<I", data[len(SEGMENT_MAGIC) : len(SEGMENT_MAGIC) + 4])[0]
    header_start = len(SEGMENT_MAGIC) + 4
    payload_start = header_start + header_length
    if header_length <= 0 or payload_start > len(body):
        fail(E_CORRUPT, "$.segment.header", "segment header length is invalid")
    try:
        header = json.loads(body[header_start:payload_start].decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(E_CORRUPT, "$.segment.header", f"segment header is invalid: {exc}")
    if not isinstance(header, dict) or header.get("format") != "mediaforge-segment":
        fail(E_CORRUPT, "$.segment.format", "unsupported segment format")
    payload = body[payload_start:]
    track_id = str(header.get("track_id", ""))
    if not track_id:
        fail(E_CORRUPT, "$.segment.track_id", "segment track id is missing")
    if expected_track_id is not None and track_id != expected_track_id:
        fail(
            E_CONFLICT,
            "$.segment.track_id",
            "segment track id mismatch",
            measured=track_id,
            declared=expected_track_id,
        )
    expected_versions = {
        "metadata_version": expected_metadata_version,
        "source_version": expected_source_version,
        "parameter_version": expected_parameter_version,
        "algorithm_version": expected_algorithm_version,
    }
    for key, expected in expected_versions.items():
        if expected is not None and header.get(key) != expected:
            fail(
                E_CONFLICT,
                f"$.segment.{key}",
                "segment version mismatch",
                measured=header.get(key),
                declared=expected,
            )
    measured_hash = sha256_bytes(payload)
    declared_hash = header.get("payload_sha256")
    if measured_hash != declared_hash:
        fail(
            E_CORRUPT,
            "$.segment.payload_sha256",
            "segment payload hash mismatch",
            measured=measured_hash,
            declared=declared_hash,
        )
    if int(header.get("payload_length", -1)) != len(payload):
        fail(E_CORRUPT, "$.segment.payload_length", "segment payload length mismatch")
    track = header.get("track")
    if not isinstance(track, dict):
        fail(E_CORRUPT, "$.segment.track", "segment track declaration is missing")
    return ParsedSegment(
        track_id=track_id,
        declaration=track,
        payload=payload,
        metadata_version=str(header.get("metadata_version", "")),
        source_version=str(header.get("source_version", "")),
        parameter_version=str(header.get("parameter_version", "")),
        algorithm_version=str(header.get("algorithm_version", "")),
        segment_sha256=sha256_bytes(data),
    )


def segment_to_model_track(segment: ParsedSegment) -> MediaTrack:
    value = segment.declaration
    timebase = Timebase.from_dict(value["timebase"], "$.segment.track.timebase")
    return MediaTrack(
        track_id=str(value["track_id"]),
        kind=str(value["kind"]),
        language=str(value["language"]),
        timebase=timebase,
        sample_count=int(value["sample_count"]),
        duration_ticks=int(value["duration_ticks"]),
        codec=str(value["codec"]),
        payload=segment.payload,
        payload_encoding=str(value.get("payload_encoding", "")),
        width=value.get("width"),
        height=value.get("height"),
        frame_interval_ticks=value.get("frame_interval_ticks"),
        sample_rate=value.get("sample_rate"),
        channels=value.get("channels"),
        sample_format=value.get("sample_format"),
        languages=tuple(value.get("languages", ())),
        roles=tuple(value.get("roles", [])),
        hdr=value.get("hdr"),
        custom=dict(value.get("custom", {})),
    )
