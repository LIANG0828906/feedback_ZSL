from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import ErrorCode, build_error
from .models import TrackConfig
from .util import read_json, sha256_file, sha256_obj, sha256_text, write_json_atomic


def entry_content_material(entry: dict[str, Any]) -> dict[str, Any]:
    keys = (
        "track_id",
        "slice_index",
        "start_ticks",
        "end_ticks",
        "start_source_sample",
        "end_source_sample",
        "start_output_sample",
        "end_output_sample",
        "audio_start_ticks",
        "audio_end_ticks",
        "video_start_ticks",
        "video_end_ticks",
        "audio_sha256",
        "video_sha256",
        "track_params_version",
        "boundaries_sha256",
    )
    return {key: entry.get(key) for key in keys}


def compute_content_hash(entry: dict[str, Any]) -> str:
    return sha256_obj(entry_content_material(entry))


def compute_chain_hash(
    previous_chain_hash: str,
    content_hash: str,
    track_params_version: str,
    boundaries_sha256: str,
) -> str:
    return sha256_text(
        f"{previous_chain_hash}|{content_hash}|{track_params_version}|{boundaries_sha256}"
    )


@dataclass
class LoadedTrackIndex:
    status: str
    entries: list[dict[str, Any]]
    chain_head: str
    failure: dict[str, Any] | None


def new_index_value(track: TrackConfig, boundaries_sha256: str, config_version: str) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "track_id": track.track_id,
        "track_params_version": track.params_version,
        "track_config": track.raw,
        "boundaries_sha256": boundaries_sha256,
        "config_version": config_version,
        "status": "pending",
        "failure": None,
        "chain_head": "GENESIS",
        "entries": [],
    }


def write_track_index(path: Path, value: dict[str, Any]) -> None:
    write_json_atomic(path, value)


def load_track_index(
    path: Path,
    track: TrackConfig,
    boundaries_sha256: str,
    config_version: str,
    slices_root: Path,
    verify_files: bool = True,
) -> LoadedTrackIndex:
    if not path.exists():
        return LoadedTrackIndex(status="pending", entries=[], chain_head="GENESIS", failure=None)
    raw = read_json(path)
    if not isinstance(raw, dict) or raw.get("schema_version") != 1:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "track index schema is invalid",
            track_id=track.track_id,
            path=str(path),
        )
    if raw.get("track_id") != track.track_id:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "track index id changed",
            track_id=track.track_id,
            found=raw.get("track_id"),
        )
    if raw.get("track_params_version") != track.params_version:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "track parameters changed",
            track_id=track.track_id,
            expected=track.params_version,
            found=raw.get("track_params_version"),
            field="track_params_version",
        )
    if raw.get("boundaries_sha256") != boundaries_sha256:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "slice boundary table changed",
            track_id=track.track_id,
            expected=boundaries_sha256,
            found=raw.get("boundaries_sha256"),
            field="boundaries_sha256",
        )
    if raw.get("config_version") != config_version:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "configuration version changed",
            track_id=track.track_id,
            expected=config_version,
            found=raw.get("config_version"),
            field="config_version",
        )
    entries_raw = raw.get("entries")
    if not isinstance(entries_raw, list):
        raise build_error(ErrorCode.SNAPSHOT_MISMATCH, "track index entries are invalid", track_id=track.track_id)

    entries: list[dict[str, Any]] = []
    previous_chain = "GENESIS"
    for expected_index, entry in enumerate(entries_raw):
        if not isinstance(entry, dict):
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "track index entry is not an object",
                track_id=track.track_id,
                expected_slice=expected_index,
            )
        if entry.get("slice_index") != expected_index:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "track index has a slice gap or duplicate",
                track_id=track.track_id,
                expected_slice=expected_index,
                found_slice=entry.get("slice_index"),
                field="slice_index",
            )
        if entry.get("track_id") != track.track_id:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "track index entry belongs to a different track",
                track_id=track.track_id,
                slice_index=expected_index,
                found=entry.get("track_id"),
                field="track_id",
            )
        content_hash = compute_content_hash(entry)
        if entry.get("content_hash") != content_hash:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "slice index content hash mismatch",
                track_id=track.track_id,
                slice_index=expected_index,
                field="content_hash",
            )
        expected_chain = compute_chain_hash(
            previous_chain,
            content_hash,
            track.params_version,
            boundaries_sha256,
        )
        if entry.get("chain_hash") != expected_chain:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "slice index hash chain mismatch",
                track_id=track.track_id,
                slice_index=expected_index,
                field="chain_hash",
            )
        slice_relpath = entry.get("slice_relpath")
        if not isinstance(slice_relpath, str):
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "slice path is missing from index",
                track_id=track.track_id,
                slice_index=expected_index,
            )
        slice_dir = slices_root / slice_relpath
        audio_path = slice_dir / "audio.wav"
        video_path = slice_dir / "video.json"
        if verify_files:
            if not audio_path.is_file() or not video_path.is_file():
                raise build_error(
                    ErrorCode.SNAPSHOT_MISMATCH,
                    "committed slice files are missing",
                    track_id=track.track_id,
                    slice_index=expected_index,
                    slice_path=str(slice_dir),
                )
            if sha256_file(audio_path) != entry.get("audio_sha256"):
                raise build_error(
                    ErrorCode.SNAPSHOT_MISMATCH,
                    "committed audio slice hash mismatch",
                    track_id=track.track_id,
                    slice_index=expected_index,
                    field="audio_sha256",
                )
            if sha256_file(video_path) != entry.get("video_sha256"):
                raise build_error(
                    ErrorCode.SNAPSHOT_MISMATCH,
                    "committed video slice hash mismatch",
                    track_id=track.track_id,
                    slice_index=expected_index,
                    field="video_sha256",
                )
        entries.append(dict(entry))
        previous_chain = expected_chain
    chain_head = raw.get("chain_head", "GENESIS")
    if chain_head != previous_chain:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "track index chain head mismatch",
            track_id=track.track_id,
            expected=previous_chain,
            found=chain_head,
            field="chain_head",
        )
    return LoadedTrackIndex(
        status=str(raw.get("status", "pending")),
        entries=entries,
        chain_head=chain_head,
        failure=raw.get("failure"),
    )

