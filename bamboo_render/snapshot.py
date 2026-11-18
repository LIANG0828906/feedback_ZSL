from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from . import ALGORITHM_VERSION
from .errors import SnapshotInvalid
from .io_utils import (
    atomic_write_json,
    decode_array,
    encode_array,
    sha256_bytes,
    stable_float,
)
from .renderer import BlockResult, block_color_summary
from .scene import BlockSpec, ImageSpec, SceneSpec

SNAPSHOT_VERSION = 1
SNAPSHOT_FILENAME = "snapshot.json"


@dataclass(frozen=True)
class SnapshotState:
    completed: dict[int, BlockResult]
    raw_bytes: int


def snapshot_path(output_dir: Path) -> Path:
    return output_dir / SNAPSHOT_FILENAME


def commit_snapshot(
    output_dir: Path,
    scene: SceneSpec,
    scene_digest: str,
    blocks: dict[int, BlockResult],
) -> None:
    ordered: list[dict[str, Any]] = []
    for block_id in sorted(blocks):
        result = blocks[block_id]
        block = result.block
        projection_data = {
            pair_id: encode_array(values)
            for pair_id, values in sorted(result.projection_gray.items())
        }
        projection_sha = {
            pair_id: sha256_bytes(np.ascontiguousarray(values).tobytes())
            for pair_id, values in sorted(result.projection_gray.items())
        }
        summary = block_color_summary(result.rgba, result.ownership)
        summary["projection_sha256"] = projection_sha
        ordered.append(
            {
                "block_id": block.block_id,
                "x": block.x,
                "y": block.y,
                "width": block.width,
                "height": block.height,
                "rgba_shape": list(result.rgba.shape),
                "rgba_dtype": result.rgba.dtype.str,
                "rgba_sha256": sha256_bytes(np.ascontiguousarray(result.rgba).tobytes()),
                "rgba_data": encode_array(result.rgba),
                "layer_rgba_shape": list(result.layer_rgba.shape),
                "layer_rgba_dtype": result.layer_rgba.dtype.str,
                "layer_rgba_sha256": sha256_bytes(
                    np.ascontiguousarray(result.layer_rgba).tobytes()
                ),
                "layer_rgba_data": encode_array(result.layer_rgba),
                "ownership_shape": list(result.ownership.shape),
                "ownership_dtype": result.ownership.dtype.str,
                "ownership_sha256": sha256_bytes(np.ascontiguousarray(result.ownership).tobytes()),
                "ownership_data": encode_array(result.ownership),
                "projection_shape": [block.height, block.width],
                "projection_data": projection_data,
                "metrics_shape": list(result.metrics.shape),
                "metrics_dtype": result.metrics.dtype.str,
                "metrics_sha256": sha256_bytes(
                    np.ascontiguousarray(result.metrics).tobytes()
                ),
                "metrics_data": encode_array(result.metrics),
                "color_summary": summary,
            }
        )
    payload = {
        "snapshot_version": SNAPSHOT_VERSION,
        "algorithm": ALGORITHM_VERSION,
        "scene_digest": scene_digest,
        "image": [scene.image.width, scene.image.height],
        "chunk_size": [scene.image.chunk_width, scene.image.chunk_height],
        "block_count": scene.image.block_count,
        "completed_count": len(ordered),
        "completed_block_ids": sorted(blocks),
        "completed_blocks": ordered,
    }
    atomic_write_json(snapshot_path(output_dir), payload)


