from __future__ import annotations

from pathlib import Path
from typing import Any

from .errors import ErrorCode, build_error
from .index import compute_chain_hash, compute_content_hash
from .util import read_json, write_json_atomic

SNAPSHOT_SCHEMA_VERSION = 1


def build_snapshot(
    config_version: str,
    boundaries_sha256: str,
    boundaries: dict[str, Any],
    track_params_versions: dict[str, str],
    track_indexes: dict[str, dict[str, Any]],
    memory: dict[str, Any],
    waiting_queue: list[dict[str, Any]],
    committed_total: int,
) -> dict[str, Any]:
    tracks: dict[str, Any] = {}
    last_committed: dict[str, int] = {}
    for track_id, index_value in sorted(track_indexes.items()):
        entries = [dict(entry) for entry in index_value.get("entries", [])]
        tracks[track_id] = {
            "status": index_value.get("status", "pending"),
            "chain_head": index_value.get("chain_head", "GENESIS"),
            "entries": entries,
            "failure": index_value.get("failure"),
        }
        last_committed[track_id] = len(entries) - 1
    return {
        "schema_version": SNAPSHOT_SCHEMA_VERSION,
        "config_version": config_version,
        "boundaries_sha256": boundaries_sha256,
        "boundaries": boundaries,
        "track_params_versions": dict(sorted(track_params_versions.items())),
        "last_committed_slice_index": dict(sorted(last_committed.items())),
        "tracks": tracks,
        "memory": {
            "global_budget_bytes": memory["global_budget_bytes"],
            "reserved_bytes": memory["reserved_bytes"],
            "high_water_bytes": memory["high_water_bytes"],
        },
        "waiting_queue": waiting_queue,
        "committed_total": committed_total,
    }


def validate_snapshot(
    snapshot: Any,
    config_version: str,
    boundaries_sha256: str,
    track_params_versions: dict[str, str],
) -> dict[str, Any]:
    if not isinstance(snapshot, dict):
        raise build_error(ErrorCode.SNAPSHOT_MISMATCH, "snapshot root must be an object")
    if snapshot.get("schema_version") != SNAPSHOT_SCHEMA_VERSION:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "snapshot schema version changed",
            expected=SNAPSHOT_SCHEMA_VERSION,
            found=snapshot.get("schema_version"),
            field="schema_version",
        )
    if snapshot.get("config_version") != config_version:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "snapshot configuration version changed",
            expected=config_version,
            found=snapshot.get("config_version"),
            field="config_version",
        )
    if snapshot.get("boundaries_sha256") != boundaries_sha256:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "snapshot boundary table changed",
            expected=boundaries_sha256,
            found=snapshot.get("boundaries_sha256"),
            field="boundaries_sha256",
        )
    boundaries = snapshot.get("boundaries")
    if not isinstance(boundaries, dict):
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "snapshot boundary table is missing",
            field="boundaries",
        )
    from .util import sha256_obj

    if sha256_obj(boundaries) != boundaries_sha256:
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "snapshot boundary table hash mismatch",
            field="boundaries",
        )
    actual_params = snapshot.get("track_params_versions")
    if actual_params != dict(sorted(track_params_versions.items())):
        raise build_error(
            ErrorCode.SNAPSHOT_MISMATCH,
            "snapshot track parameter versions changed",
            expected=dict(sorted(track_params_versions.items())),
            found=actual_params,
            field="track_params_versions",
        )
    tracks = snapshot.get("tracks")
    if not isinstance(tracks, dict):
        raise build_error(ErrorCode.SNAPSHOT_MISMATCH, "snapshot tracks are invalid")
    for track_id in sorted(track_params_versions):
        value = tracks.get(track_id)
        if not isinstance(value, dict):
            raise build_error(ErrorCode.SNAPSHOT_MISMATCH, "snapshot is missing a track", track_id=track_id)
        entries = value.get("entries")
        if not isinstance(entries, list):
            raise build_error(ErrorCode.SNAPSHOT_MISMATCH, "snapshot track entries are invalid", track_id=track_id)
        previous_chain = "GENESIS"
        for expected_index, entry in enumerate(entries):
            if not isinstance(entry, dict) or entry.get("slice_index") != expected_index:
                raise build_error(
                    ErrorCode.SNAPSHOT_MISMATCH,
                    "snapshot track index is not contiguous",
                    track_id=track_id,
                    expected_slice=expected_index,
                )
            content_hash = compute_content_hash(entry)
            if entry.get("content_hash") != content_hash:
                raise build_error(
                    ErrorCode.SNAPSHOT_MISMATCH,
                    "snapshot slice content hash mismatch",
                    track_id=track_id,
                    slice_index=expected_index,
                    field="content_hash",
                )
            previous_chain = compute_chain_hash(
                previous_chain,
                content_hash,
                track_params_versions[track_id],
                boundaries_sha256,
            )
            if entry.get("chain_hash") != previous_chain:
                raise build_error(
                    ErrorCode.SNAPSHOT_MISMATCH,
                    "snapshot hash chain mismatch",
                    track_id=track_id,
                    slice_index=expected_index,
                    field="chain_hash",
                )
        if value.get("chain_head") != previous_chain:
            raise build_error(
                ErrorCode.SNAPSHOT_MISMATCH,
                "snapshot chain head mismatch",
                track_id=track_id,
                expected=previous_chain,
                found=value.get("chain_head"),
                field="chain_head",
            )
    return snapshot


def save_snapshot(path: Path, value: dict[str, Any]) -> None:
    write_json_atomic(path, value)


def load_snapshot(
    path: Path,
    config_version: str,
    boundaries_sha256: str,
    track_params_versions: dict[str, str],
) -> dict[str, Any] | None:
    if not path.exists():
        return None
    raw = read_json(path)
    return validate_snapshot(raw, config_version, boundaries_sha256, track_params_versions)

