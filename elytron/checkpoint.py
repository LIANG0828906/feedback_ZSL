"""Atomic, self-contained block snapshots for restart-safe rendering."""

from __future__ import annotations

import base64
import json
import os
import zlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .fileio import write_json_atomic
from .render import BlockSpec, TileResult
from .scene import SceneBundle, canonical_bytes, sha256_bytes

PROGRESS_FORMAT = "elytron-progress/v1"


@dataclass
class ResumeResult:
    granted: bool
    snapshot_found: bool
    reasons: list[str]
    tiles: dict[str, TileResult]
    records: dict[str, dict[str, Any]]


def _encode_array(array: np.ndarray) -> dict[str, Any]:
    contiguous = np.ascontiguousarray(array)
    return {
        "dtype": contiguous.dtype.str,
        "shape": list(contiguous.shape),
        "data": base64.b64encode(
            zlib.compress(contiguous.tobytes(order="C"), level=9)
        ).decode("ascii"),
    }


def _decode_array(payload: dict[str, Any]) -> np.ndarray:
    dtype = np.dtype(payload["dtype"])
    shape = tuple(int(v) for v in payload["shape"])
    data = zlib.decompress(base64.b64decode(payload["data"], validate=True))
    expected = int(np.prod(shape, dtype=np.int64)) * dtype.itemsize
    if len(data) != expected:
        raise ValueError("decoded array byte count does not match its shape and dtype")
    result = np.frombuffer(data, dtype=dtype).reshape(shape).copy()
    return result


def save_snapshot(
    path: str | Path,
    bundle: SceneBundle,
    specs: list[BlockSpec],
    tiles: dict[str, TileResult],
    records: dict[str, dict[str, Any]],
) -> None:
    completed: list[dict[str, Any]] = []
    for spec in specs[: len(records)]:
        if spec.block_id not in tiles or spec.block_id not in records:
            raise RuntimeError(f"snapshot is missing completed block {spec.block_id}")
        tile = tiles[spec.block_id]
        completed.append(
            {
                "block_id": spec.block_id,
                "camera_id": spec.camera_id,
                "block_index": spec.block_index,
                "x": spec.x,
                "y": spec.y,
                "width": spec.width,
                "height": spec.height,
                "summary": records[spec.block_id],
                "rgba": _encode_array(tile.rgba),
                "face_id": _encode_array(tile.face_id),
            }
        )
    payload = {
        "format": PROGRESS_FORMAT,
        "scene_digest": bundle.scene_digest,
        "film_signature": bundle.film_signature,
        "scene_summary": bundle.scene_summary,
        "completed_block_ids": [record["block_id"] for record in completed],
        "next_block_index": len(completed),
        "blocks": completed,
    }
    envelope = {
        "format": PROGRESS_FORMAT,
        "checksum": sha256_bytes(canonical_bytes(payload)),
        "payload": payload,
    }
    write_json_atomic(path, envelope)


def _discard(snapshot_found: bool, reasons: list[str]) -> ResumeResult:
    return ResumeResult(
        granted=False,
        snapshot_found=snapshot_found,
        reasons=reasons,
        tiles={},
        records={},
    )


def load_snapshot(
    path: str | Path,
    bundle: SceneBundle,
    specs: list[BlockSpec],
) -> ResumeResult:
    snapshot_path = Path(path)
    if not snapshot_path.exists():
        return _discard(False, ["snapshot_missing"])
    try:
        with snapshot_path.open("r", encoding="utf-8") as handle:
            envelope = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        return _discard(True, [f"snapshot_unreadable:{type(exc).__name__}"])

    try:
        if not isinstance(envelope, dict):
            raise ValueError("snapshot envelope is not an object")
        if envelope.get("format") != PROGRESS_FORMAT:
            raise ValueError("snapshot format tag is not recognized")
        payload = envelope.get("payload")
        if not isinstance(payload, dict):
            raise ValueError("snapshot payload is missing")
        expected_checksum = sha256_bytes(canonical_bytes(payload))
        if envelope.get("checksum") != expected_checksum:
            raise ValueError("snapshot checksum mismatch")
        if payload.get("format") != PROGRESS_FORMAT:
            raise ValueError("snapshot payload format mismatch")
        if payload.get("scene_digest") != bundle.scene_digest:
            raise ValueError("scene digest mismatch")
        if payload.get("film_signature") != bundle.film_signature:
            raise ValueError("film parameter signature mismatch")
        if canonical_bytes(payload.get("scene_summary")) != canonical_bytes(bundle.scene_summary):
            raise ValueError("scene summary mismatch")
        blocks = payload.get("blocks")
        completed_ids = payload.get("completed_block_ids")
        next_index = payload.get("next_block_index")
        if not isinstance(blocks, list) or not isinstance(completed_ids, list):
            raise ValueError("snapshot block list is malformed")
        if isinstance(next_index, bool) or not isinstance(next_index, int):
            raise ValueError("snapshot next_block_index is malformed")
        if next_index != len(blocks) or len(completed_ids) != len(blocks):
            raise ValueError("snapshot block offsets are inconsistent")
        if next_index > len(specs):
            raise ValueError("snapshot contains more blocks than the scene can produce")

        tiles: dict[str, TileResult] = {}
        records: dict[str, dict[str, Any]] = {}
        expected_ids = [spec.block_id for spec in specs[:next_index]]
        if completed_ids != expected_ids:
            raise ValueError("completed block IDs do not match the canonical block prefix")
        for index, record in enumerate(blocks):
            spec = specs[index]
            if not isinstance(record, dict):
                raise ValueError("snapshot block record is malformed")
            for key, expected in [
                ("block_id", spec.block_id),
                ("camera_id", spec.camera_id),
                ("block_index", spec.block_index),
                ("x", spec.x),
                ("y", spec.y),
                ("width", spec.width),
                ("height", spec.height),
            ]:
                if record.get(key) != expected:
                    raise ValueError(f"snapshot block {spec.block_id} has stale {key}")
            rgba = _decode_array(record["rgba"])
            face_id = _decode_array(record["face_id"])
            if rgba.shape != (spec.height, spec.width, 4) or rgba.dtype != np.uint8:
                raise ValueError(f"snapshot block {spec.block_id} has a bad RGBA tile")
            if face_id.shape != (spec.height, spec.width) or face_id.dtype != np.int32:
                raise ValueError(f"snapshot block {spec.block_id} has a bad face tile")
            summary = record.get("summary")
            if not isinstance(summary, dict):
                raise ValueError(f"snapshot block {spec.block_id} has no color summary")
            if summary.get("rgba_sha256") != sha256_bytes(rgba.tobytes(order="C")):
                raise ValueError(f"snapshot block {spec.block_id} RGBA summary mismatch")
            if summary.get("face_id_sha256") != sha256_bytes(face_id.tobytes(order="C")):
                raise ValueError(f"snapshot block {spec.block_id} face summary mismatch")
            tile = TileResult(
                rgba=rgba,
                face_id=face_id,
                thickness_sum=float(summary["thickness_sum"]),
                thickness_count=int(summary["thickness_count"]),
                visible_face_count=int(summary["visible_face_count"]),
            )
            tiles[spec.block_id] = tile
            records[spec.block_id] = summary
    except (KeyError, TypeError, ValueError, zlib.error, json.JSONDecodeError) as exc:
        return _discard(True, [f"snapshot_discarded:{str(exc)}"])

    return ResumeResult(
        granted=True,
        snapshot_found=True,
        reasons=[],
        tiles=tiles,
        records=records,
    )


def remove_snapshot(path: str | Path) -> None:
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
