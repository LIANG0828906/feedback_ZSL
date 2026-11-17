from __future__ import annotations

import base64
import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
import tempfile
from typing import Any

import numpy as np

from . import RENDERER_VERSION
from .renderer import TileResult
from .scene import Scene


CHECKPOINT_FORMAT = "serene-lattice-checkpoint"
CHECKPOINT_VERSION = 1


@dataclass(frozen=True)
class SnapshotLoad:
    tiles: dict[int, TileResult]
    status: str
    reason: str | None
    completed_count: int


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _encode_array(array: np.ndarray, dtype: str) -> str:
    contiguous = np.ascontiguousarray(array, dtype=dtype)
    return base64.b64encode(contiguous.tobytes()).decode("ascii")


def _decode_array(
    encoded: Any,
    dtype: str,
    shape: tuple[int, ...],
) -> np.ndarray:
    if not isinstance(encoded, str):
        raise ValueError("payload is not a base64 string")
    raw = base64.b64decode(encoded.encode("ascii"), validate=True)
    required = int(np.prod(shape, dtype=np.int64)) * np.dtype(dtype).itemsize
    if len(raw) != required:
        raise ValueError(f"payload has {len(raw)} bytes, expected {required}")
    array = np.frombuffer(raw, dtype=dtype).copy().reshape(shape)
    return array


def _scene_summary(scene: Scene) -> dict[str, Any]:
    return {
        "digest": scene.digest,
        "renderer_version": RENDERER_VERSION,
        "width": scene.width,
        "height": scene.height,
        "tile_size": scene.tile_size,
        "layer_ids": [layer.layer_id for layer in scene.layers],
        "layer_indexes": [layer.layer_index for layer in scene.layers],
        "sun_direction": [float(value) for value in scene.sun.direction],
    }


def _tile_grid(scene: Scene) -> tuple[int, int]:
    columns = (scene.width + scene.tile_size - 1) // scene.tile_size
    rows = (scene.height + scene.tile_size - 1) // scene.tile_size
    return columns, rows


def build_snapshot_payload(
    scene: Scene,
    tiles: dict[int, TileResult],
    coordinates: list[tuple[int, int, int, int]],
    complete: bool,
) -> dict[str, Any]:
    completed_blocks: list[dict[str, Any]] = []
    for index in range(len(tiles)):
        if index not in tiles:
            raise ValueError(f"completed tile set is not a prefix; missing tile {index}")
        tile = tiles[index]
        expected = coordinates[index]
        actual = (tile.x, tile.y, tile.width, tile.height)
        if actual != expected:
            raise ValueError(
                f"tile {index} coordinates {actual} do not match {expected}"
            )
        rgba_bytes = np.ascontiguousarray(tile.rgba, dtype=np.uint8).tobytes()
        layer_bytes = np.ascontiguousarray(tile.layer_map, dtype="<i4").tobytes()
        mean_rgba = np.mean(tile.rgba.astype(np.float64), axis=(0, 1))
        completed_blocks.append(
            {
                "index": index,
                "row": tile.y // scene.tile_size,
                "column": tile.x // scene.tile_size,
                "x": tile.x,
                "y": tile.y,
                "width": tile.width,
                "height": tile.height,
                "rgba_sha256": _sha256_bytes(rgba_bytes),
                "layer_sha256": _sha256_bytes(layer_bytes),
                "color_summary": {
                    "mean_rgba": [
                        round(float(value), 8) for value in mean_rgba
                    ]
                },
                "rgba_b64": base64.b64encode(rgba_bytes).decode("ascii"),
                "layer_b64": base64.b64encode(layer_bytes).decode("ascii"),
            }
        )

    columns, rows = _tile_grid(scene)
    return {
        "format": CHECKPOINT_FORMAT,
        "version": CHECKPOINT_VERSION,
        "renderer_version": RENDERER_VERSION,
        "scene_digest": scene.digest,
        "scene_summary": _scene_summary(scene),
        "image": {
            "width": scene.width,
            "height": scene.height,
            "tile_size": scene.tile_size,
            "tile_grid": [columns, rows],
            "tile_count": len(coordinates),
        },
        "completed_blocks": completed_blocks,
        "completed_count": len(completed_blocks),
        "next_block": len(completed_blocks),
        "complete": bool(complete),
    }