def load_snapshot(
    output_dir: Path,
    scene: SceneSpec,
    scene_digest: str,
    expected_pairs: list[str],
) -> SnapshotState:
    path = snapshot_path(output_dir)
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise SnapshotInvalid("SNAPSHOT_UNREADABLE", f"cannot read snapshot: {exc}") from exc
    try:
        payload = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", f"snapshot JSON is invalid: {exc}") from exc
    if not isinstance(payload, dict):
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "snapshot root must be an object")
    if payload.get("snapshot_version") != SNAPSHOT_VERSION:
        raise SnapshotInvalid("SNAPSHOT_VERSION_MISMATCH", "snapshot version is unsupported")
    if payload.get("algorithm") != ALGORITHM_VERSION:
        raise SnapshotInvalid("SNAPSHOT_ALGORITHM_MISMATCH", "snapshot algorithm version differs")
    if payload.get("scene_digest") != scene_digest:
        raise SnapshotInvalid("SNAPSHOT_SCENE_MISMATCH", "snapshot scene digest differs")
    if payload.get("image") != [scene.image.width, scene.image.height]:
        raise SnapshotInvalid("SNAPSHOT_GEOMETRY_MISMATCH", "snapshot image size differs")
    if payload.get("chunk_size") != [scene.image.chunk_width, scene.image.chunk_height]:
        raise SnapshotInvalid("SNAPSHOT_GEOMETRY_MISMATCH", "snapshot chunk size differs")
    if payload.get("block_count") != scene.image.block_count:
        raise SnapshotInvalid("SNAPSHOT_GEOMETRY_MISMATCH", "snapshot block count differs")

    entries = payload.get("completed_blocks")
    ids = payload.get("completed_block_ids")
    if not isinstance(entries, list) or not isinstance(ids, list):
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "completed block records are missing")
    if sorted(ids) != ids or len(set(ids)) != len(ids):
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "completed block ids are not unique and sorted")
    if len(entries) != len(ids):
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "completed block count is inconsistent")

    expected_blocks = {block.block_id: block for block in scene.image.blocks()}
    completed: dict[int, BlockResult] = {}
    for entry in entries:
        if not isinstance(entry, dict):
            raise SnapshotInvalid("SNAPSHOT_CORRUPT", "block record must be an object")
        block_id = entry.get("block_id")
        if not isinstance(block_id, int) or block_id not in expected_blocks:
            raise SnapshotInvalid("SNAPSHOT_BLOCK_RANGE", "snapshot block id is outside the image", block_id)
        if block_id in completed:
            raise SnapshotInvalid("SNAPSHOT_CORRUPT", "duplicate completed block", block_id)
        expected = expected_blocks[block_id]
        stored_geometry = (
            entry.get("x"),
            entry.get("y"),
            entry.get("width"),
            entry.get("height"),
        )
        expected_geometry = (expected.x, expected.y, expected.width, expected.height)
        if stored_geometry != expected_geometry:
            raise SnapshotInvalid(
                "SNAPSHOT_BLOCK_OFFSET_MISMATCH",
                "snapshot block geometry does not match the declared chunk grid",
                block_id,
            )
        try:
            rgba = _decode_checked(
                entry,
                "rgba",
                (expected.height, expected.width, 4),
                np.dtype(np.uint8),
            )
            layer_rgba = _decode_checked(
                entry,
                "layer_rgba",
                (len(scene.layers), expected.height, expected.width, 4),
                np.dtype(np.uint8),
            )
            ownership = _decode_checked(
                entry,
                "ownership",
                (expected.height, expected.width, 4),
                np.dtype(np.uint8),
            )
            metrics = _decode_checked(
                entry,
                "metrics",
                (len(scene.layers), 5),
                np.dtype(np.float64),
            )
            projection_shape = entry.get("projection_shape")
            if projection_shape != [expected.height, expected.width]:
                raise ValueError("projection shape mismatch")
            projection_data = entry.get("projection_data")
            if not isinstance(projection_data, dict) or sorted(projection_data) != sorted(expected_pairs):
                raise ValueError("projection pair set mismatch")
            projection_sha = entry.get("color_summary", {}).get("projection_sha256")
            if not isinstance(projection_sha, dict):
                raise ValueError("projection summaries are missing")
            projection_gray: dict[str, np.ndarray] = {}
            for pair_id in expected_pairs:
                encoded = projection_data.get(pair_id)
                if not isinstance(encoded, str):
                    raise ValueError(f"projection payload missing for {pair_id}")
                values = decode_array(encoded, (expected.height, expected.width), "|u1")
                actual_sha = sha256_bytes(np.ascontiguousarray(values).tobytes())
                if projection_sha.get(pair_id) != actual_sha:
                    raise ValueError(f"projection hash mismatch for {pair_id}")
                projection_gray[pair_id] = values
            if metrics.dtype != np.float64:
                raise ValueError("metrics dtype mismatch")
        except (ValueError, TypeError) as exc:
            raise SnapshotInvalid("SNAPSHOT_CORRUPT", str(exc), block_id) from exc
        _validate_color_summary(entry, rgba, ownership, block_id)
        completed[block_id] = BlockResult(
            block=expected,
            rgba=rgba,
            layer_rgba=layer_rgba,
            ownership=ownership,
            projection_gray=projection_gray,
            metrics=metrics,
        )
    if sorted(completed) != ids:
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "completed block id list does not match records")
    return SnapshotState(completed=completed, raw_bytes=len(raw))


def discard_snapshot(output_dir: Path) -> None:
    try:
        snapshot_path(output_dir).unlink()
    except FileNotFoundError:
        pass


def _decode_checked(
    entry: dict[str, Any],
    prefix: str,
    shape: tuple[int, ...],
    expected_dtype: np.dtype,
) -> np.ndarray:
    stored_shape = entry.get(f"{prefix}_shape")
    if stored_shape != list(shape):
        raise ValueError(f"{prefix} shape mismatch")
    dtype = entry.get(f"{prefix}_dtype")
    if not isinstance(dtype, str):
        raise ValueError(f"{prefix} dtype is missing")
    payload = entry.get(f"{prefix}_data")
    if not isinstance(payload, str):
        raise ValueError(f"{prefix} payload is missing")
    array = decode_array(payload, shape, dtype)
    if np.dtype(dtype) != expected_dtype:
        raise ValueError(f"{prefix} dtype mismatch")
    actual_sha = sha256_bytes(np.ascontiguousarray(array).tobytes())
    if entry.get(f"{prefix}_sha256") != actual_sha:
        raise ValueError(f"{prefix} hash mismatch")
    return array


def _validate_color_summary(
    entry: dict[str, Any],
    rgba: np.ndarray,
    ownership: np.ndarray,
    block_id: int,
) -> None:
    summary = entry.get("color_summary")
    if not isinstance(summary, dict):
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "block color summary is missing", block_id)
    expected_sha = sha256_bytes(np.ascontiguousarray(rgba).tobytes())
    if summary.get("rgba_sha256") != expected_sha:
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "block rgba summary hash differs", block_id)
    expected_owner_sha = sha256_bytes(np.ascontiguousarray(ownership).tobytes())
    if summary.get("ownership_sha256") != expected_owner_sha:
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "block ownership summary hash differs", block_id)
    means = summary.get("mean_rgba")
    actual_means = rgba.reshape(-1, 4).mean(axis=0)
    if not isinstance(means, list) or len(means) != 4:
        raise SnapshotInvalid("SNAPSHOT_CORRUPT", "block mean color summary is malformed", block_id)
    for stored, actual in zip(means, actual_means):
        if not isinstance(stored, (int, float)) or abs(float(stored) - float(actual)) > 1e-6:
            raise SnapshotInvalid("SNAPSHOT_CORRUPT", "block mean color summary differs", block_id)
