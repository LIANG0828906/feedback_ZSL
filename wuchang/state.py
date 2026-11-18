"""Atomic block snapshots and resume-state validation."""

from __future__ import annotations

import hashlib
import json
import shutil
from pathlib import Path
from typing import Any

import numpy as np

from .errors import atomic_write_bytes, canonical_json_bytes, write_json

SNAPSHOT_SCHEMA_VERSION = 1


def snapshot_path(state_dir: str | Path) -> Path:
    return Path(state_dir) / "state.json"


def blocks_dir(state_dir: str | Path) -> Path:
    return Path(state_dir) / "blocks"


def load_snapshot(
    state_dir: str | Path,
) -> tuple[dict[str, Any] | None, dict[str, Any] | None]:
    path = snapshot_path(state_dir)
    if not path.is_file():
        return None, None
    try:
        with open(path, "r", encoding="utf-8") as handle:
            snapshot = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        return None, {
            "reason": "snapshot_unreadable",
            "detail": str(exc).splitlines()[0][:200],
        }
    if not isinstance(snapshot, dict):
        return None, {"reason": "snapshot_not_object", "detail": ""}
    if snapshot.get("schema_version") != SNAPSHOT_SCHEMA_VERSION:
        return None, {
            "reason": "snapshot_schema_mismatch",
            "detail": {
                "expected": SNAPSHOT_SCHEMA_VERSION,
                "actual": snapshot.get("schema_version"),
            },
        }
    return snapshot, None


def discard_state(state_dir: str | Path) -> None:
    directory = Path(state_dir)
    if directory.exists():
        shutil.rmtree(directory)
    directory.mkdir(parents=True, exist_ok=True)
    blocks_dir(directory).mkdir(parents=True, exist_ok=True)


def remove_state(state_dir: str | Path) -> None:
    directory = Path(state_dir)
    if directory.exists():
        shutil.rmtree(directory)


