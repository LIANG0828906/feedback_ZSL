from __future__ import annotations

import base64
import json
import math
import zlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .util import canonical_json_bytes, sha256_array, sha256_bytes, write_json_atomic
from .validation import SceneData

SNAPSHOT_FORMAT = "canopy-snapshot"
SNAPSHOT_VERSION = 1


@dataclass(frozen=True)
class TileLayout:
    width: int
    height: int
    tile_width: int
    tile_height: int

    @property
    def tiles_x(self) -> int:
        return (self.width + self.tile_width - 1) // self.tile_width

    @property
    def tiles_y(self) -> int:
        return (self.height + self.tile_height - 1) // self.tile_height

    @property
    def total(self) -> int:
        return self.tiles_x * self.tiles_y

    def rect(self, tile_index: int) -> tuple[int, int, int, int]:
        if tile_index < 0 or tile_index >= self.total:
            raise IndexError("tile index out of range")
        tile_x = tile_index % self.tiles_x
        tile_y = tile_index // self.tiles_x
        x0 = tile_x * self.tile_width
        y0 = tile_y * self.tile_height
        return (
            x0,
            y0,
            min(self.tile_width, self.width - x0),
            min(self.tile_height, self.height - y0),
        )

    def as_dict(self) -> dict[str, int]:
        return {
            "width": self.width,
            "height": self.height,
            "tile_width": self.tile_width,
            "tile_height": self.tile_height,
            "tiles_x": self.tiles_x,
            "tiles_y": self.tiles_y,
            "total_tiles": self.total,
        }


@dataclass
class TileResult:
    tile_index: int
    rgba: np.ndarray
    owner: np.ndarray
    fabric: np.ndarray
    layer_rgba: dict[int, np.ndarray]
    stats: dict[str, Any]

    @property
    def color_digest(self) -> str:
        return tile_color_digest(
            self.rgba,
            self.owner,
            self.fabric,
            self.layer_rgba,
        )


def tile_color_digest(
    rgba: np.ndarray,
    owner: np.ndarray,
    fabric: np.ndarray,
    layer_rgba: dict[int, np.ndarray],
) -> str:
    pieces = [
        np.ascontiguousarray(rgba).tobytes(order="C"),
        np.ascontiguousarray(owner).tobytes(order="C"),
        np.ascontiguousarray(fabric).tobytes(order="C"),
    ]
    for layer_index in sorted(layer_rgba):
        pieces.append(int(layer_index).to_bytes(4, "little", signed=False))
        pieces.append(
            np.ascontiguousarray(layer_rgba[layer_index]).tobytes(order="C")
        )
    return sha256_bytes(b"".join(pieces))


@dataclass
class SnapshotState:
    tiles: dict[int, TileResult]
    recovery_count: int
    snapshot_digest: str


class SnapshotRejected(Exception):
    def __init__(self, reason: str):
        super().__init__(reason)
        self.reason = reason


def _encode_array(array: np.ndarray) -> str:
    raw = np.ascontiguousarray(array).tobytes(order="C")
    return base64.b64encode(zlib.compress(raw, level=9)).decode("ascii")


def _decode_array(
    payload: Any,
    dtype: np.dtype,
    shape: tuple[int, ...],
) -> np.ndarray:
    if not isinstance(payload, str):
        raise SnapshotRejected("snapshot_payload_not_string")
    try:
        compressed = base64.b64decode(payload.encode("ascii"), validate=True)
        raw = zlib.decompress(compressed)
    except Exception as exc:
        raise SnapshotRejected("snapshot_payload_undecodable") from exc
    expected = int(np.prod(shape)) * np.dtype(dtype).itemsize
    if len(raw) != expected:
        raise SnapshotRejected("snapshot_payload_length_mismatch")
    return np.frombuffer(raw, dtype=dtype).reshape(shape).copy()


def _snapshot_digest(document: dict[str, Any]) -> str:
    body = {key: value for key, value in document.items() if key != "snapshot_digest"}
    return sha256_bytes(canonical_json_bytes(body))