def atomic_write_bytes(path: str | Path, payload: bytes) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{destination.name}.",
        suffix=".tmp",
        dir=str(destination.parent),
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_name, destination)
        try:
            directory_fd = os.open(destination.parent, os.O_RDONLY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        except OSError:
            pass
    except BaseException:
        try:
            os.unlink(temporary_name)
        except OSError:
            pass
        raise


def atomic_write_json(path: str | Path, value: Any) -> None:
    payload = json.dumps(
        value,
        indent=2,
        sort_keys=True,
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(path, payload)


def write_snapshot(
    path: str | Path,
    scene: Scene,
    tiles: dict[int, TileResult],
    coordinates: list[tuple[int, int, int, int]],
    complete: bool = False,
) -> dict[str, Any]:
    payload = build_snapshot_payload(scene, tiles, coordinates, complete)
    atomic_write_json(path, payload)
    return payload


def load_snapshot(
    path: str | Path,
    scene: Scene,
    coordinates: list[tuple[int, int, int, int]],
) -> SnapshotLoad:
    source = Path(path)
    if not source.exists():
        return SnapshotLoad({}, "new", None, 0)
    try:
        with source.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        return SnapshotLoad({}, "discarded", f"unreadable checkpoint: {exc}", 0)

    try:
        _validate_snapshot_header(payload, scene, coordinates)
        tiles = _decode_snapshot_blocks(payload, coordinates)
    except (KeyError, TypeError, ValueError, IndexError) as exc:
        return SnapshotLoad({}, "discarded", f"invalid checkpoint: {exc}", 0)
    return SnapshotLoad(
        tiles=tiles,
        status="resumed" if tiles else "empty",
        reason=None,
        completed_count=len(tiles),
    )


def _validate_snapshot_header(
    payload: Any,
    scene: Scene,
    coordinates: list[tuple[int, int, int, int]],
) -> None:
    if not isinstance(payload, dict):
        raise ValueError("checkpoint root is not an object")
    if payload.get("format") != CHECKPOINT_FORMAT:
        raise ValueError("checkpoint format marker is missing or unknown")
    if payload.get("version") != CHECKPOINT_VERSION:
        raise ValueError("checkpoint version is unknown")
    if payload.get("renderer_version") != RENDERER_VERSION:
        raise ValueError("checkpoint was produced by a different renderer version")
    if payload.get("scene_digest") != scene.digest:
        raise ValueError(
            "scene digest changed; all old tile offsets must be discarded"
        )
    summary = payload.get("scene_summary")
    if not isinstance(summary, dict) or summary.get("digest") != scene.digest:
        raise ValueError("checkpoint scene summary is inconsistent")
    image = payload.get("image")
    if not isinstance(image, dict):
        raise ValueError("checkpoint image metadata is missing")
    if (
        image.get("width") != scene.width
        or image.get("height") != scene.height
        or image.get("tile_size") != scene.tile_size
        or image.get("tile_count") != len(coordinates)
    ):
        raise ValueError("checkpoint image/tile dimensions do not match the scene")
    completed = payload.get("completed_blocks")
    if not isinstance(completed, list):
        raise ValueError("completed_blocks must be an array")
    if payload.get("completed_count") != len(completed):
        raise ValueError("completed_count does not match completed_blocks")
    if payload.get("next_block") != len(completed):
        raise ValueError("checkpoint block offset is inconsistent")
    if len(completed) > len(coordinates):
        raise ValueError("checkpoint contains more blocks than the image")
    for expected_index, block in enumerate(completed):
        if not isinstance(block, dict):
            raise ValueError(f"block {expected_index} is not an object")
        if block.get("index") != expected_index:
            raise ValueError(
                f"completed blocks must form an exact prefix; expected {expected_index}"
            )
        expected = coordinates[expected_index]
        actual = (
            block.get("x"),
            block.get("y"),
            block.get("width"),
            block.get("height"),
        )
        if actual != expected:
            raise ValueError(
                f"block {expected_index} coordinates {actual} do not match {expected}"
            )
        expected_row = expected[1] // scene.tile_size
        expected_column = expected[0] // scene.tile_size
        if (
            block.get("row") != expected_row
            or block.get("column") != expected_column
        ):
            raise ValueError(f"block {expected_index} row/column metadata is inconsistent")


def _decode_snapshot_blocks(
    payload: dict[str, Any],
    coordinates: list[tuple[int, int, int, int]],
) -> dict[int, TileResult]:
    tiles: dict[int, TileResult] = {}
    for index, block in enumerate(payload["completed_blocks"]):
        x, y, width, height = coordinates[index]
        rgba = _decode_array(block.get("rgba_b64"), np.uint8, (height, width, 4))
        layer = _decode_array(block.get("layer_b64"), "<i4", (height, width))
        if _sha256_bytes(rgba.tobytes()) != block.get("rgba_sha256"):
            raise ValueError(f"block {index} RGBA digest mismatch")
        if _sha256_bytes(layer.astype("<i4").tobytes()) != block.get("layer_sha256"):
            raise ValueError(f"block {index} layer-map digest mismatch")
        color_summary = block.get("color_summary")
        if not isinstance(color_summary, dict):
            raise ValueError(f"block {index} color summary is missing")
        mean_rgba = color_summary.get("mean_rgba")
        if not isinstance(mean_rgba, list) or len(mean_rgba) != 4:
            raise ValueError(f"block {index} color summary is malformed")
        tiles[index] = TileResult(
            x=x,
            y=y,
            width=width,
            height=height,
            rgba=rgba,
            layer_map=layer.astype(np.int32, copy=False),
        )
    return tiles


def completed_block_summary(
    tiles: dict[int, TileResult],
    coordinates: list[tuple[int, int, int, int]],
) -> list[dict[str, Any]]:
    summary: list[dict[str, Any]] = []
    for index in sorted(tiles):
        tile = tiles[index]
        summary.append(
            {
                "index": index,
                "x": tile.x,
                "y": tile.y,
                "width": tile.width,
                "height": tile.height,
                "rgba_sha256": _sha256_bytes(tile.rgba.tobytes()),
                "layer_sha256": _sha256_bytes(
                    np.ascontiguousarray(tile.layer_map, dtype="<i4").tobytes()
                ),
            }
        )
    return summary