def alignment_parameters(
    processed_channels: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    return [
        {
            "channel": channel["name"],
            "alignment_offset_samples": int(
                channel["alignment_offset_samples"]
            ),
            "alignment_status": channel["alignment_status"],
        }
        for channel in processed_channels
    ]


def reflection_parameters(
    processed_channels: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    output: list[dict[str, Any]] = []
    for channel in processed_channels:
        reflection = channel["reflection"]
        output.append(
            {
                "channel": channel["name"],
                "enabled": bool(reflection["enabled"]),
                "delay_samples": reflection["delay_samples"],
                "delay_ms": reflection["delay_ms"],
                "estimated_gain_db": reflection["estimated_gain_db"],
                "window_samples": reflection["window_samples"],
                "estimation_status": reflection["estimation_status"],
            }
        )
    return output


def snapshot_manifest_summary(document: dict[str, Any]) -> dict[str, Any]:
    return {
        "schema_version": document["schema_version"],
        "sample_rate": int(document["sample_rate"]),
        "bit_depth": int(document["bit_depth"]),
        "duration_seconds": float(document["duration_seconds"]),
        "block_frames": int(document["block_frames"]),
        "target_loudness_lufs": float(document["target_loudness_lufs"]),
        "true_peak_limit_dbtp": float(
            document.get("true_peak_limit_dbtp", -1.0)
        ),
        "channels": [
            {
                "name": channel["name"],
                "instrument": channel["instrument"],
                "file": channel["file"],
                "seed": int(channel["seed"]),
                "start_offset_samples": int(channel["start_offset_samples"]),
                "cues": [float(value) for value in channel["cues"]],
            }
            for channel in document["channels"]
        ],
    }


def compare_snapshot(
    snapshot: dict[str, Any],
    manifest_digest: str,
    input_digest: str,
    alignment: list[dict[str, Any]],
    reflection: list[dict[str, Any]],
) -> dict[str, Any]:
    differences: dict[str, Any] = {}
    expected_pairs = {
        "manifest_digest": (snapshot.get("manifest_digest"), manifest_digest),
        "input_digest": (snapshot.get("input_digest"), input_digest),
        "alignment_offsets": (
            snapshot.get("alignment_offsets"),
            alignment,
        ),
        "reflection_params": (
            snapshot.get("reflection_params"),
            reflection,
        ),
    }
    for field, (old, current) in expected_pairs.items():
        if canonical_json_bytes(old) != canonical_json_bytes(current):
            differences[field] = {"snapshot": old, "current": current}
    return differences


def new_snapshot(
    document: dict[str, Any],
    manifest_digest: str,
    input_digest: str,
    processing_alignment: list[dict[str, Any]],
    processing_reflection: list[dict[str, Any]],
    block_frames: int,
    total_frames: int,
    resume_count: int,
) -> dict[str, Any]:
    total_blocks = (total_frames + block_frames - 1) // block_frames
    return {
        "schema_version": SNAPSHOT_SCHEMA_VERSION,
        "manifest_digest": manifest_digest,
        "input_digest": input_digest,
        "manifest_summary": snapshot_manifest_summary(document),
        "block_frames": int(block_frames),
        "total_frames": int(total_frames),
        "total_blocks": int(total_blocks),
        "completed_blocks": 0,
        "frames_done": 0,
        "resume_count": int(resume_count),
        "alignment_offsets": processing_alignment,
        "reflection_params": processing_reflection,
        "blocks": [],
    }


def block_filename(index: int) -> str:
    return f"blocks/{index:08d}.f32le"


def write_block(
    state_dir: str | Path,
    index: int,
    block: np.ndarray,
) -> dict[str, Any]:
    interleaved = np.asarray(block, dtype="<f4").reshape(-1, 2)
    payload = interleaved.tobytes(order="C")
    relative = block_filename(index)
    atomic_write_bytes(Path(state_dir) / relative, payload)
    return {
        "index": int(index),
        "frames": int(block.shape[0]),
        "bytes": len(payload),
        "sha256": hashlib.sha256(payload).hexdigest(),
        "file": relative,
    }


def commit_snapshot(
    state_dir: str | Path,
    snapshot: dict[str, Any],
) -> None:
    write_json(snapshot_path(state_dir), snapshot)


def validate_committed_blocks(
    snapshot: dict[str, Any],
    state_dir: str | Path,
    total_frames: int,
    block_frames: int,
) -> tuple[bool, dict[str, Any]]:
    total_blocks = (total_frames + block_frames - 1) // block_frames
    completed = snapshot.get("completed_blocks")
    blocks = snapshot.get("blocks")
    if (
        not isinstance(completed, int)
        or not isinstance(blocks, list)
        or completed < 0
        or completed > total_blocks
        or len(blocks) != completed
    ):
        return False, {
            "reason": "block_count_invalid",
            "snapshot": completed,
            "current": total_blocks,
        }
    frames_done = 0
    for index, entry in enumerate(blocks):
        if not isinstance(entry, dict):
            return False, {
                "reason": "block_entry_invalid",
                "snapshot": index,
                "current": index,
            }
        expected_frames = min(
            block_frames, max(0, total_frames - index * block_frames)
        )
        relative = entry.get("file")
        if (
            entry.get("index") != index
            or entry.get("frames") != expected_frames
            or not isinstance(relative, str)
        ):
            return False, {
                "reason": "block_metadata_mismatch",
                "snapshot": entry,
                "current": {
                    "index": index,
                    "frames": expected_frames,
                },
            }
        path = Path(state_dir) / relative
        try:
            payload = path.read_bytes()
        except OSError:
            return False, {
                "reason": "block_missing",
                "snapshot": relative,
                "current": None,
            }
        expected_bytes = expected_frames * 2 * 4
        if len(payload) != expected_bytes:
            return False, {
                "reason": "block_size_mismatch",
                "snapshot": len(payload),
                "current": expected_bytes,
            }
        digest = hashlib.sha256(payload).hexdigest()
        if digest != entry.get("sha256"):
            return False, {
                "reason": "block_digest_mismatch",
                "snapshot": entry.get("sha256"),
                "current": digest,
            }
        frames_done += expected_frames
    if snapshot.get("frames_done") != frames_done:
        return False, {
            "reason": "frames_done_mismatch",
            "snapshot": snapshot.get("frames_done"),
            "current": frames_done,
        }
    if snapshot.get("total_frames") != total_frames:
        return False, {
            "reason": "total_frames_mismatch",
            "snapshot": snapshot.get("total_frames"),
            "current": total_frames,
        }
    return True, {}


def assemble_blocks(
    snapshot: dict[str, Any],
    state_dir: str | Path,
) -> np.ndarray:
    chunks: list[np.ndarray] = []
    for entry in snapshot["blocks"]:
        path = Path(state_dir) / entry["file"]
        payload = path.read_bytes()
        values = np.frombuffer(payload, dtype="<f4")
        if values.size != int(entry["frames"]) * 2:
            raise ValueError("committed block frame count changed")
        chunks.append(values.reshape(-1, 2))
    if not chunks:
        return np.zeros((0, 2), dtype=np.float32)
    return np.concatenate(chunks, axis=0).astype(np.float32, copy=False)