def _validate_stats(stats: Any, layer_indices: set[int]) -> dict[str, Any]:
    if not isinstance(stats, dict):
        raise SnapshotRejected("snapshot_stats_invalid")
    layers = stats.get("layers")
    if not isinstance(layers, dict):
        raise SnapshotRejected("snapshot_stats_layers_invalid")
    normalized_layers: dict[str, dict[str, Any]] = {}
    if set(layers.keys()) != {str(index) for index in layer_indices}:
        raise SnapshotRejected("snapshot_stats_layer_set_mismatch")
    for key in sorted(layers, key=lambda item: int(item)):
        item = layers[key]
        if not isinstance(item, dict):
            raise SnapshotRejected("snapshot_stats_layer_invalid")
        normalized: dict[str, Any] = {}
        for field in (
            "visible_fragments",
            "transmission_energy",
            "reflection_energy",
            "effective_transmittance_sum",
        ):
            value = item.get(field)
            if not isinstance(value, (int, float)) or isinstance(value, bool):
                raise SnapshotRejected("snapshot_stats_value_invalid")
            if not math.isfinite(float(value)):
                raise SnapshotRejected("snapshot_stats_value_invalid")
            normalized[field] = value
        normalized_layers[key] = normalized
    shadow_rays = stats.get("shadow_rays_evaluated")
    fabric_pixels = stats.get("fabric_pixels")
    if (
        not isinstance(shadow_rays, int)
        or isinstance(shadow_rays, bool)
        or shadow_rays < 0
        or not isinstance(fabric_pixels, int)
        or isinstance(fabric_pixels, bool)
        or fabric_pixels < 0
    ):
        raise SnapshotRejected("snapshot_stats_value_invalid")
    return {
        "layers": normalized_layers,
        "shadow_rays_evaluated": shadow_rays,
        "fabric_pixels": fabric_pixels,
    }


def save_snapshot(
    path: Path,
    scene: SceneData,
    layout: TileLayout,
    tiles: dict[int, TileResult],
    recovery_count: int,
) -> str:
    completed = sorted(tiles)
    tile_documents: list[dict[str, Any]] = []
    layer_indices = {layer.layer_index for layer in scene.layers}
    for tile_index in completed:
        tile = tiles[tile_index]
        tile_documents.append(
            {
                "tile_index": tile_index,
                "rgba_b64": _encode_array(tile.rgba),
                "owner_b64": _encode_array(tile.owner),
                "fabric_b64": _encode_array(tile.fabric),
                "layer_b64": {
                    str(layer_index): _encode_array(tile.layer_rgba[layer_index])
                    for layer_index in sorted(tile.layer_rgba)
                },
                "rgba_sha256": sha256_array(tile.rgba),
                "owner_sha256": sha256_array(tile.owner),
                "fabric_sha256": sha256_array(tile.fabric),
                "layer_sha256": {
                    str(layer_index): sha256_array(tile.layer_rgba[layer_index])
                    for layer_index in sorted(tile.layer_rgba)
                },
                "tile_color_sha256": tile.color_digest,
                "stats": _validate_stats(tile.stats, layer_indices),
            }
        )
    document: dict[str, Any] = {
        "format": SNAPSHOT_FORMAT,
        "version": SNAPSHOT_VERSION,
        "scene_digest": scene.digest,
        "canvas": {"width": layout.width, "height": layout.height},
        "tile": {
            "tile_width": layout.tile_width,
            "tile_height": layout.tile_height,
            "tiles_x": layout.tiles_x,
            "tiles_y": layout.tiles_y,
            "total_tiles": layout.total,
        },
        "recovery_count": int(recovery_count),
        "completed_tile_indices": completed,
        "tiles": tile_documents,
    }
    document["snapshot_digest"] = _snapshot_digest(document)
    write_json_atomic(path, document)
    return document["snapshot_digest"]


