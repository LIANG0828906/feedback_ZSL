"""Atomic progress snapshots and resume validation."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import zlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from . import __version__
from .scene import Scene

SNAPSHOT_VERSION = 1


@dataclass
class LoadedSnapshot:
    blocks: dict[int, tuple[np.ndarray, np.ndarray]]
    recovery_count: int
    completed_ids: list[int]


@dataclass
class SnapshotLoadResult:
    snapshot: LoadedSnapshot | None
    discarded: bool
    reason: str


def _canonical_bytes(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False).encode("utf-8")


def _checksum(value: dict[str, Any]) -> str:
    payload = dict(value)
    payload.pop("snapshot_sha256", None)
    return hashlib.sha256(_canonical_bytes(payload)).hexdigest()


def _encode_array(array: np.ndarray, dtype: str) -> str:
    raw = np.asarray(array).astype(dtype, copy=False).tobytes()
    return base64.b64encode(zlib.compress(raw, 9)).decode("ascii")


def _decode_array(encoded: str, dtype: str, shape: tuple[int, ...], expected_digest: str) -> np.ndarray:
    compressed = base64.b64decode(encoded.encode("ascii"), validate=True)
    raw = zlib.decompress(compressed)
    digest = hashlib.sha256(raw).hexdigest()
    if digest != expected_digest:
        raise ValueError("block payload digest mismatch")
    array = np.frombuffer(raw, dtype=dtype)
    expected_size = int(np.prod(shape))
    if array.size != expected_size:
        raise ValueError("block payload size mismatch")
    return array.reshape(shape).astype("<u2" if dtype == "<u2" else np.uint8, copy=True)


def block_color_summary(rgba16: np.ndarray, layer_map: np.ndarray) -> dict[str, Any]:
    return {
        "rgba16_mean": [round(float(value) / 65535.0, 9) for value in np.mean(rgba16, axis=(0, 1))],
        "layer_map_mean": [round(float(value) / 255.0, 9) for value in np.mean(layer_map, axis=(0, 1))],
    }


def _snapshot_block(scene: Scene, block_id: int, rgba16: np.ndarray, layer_map: np.ndarray) -> dict[str, Any]:
    x, y, width, height = scene.renderer.block_rect(block_id)
    rgba_raw = rgba16.astype("<u2", copy=False).tobytes()
    layer_raw = layer_map.astype("u1", copy=False).tobytes()
    return {
        "id": block_id,
        "x": x,
        "y": y,
        "width": width,
        "height": height,
        "rgba_sha256": hashlib.sha256(rgba_raw).hexdigest(),
        "layer_sha256": hashlib.sha256(layer_raw).hexdigest(),
        "rgba_zlib_b64": _encode_array(rgba16, "<u2"),
        "layer_zlib_b64": _encode_array(layer_map, "u1"),
        "color_summary": block_color_summary(rgba16, layer_map),
    }


def build_snapshot(
    scene: Scene,
    blocks: dict[int, tuple[np.ndarray, np.ndarray]],
    recovery_count: int,
) -> dict[str, Any]:
    ordered = sorted(blocks)
    snapshot: dict[str, Any] = {
        "snapshot_version": SNAPSHOT_VERSION,
        "renderer_version": __version__,
        "scene_digest": scene.digest,
        "scene_summary": scene.summary,
        "layout": {
            "width": scene.renderer.width,
            "height": scene.renderer.height,
            "block_size": [scene.renderer.block_size[0], scene.renderer.block_size[1]],
            "blocks_x": scene.renderer.blocks_x,
            "blocks_y": scene.renderer.blocks_y,
            "block_count": scene.renderer.block_count,
        },
        "recovery_count": int(recovery_count),
        "completed_count": len(ordered),
        "completed_block_ids": ordered,
        "completed_blocks": [_snapshot_block(scene, block_id, *blocks[block_id]) for block_id in ordered],
    }
    snapshot["snapshot_sha256"] = _checksum(snapshot)
    return snapshot


def write_snapshot_atomic(path: Path, snapshot: dict[str, Any]) -> None:
    data = _canonical_bytes(snapshot) + b"\n"
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    try:
        directory_fd = os.open(str(path.parent), os.O_DIRECTORY)
    except (AttributeError, OSError):
        return
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def _validate_layout(raw_layout: Any, scene: Scene) -> None:
    expected = {
        "width": scene.renderer.width,
        "height": scene.renderer.height,
        "block_size": [scene.renderer.block_size[0], scene.renderer.block_size[1]],
        "blocks_x": scene.renderer.blocks_x,
        "blocks_y": scene.renderer.blocks_y,
        "block_count": scene.renderer.block_count,
    }
    if raw_layout != expected:
        raise ValueError("snapshot block layout does not match scene")


def load_snapshot(path: Path, scene: Scene) -> SnapshotLoadResult:
    if not path.exists():
        return SnapshotLoadResult(None, False, "missing")
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(raw, dict):
            raise ValueError("snapshot root is not an object")
        if raw.get("snapshot_version") != SNAPSHOT_VERSION:
            raise ValueError("snapshot version mismatch")
        if raw.get("renderer_version") != __version__:
            raise ValueError("renderer version mismatch")
        if raw.get("snapshot_sha256") != _checksum(raw):
            raise ValueError("snapshot checksum mismatch")
        if raw.get("scene_digest") != scene.digest:
            raise ValueError("scene digest mismatch")
        if raw.get("scene_summary") != scene.summary:
            raise ValueError("scene summary mismatch")
        _validate_layout(raw.get("layout"), scene)
        recovery_count = raw.get("recovery_count")
        if isinstance(recovery_count, bool) or not isinstance(recovery_count, int) or recovery_count < 0:
            raise ValueError("invalid recovery count")
        blocks_raw = raw.get("completed_blocks")
        ids_raw = raw.get("completed_block_ids")
        if not isinstance(blocks_raw, list) or not isinstance(ids_raw, list):
            raise ValueError("missing completed block list")
        if len(blocks_raw) != len(ids_raw) or raw.get("completed_count") != len(blocks_raw):
            raise ValueError("inconsistent completed block count")
        if len(blocks_raw) == 0 or len(blocks_raw) > scene.renderer.block_count:
            raise ValueError("invalid completed block count")
        blocks: dict[int, tuple[np.ndarray, np.ndarray]] = {}
        for entry in blocks_raw:
            if not isinstance(entry, dict):
                raise ValueError("invalid block entry")
            block_id = entry.get("id")
            if isinstance(block_id, bool) or not isinstance(block_id, int):
                raise ValueError("invalid block id")
            if block_id < 0 or block_id >= scene.renderer.block_count or block_id in blocks:
                raise ValueError("block id out of range or duplicated")
            x, y, width, height = scene.renderer.block_rect(block_id)
            if [entry.get("x"), entry.get("y"), entry.get("width"), entry.get("height")] != [x, y, width, height]:
                raise ValueError("stored block offset does not match scene layout")
            if not isinstance(entry.get("color_summary"), dict):
                raise ValueError("missing block color summary")
            rgba = _decode_array(entry.get("rgba_zlib_b64", ""), "<u2", (height, width, 4), entry.get("rgba_sha256", ""))
            layer = _decode_array(entry.get("layer_zlib_b64", ""), "u1", (height, width, 4), entry.get("layer_sha256", ""))
            expected_summary = block_color_summary(rgba, layer)
            if entry.get("color_summary") != expected_summary:
                raise ValueError("block color summary mismatch")
            blocks[block_id] = (rgba, layer)
        completed_ids = sorted(blocks)
        if ids_raw != completed_ids:
            raise ValueError("completed block ids are not canonical")
        return SnapshotLoadResult(
            LoadedSnapshot(blocks=blocks, recovery_count=recovery_count, completed_ids=completed_ids),
            False,
            "resumed",
        )
    except (OSError, ValueError, KeyError, TypeError, zlib.error, json.JSONDecodeError, base64.binascii.Error) as exc:
        return SnapshotLoadResult(None, True, f"{exc.__class__.__name__}: {exc}")
