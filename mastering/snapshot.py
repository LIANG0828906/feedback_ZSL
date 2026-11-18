from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import BusinessError, BusinessFailure, EXIT_RESUME_MISMATCH
from .util import atomic_write_json, decode_float64_array, encode_float64_array


SNAPSHOT_SCHEMA = "mastering-snapshot/v1"


@dataclass(frozen=True)
class SnapshotState:
    completed_blocks: int
    prefix: np.ndarray
    commits: int


def write_snapshot(
    path: Path,
    *,
    manifest_digest: str,
    sample_rate: int,
    block_frames: int,
    total_frames: int,
    completed_blocks: int,
    commits: int,
    parameters: dict[str, Any],
    prefix: np.ndarray,
) -> None:
    payload = {
        "schema": SNAPSHOT_SCHEMA,
        "manifest_digest": manifest_digest,
        "sample_rate": sample_rate,
        "block_frames": block_frames,
        "total_frames": total_frames,
        "completed_blocks": int(completed_blocks),
        "commits": int(commits),
        "parameters": parameters,
        "prefix": encode_float64_array(prefix),
    }
    atomic_write_json(path, payload)


def _resume_mismatch(path: Path, reason: str) -> BusinessFailure:
    try:
        path.unlink(missing_ok=True)
    except OSError:
        pass
    return BusinessFailure(
        [
            BusinessError(
                "E_RESUME_MISMATCH",
                f"snapshot is incompatible with current inputs; old progress discarded: {reason}",
                details={"snapshot": str(path)},
            )
        ],
        EXIT_RESUME_MISMATCH,
    )


def load_snapshot(
    path: Path,
    *,
    manifest_digest: str,
    sample_rate: int,
    block_frames: int,
    total_frames: int,
    parameters: dict[str, Any],
) -> SnapshotState | None:
    if not path.exists():
        return None
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise _resume_mismatch(path, f"snapshot unreadable: {exc}") from exc

    if not isinstance(payload, dict) or payload.get("schema") != SNAPSHOT_SCHEMA:
        raise _resume_mismatch(path, "snapshot schema changed")
    expected_fields = {
        "manifest_digest": manifest_digest,
        "sample_rate": sample_rate,
        "block_frames": block_frames,
        "total_frames": total_frames,
    }
    for field, expected in expected_fields.items():
        if payload.get(field) != expected:
            raise _resume_mismatch(path, f"{field} changed")
    if payload.get("parameters") != parameters:
        raise _resume_mismatch(path, "determined alignment or ducking parameters changed")

    try:
        completed_blocks = int(payload["completed_blocks"])
        commits = int(payload["commits"])
        prefix = decode_float64_array(payload["prefix"])
    except (KeyError, TypeError, ValueError) as exc:
        raise _resume_mismatch(path, f"snapshot payload invalid: {exc}") from exc

    total_blocks = (total_frames + block_frames - 1) // block_frames
    if completed_blocks < 0 or completed_blocks > total_blocks:
        raise _resume_mismatch(path, "completed block count is outside current render")
    expected_frames = (
        total_frames if completed_blocks == total_blocks else completed_blocks * block_frames
    )
    if prefix.shape != (expected_frames, 2):
        raise _resume_mismatch(
            path,
            f"prefix shape {prefix.shape} does not match committed blocks {(expected_frames, 2)}",
        )
    if commits < completed_blocks:
        raise _resume_mismatch(path, "snapshot commit count is invalid")
    return SnapshotState(completed_blocks=completed_blocks, prefix=prefix, commits=commits)
