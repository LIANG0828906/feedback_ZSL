from __future__ import annotations

import base64
import math
from pathlib import Path
from typing import Any

import numpy as np

from .errors import MasterError
from .util import atomic_write_json, canonical_json_bytes, sha256_bytes


SNAPSHOT_VERSION = 1


def _without_checksum(state: dict[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in state.items() if key != "state_checksum"}


def encode_master(array: np.ndarray) -> str:
    little_endian = np.asarray(array, dtype="<f8")
    return base64.b64encode(little_endian.tobytes(order="C")).decode("ascii")


def decode_master(encoded: str, shape: tuple[int, int]) -> np.ndarray:
    try:
        raw = base64.b64decode(encoded.encode("ascii"), validate=True)
    except (ValueError, UnicodeEncodeError) as exc:
        raise ValueError(f"invalid base64 master: {exc}") from exc
    expected = int(shape[0]) * int(shape[1]) * 8
    if len(raw) != expected:
        raise ValueError(
            f"master byte length mismatch: expected {expected}, received {len(raw)}"
        )
    return np.frombuffer(raw, dtype="<f8").reshape(shape).astype(np.float64, copy=True)


def build_snapshot(
    *,
    manifest_digest: str,
    input_digests: dict[str, str],
    block_size: int,
    sample_rate: int,
    total_samples: int,
    completed_blocks: int,
    confirmed_intervals: list[dict[str, Any]],
    voice_corrections: dict[str, float],
    pre_master: np.ndarray,
    stage: str,
) -> dict[str, Any]:
    array = np.asarray(pre_master, dtype=np.float64)
    if array.ndim != 2 or array.shape[1] != 2:
        raise ValueError("pre_master must have shape (samples, 2)")
    total_blocks = int(math.ceil(total_samples / block_size)) if total_samples else 0
    state: dict[str, Any] = {
        "snapshot_version": SNAPSHOT_VERSION,
        "manifest_digest": manifest_digest,
        "input_digests": dict(sorted(input_digests.items())),
        "block_size": int(block_size),
        "sample_rate": int(sample_rate),
        "total_samples": int(total_samples),
        "total_blocks": total_blocks,
        "completed_blocks": int(completed_blocks),
        "confirmed_intervals": confirmed_intervals,
        "voice_corrections": {
            voice: float(value) for voice, value in sorted(voice_corrections.items())
        },
        "stage": stage,
        "pre_master_shape": [int(array.shape[0]), 2],
        "pre_master_f64le_b64": encode_master(array),
    }
    state["state_checksum"] = sha256_bytes(canonical_json_bytes(state))
    return state


def inspect_snapshot(snapshot: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(snapshot, dict):
        raise ValueError("snapshot root is not an object")
    required = {
        "snapshot_version",
        "manifest_digest",
        "input_digests",
        "block_size",
        "sample_rate",
        "total_samples",
        "total_blocks",
        "completed_blocks",
        "confirmed_intervals",
        "voice_corrections",
        "stage",
        "pre_master_shape",
        "pre_master_f64le_b64",
        "state_checksum",
    }
    missing = sorted(required - set(snapshot))
    if missing:
        raise ValueError(f"snapshot is missing fields: {', '.join(missing)}")
    if snapshot["snapshot_version"] != SNAPSHOT_VERSION:
        raise ValueError("unsupported snapshot version")
    expected_checksum = sha256_bytes(canonical_json_bytes(_without_checksum(snapshot)))
    if snapshot["state_checksum"] != expected_checksum:
        raise ValueError("snapshot checksum mismatch")
    shape = snapshot["pre_master_shape"]
    if (
        not isinstance(shape, list)
        or len(shape) != 2
        or int(shape[1]) != 2
        or int(shape[0]) < 0
    ):
        raise ValueError("invalid pre_master_shape")
    pre_master = decode_master(snapshot["pre_master_f64le_b64"], (int(shape[0]), 2))
    snapshot["_pre_master"] = pre_master
    return snapshot


def save_snapshot(path: Path, snapshot: dict[str, Any]) -> None:
    atomic_write_json(path, snapshot)


def discard_snapshot(path: Path) -> None:
    try:
        path.unlink()
    except FileNotFoundError:
        pass


def load_resume_state(
    path: Path,
    *,
    manifest_digest: str,
    input_digests: dict[str, str],
    block_size: int,
    sample_rate: int,
    total_samples: int,
    confirmed_intervals: list[dict[str, Any]],
    voice_corrections: dict[str, float],
) -> dict[str, Any]:
    import json

    try:
        with path.open("r", encoding="utf-8") as handle:
            snapshot = json.load(handle)
        inspect_snapshot(snapshot)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        discard_snapshot(path)
        raise MasterError(
            "E_RESUME_MISMATCH",
            f"Persisted snapshot cannot be trusted and was discarded: {exc}",
            path=str(path),
            exit_code=3,
        ) from exc

    mismatches: list[str] = []
    if snapshot["manifest_digest"] != manifest_digest:
        mismatches.append("manifest_digest")
    if snapshot["input_digests"] != dict(sorted(input_digests.items())):
        mismatches.append("input_digests")
    if int(snapshot["block_size"]) != int(block_size):
        mismatches.append("block_size")
    if int(snapshot["sample_rate"]) != int(sample_rate):
        mismatches.append("sample_rate")
    if int(snapshot["total_samples"]) != int(total_samples):
        mismatches.append("total_samples")
    if snapshot["confirmed_intervals"] != confirmed_intervals:
        mismatches.append("confirmed_intervals")
    stored_corrections = snapshot["voice_corrections"]
    if set(stored_corrections) != set(voice_corrections):
        mismatches.append("voice_corrections.keys")
    elif any(
        abs(float(stored_corrections[voice]) - float(value)) > 1e-12
        for voice, value in voice_corrections.items()
    ):
        mismatches.append("voice_corrections.values")

    total_blocks = int(math.ceil(total_samples / block_size)) if total_samples else 0
    completed_blocks = int(snapshot["completed_blocks"])
    if int(snapshot["total_blocks"]) != total_blocks:
        mismatches.append("total_blocks")
    if not 0 <= completed_blocks <= total_blocks:
        mismatches.append("completed_blocks")
    if pre_master_length := len(snapshot["_pre_master"]):
        expected_samples = min(completed_blocks * block_size, total_samples)
        if pre_master_length != expected_samples:
            mismatches.append("pre_master_length")
    elif completed_blocks != 0:
        mismatches.append("pre_master_length")
    if snapshot["stage"] not in {"rendering", "finalized"}:
        mismatches.append("stage")

    if mismatches:
        discard_snapshot(path)
        raise MasterError(
            "E_RESUME_MISMATCH",
            "Persisted snapshot does not match the current manifest, audio input, or confirmed results; it was discarded.",
            path=str(path),
            details={"mismatch_fields": mismatches},
            exit_code=3,
        )
    return snapshot