def load_snapshot(
    path: Path,
    scene: SceneData,
    layout: TileLayout,
) -> SnapshotState:
    if not path.exists():
        raise SnapshotRejected("snapshot_missing")
    try:
        with open(path, "r", encoding="utf-8") as handle:
            document = json.load(handle)
    except Exception as exc:
        raise SnapshotRejected("snapshot_json_invalid") from exc
    if not isinstance(document, dict):
        raise SnapshotRejected("snapshot_root_invalid")
    if document.get("format") != SNAPSHOT_FORMAT:
        raise SnapshotRejected("snapshot_format_mismatch")
    if document.get("version") != SNAPSHOT_VERSION:
        raise SnapshotRejected("snapshot_version_unsupported")
    digest = document.get("snapshot_digest")
    if not isinstance(digest, str) or digest != _snapshot_digest(document):
        raise SnapshotRejected("snapshot_digest_mismatch")
    if document.get("scene_digest") != scene.digest:
        raise SnapshotRejected("snapshot_scene_digest_mismatch")
    if document.get("canvas") != {
        "width": layout.width,
        "height": layout.height,
    }:
        raise SnapshotRejected("snapshot_canvas_mismatch")
    if document.get("tile") != {
        "tile_width": layout.tile_width,
        "tile_height": layout.tile_height,
        "tiles_x": layout.tiles_x,
        "tiles_y": layout.tiles_y,
        "total_tiles": layout.total,
    }:
        raise SnapshotRejected("snapshot_tile_layout_mismatch")
    recovery_count = document.get("recovery_count")
    if (
        not isinstance(recovery_count, int)
        or isinstance(recovery_count, bool)
        or recovery_count < 0
    ):
        raise SnapshotRejected("snapshot_recovery_count_invalid")
    completed = document.get("completed_tile_indices")
    if (
        not isinstance(completed, list)
        or not all(
            isinstance(index, int) and not isinstance(index, bool)
            for index in completed
        )
        or completed != sorted(set(completed))
        or any(index < 0 or index >= layout.total for index in completed)
    ):
        raise SnapshotRejected("snapshot_completed_tiles_invalid")
    tile_documents = document.get("tiles")
    if not isinstance(tile_documents, list) or len(tile_documents) != len(completed):
        raise SnapshotRejected("snapshot_tile_payload_count_mismatch")

    layer_indices = {layer.layer_index for layer in scene.layers}
    tiles: dict[int, TileResult] = {}
    for expected_index, tile_document in zip(completed, tile_documents):
        if not isinstance(tile_document, dict):
            raise SnapshotRejected("snapshot_tile_payload_invalid")
        if tile_document.get("tile_index") != expected_index:
            raise SnapshotRejected("snapshot_tile_index_mismatch")
        # Offsets are intentionally recomputed from the current scene/layout.
        _, _, tile_width, tile_height = layout.rect(expected_index)
        rgba = _decode_array(
            tile_document.get("rgba_b64"),
            np.dtype(np.uint8),
            (tile_height, tile_width, 4),
        )
        owner = _decode_array(
            tile_document.get("owner_b64"),
            np.dtype(np.uint16),
            (tile_height, tile_width),
        )
        fabric = _decode_array(
            tile_document.get("fabric_b64"),
            np.dtype(np.uint8),
            (tile_height, tile_width, 4),
        )
        layer_payloads = tile_document.get("layer_b64")
        layer_digests = tile_document.get("layer_sha256")
        if (
            not isinstance(layer_payloads, dict)
            or not isinstance(layer_digests, dict)
            or set(layer_payloads) != {str(index) for index in layer_indices}
            or set(layer_digests) != set(layer_payloads)
        ):
            raise SnapshotRejected("snapshot_layer_payload_invalid")
        layer_rgba: dict[int, np.ndarray] = {}
        for layer_index in sorted(layer_indices):
            key = str(layer_index)
            layer_array = _decode_array(
                layer_payloads.get(key),
                np.dtype(np.uint8),
                (tile_height, tile_width, 4),
            )
            if layer_digests.get(key) != sha256_array(layer_array):
                raise SnapshotRejected("snapshot_layer_digest_mismatch")
            layer_rgba[layer_index] = layer_array
        if tile_document.get("rgba_sha256") != sha256_array(rgba):
            raise SnapshotRejected("snapshot_rgba_digest_mismatch")
        if tile_document.get("owner_sha256") != sha256_array(owner):
            raise SnapshotRejected("snapshot_owner_digest_mismatch")
        if tile_document.get("fabric_sha256") != sha256_array(fabric):
            raise SnapshotRejected("snapshot_fabric_digest_mismatch")
        color_digest = tile_color_digest(rgba, owner, fabric, layer_rgba)
        if tile_document.get("tile_color_sha256") != color_digest:
            raise SnapshotRejected("snapshot_tile_color_digest_mismatch")
        stats = _validate_stats(tile_document.get("stats"), layer_indices)
        tiles[expected_index] = TileResult(
            tile_index=expected_index,
            rgba=rgba,
            owner=owner,
            fabric=fabric,
            layer_rgba=layer_rgba,
            stats=stats,
        )
    return SnapshotState(
        tiles=tiles,
        recovery_count=recovery_count,
        snapshot_digest=digest,
    )

