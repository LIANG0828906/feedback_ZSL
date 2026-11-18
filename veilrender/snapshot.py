"""Atomic per-tile snapshots and strict all-or-nothing recovery."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import struct
from pathlib import Path
from typing import Any

import numpy as np

from .scene import canonical_json
from .version import ALGORITHM_VERSION

SNAPSHOT_SCHEMA = "veil-snapshot/1"
_TILE_MAGIC = b"VEILTILE2"
_TILE_VERSION = 2


class SnapshotError(RuntimeError):
    pass


def _sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    try:
        directory_fd = os.open(path.parent, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def _encode_tile(
    tile_id: int,
    rgba: np.ndarray,
    ownership: np.ndarray,
    stats: dict[str, np.ndarray],
) -> bytes:
    height, width = rgba.shape[:2]
    layer_count = int(stats["hit_count"].shape[0])
    header = struct.pack(
        "<9sIIIII",
        _TILE_MAGIC,
        _TILE_VERSION,
        int(tile_id),
        int(height),
        int(width),
        layer_count,
    )
    chunks = [header, np.ascontiguousarray(rgba, dtype=np.uint8).tobytes()]
    chunks.append(np.ascontiguousarray(ownership, dtype=np.uint8).tobytes())
    for index in range(layer_count):
        chunks.append(
            struct.pack(
                "<Qddd",
                int(stats["hit_count"][index]),
                float(stats["transmittance_sum"][index]),
                float(stats["transmission_contribution_sum"][index]),
                float(stats["reflection_contribution_sum"][index]),
            )
        )
    return b"".join(chunks)


def _decode_tile(
    data: bytes,
    expected_tile_id: int,
    expected_shape: tuple[int, int],
    layer_count: int,
) -> tuple[np.ndarray, np.ndarray, dict[str, np.ndarray]]:
    header_size = struct.calcsize("<9sIIIII")
    if len(data) < header_size:
        raise SnapshotError("truncated tile header")
    magic, version, tile_id, height, width, stored_layers = struct.unpack(
        "<9sIIIII", data[:header_size]
    )
    if magic != _TILE_MAGIC or version != _TILE_VERSION:
        raise SnapshotError("tile magic/version mismatch")
    if int(tile_id) != int(expected_tile_id):
        raise SnapshotError("tile id mismatch")
    if (int(height), int(width)) != expected_shape:
        raise SnapshotError("tile dimensions do not match manifest")
    if int(stored_layers) != int(layer_count):
        raise SnapshotError("tile layer count does not match scene")
    pixel_bytes = height * width * 4
    stats_size = layer_count * struct.calcsize("<Qddd")
    expected_size = header_size + pixel_bytes * 2 + stats_size
    if len(data) != expected_size:
        raise SnapshotError("tile payload length mismatch")
    offset = header_size
    rgba = np.frombuffer(data[offset : offset + pixel_bytes], dtype=np.uint8).copy().reshape(
        height, width, 4
    )
    offset += pixel_bytes
    ownership = (
        np.frombuffer(data[offset : offset + pixel_bytes], dtype=np.uint8)
        .copy()
        .reshape(height, width, 4)
    )
    offset += pixel_bytes
    hit_count = np.empty(layer_count, dtype=np.uint64)
    transmittance_sum = np.empty(layer_count, dtype=np.float64)
    transmission_contribution_sum = np.empty(layer_count, dtype=np.float64)
    reflection_contribution_sum = np.empty(layer_count, dtype=np.float64)
    for index in range(layer_count):
        hit, transmittance, transmission, reflection = struct.unpack(
            "<Qddd", data[offset : offset + struct.calcsize("<Qddd")]
        )
        offset += struct.calcsize("<Qddd")
        hit_count[index] = hit
        transmittance_sum[index] = transmittance
        transmission_contribution_sum[index] = transmission
        reflection_contribution_sum[index] = reflection
    stats = {
        "hit_count": hit_count,
        "transmittance_sum": transmittance_sum,
        "transmission_contribution_sum": transmission_contribution_sum,
        "reflection_contribution_sum": reflection_contribution_sum,
    }
    return rgba, ownership, stats


class SnapshotStore:
    def __init__(
        self,
        output_dir: str | Path,
        *,
        scene_digest: str,
        scene_layer_count: int,
        image_width: int,
        image_height: int,
        tile_width: int,
        tile_height: int,
    ) -> None:
        self.output_dir = Path(output_dir)
        self.root = self.output_dir / "snapshot"
        self.tile_dir = self.root / "tiles"
        self.manifest_path = self.root / "manifest.json"
        self.scene_digest = scene_digest
        self.scene_layer_count = int(scene_layer_count)
        self.image_width = int(image_width)
        self.image_height = int(image_height)
        self.tile_width = int(tile_width)
        self.tile_height = int(tile_height)
        self.tiles_x = (self.image_width + self.tile_width - 1) // self.tile_width
        self.tiles_y = (self.image_height + self.tile_height - 1) // self.tile_height
        self.total_tiles = self.tiles_x * self.tiles_y
        self.entries: dict[int, dict[str, Any]] = {}

    def tile_origin(self, tile_id: int) -> tuple[int, int]:
        if not 0 <= tile_id < self.total_tiles:
            raise SnapshotError(f"tile id {tile_id} outside [0,{self.total_tiles})")
        return (
            (tile_id % self.tiles_x) * self.tile_width,
            (tile_id // self.tiles_x) * self.tile_height,
        )

    def tile_shape(self, tile_id: int) -> tuple[int, int]:
        x0, y0 = self.tile_origin(tile_id)
        return (
            min(self.tile_height, self.image_height - y0),
            min(self.tile_width, self.image_width - x0),
        )

    def tile_path(self, tile_id: int) -> Path:
        return self.tile_dir / f"tile-{tile_id:08d}.tile"

    def completed_ids(self) -> list[int]:
        return sorted(self.entries)

    def pending_ids(self) -> list[int]:
        return [tile_id for tile_id in range(self.total_tiles) if tile_id not in self.entries]

    def _manifest_payload(self) -> dict[str, Any]:
        return {
            "schema_version": SNAPSHOT_SCHEMA,
            "algorithm_version": ALGORITHM_VERSION,
            "scene_digest": self.scene_digest,
            "image": {
                "width": self.image_width,
                "height": self.image_height,
                "tile_width": self.tile_width,
                "tile_height": self.tile_height,
                "tiles_x": self.tiles_x,
                "tiles_y": self.tiles_y,
                "total_tiles": self.total_tiles,
                "layer_count": self.scene_layer_count,
            },
            "commit_count": len(self.entries),
            "completed": {
                str(tile_id): self.entries[tile_id] for tile_id in sorted(self.entries)
            },
        }

    def _write_manifest(self) -> None:
        payload = canonical_json(self._manifest_payload()) + "\n"
        _atomic_write(self.manifest_path, payload.encode("utf-8"))

    def discard(self) -> None:
        self.entries = {}
        if self.root.exists():
            shutil.rmtree(self.root)
        self.tile_dir.mkdir(parents=True, exist_ok=True)
        self._write_manifest()

    def _validate_manifest_structure(self, manifest: Any) -> bool:
        if not isinstance(manifest, dict):
            return False
        if manifest.get("schema_version") != SNAPSHOT_SCHEMA:
            return False
        if manifest.get("algorithm_version") != ALGORITHM_VERSION:
            return False
        if manifest.get("scene_digest") != self.scene_digest:
            return False
        image = manifest.get("image")
        expected_image = {
            "width": self.image_width,
            "height": self.image_height,
            "tile_width": self.tile_width,
            "tile_height": self.tile_height,
            "tiles_x": self.tiles_x,
            "tiles_y": self.tiles_y,
            "total_tiles": self.total_tiles,
            "layer_count": self.scene_layer_count,
        }
        if image != expected_image:
            return False
        completed = manifest.get("completed")
        if not isinstance(completed, dict):
            return False
        if int(manifest.get("commit_count", -1)) != len(completed):
            return False
        if len(completed) > self.total_tiles:
            return False
        for key, entry in completed.items():
            try:
                tile_id = int(key)
            except (TypeError, ValueError):
                return False
            if str(tile_id) != key or not 0 <= tile_id < self.total_tiles:
                return False
            if not isinstance(entry, dict):
                return False
            required = {"file", "file_sha256", "rgba_sha256", "ownership_sha256"}
            if not required.issubset(entry):
                return False
            if entry["file"] != self.tile_path(tile_id).name:
                return False
            if not all(
                isinstance(entry[name], str) and len(entry[name]) == 64
                for name in ("file_sha256", "rgba_sha256", "ownership_sha256")
            ):
                return False
        return True

    def load(self) -> tuple[str, str | None]:
        """Load committed tiles.

        Returns (mode, invalidation_reason), where mode is one of
        ``empty``, ``resumed`` or ``reset``.  Any damaged component discards
        the entire snapshot; no individual stale tile is trusted.
        """

        if not self.root.exists() and not self.manifest_path.exists():
            self.tile_dir.mkdir(parents=True, exist_ok=True)
            self._write_manifest()
            return "empty", None
        reason: str | None = None
        try:
            if not self.manifest_path.is_file():
                raise SnapshotError("manifest is missing")
            manifest = json.loads(self.manifest_path.read_text(encoding="utf-8"))
            if not self._validate_manifest_structure(manifest):
                raise SnapshotError("manifest structure or scene summary mismatch")
            loaded: dict[int, dict[str, Any]] = {}
            for key, entry in manifest["completed"].items():
                tile_id = int(key)
                tile_path = self.tile_path(tile_id)
                if not tile_path.is_file():
                    raise SnapshotError(f"tile {tile_id} is missing")
                data = tile_path.read_bytes()
                if _sha256_bytes(data) != entry["file_sha256"]:
                    raise SnapshotError(f"tile {tile_id} checksum mismatch")
                rgba, ownership, _stats = _decode_tile(
                    data,
                    tile_id,
                    self.tile_shape(tile_id),
                    self.scene_layer_count,
                )
                if _sha256_bytes(rgba.tobytes(order="C")) != entry["rgba_sha256"]:
                    raise SnapshotError(f"tile {tile_id} rgba summary mismatch")
                if _sha256_bytes(ownership.tobytes(order="C")) != entry["ownership_sha256"]:
                    raise SnapshotError(f"tile {tile_id} ownership summary mismatch")
                loaded[tile_id] = entry
            self.entries = loaded
            return ("resumed" if loaded else "empty"), None
        except (OSError, ValueError, SnapshotError, json.JSONDecodeError) as exc:
            reason = str(exc)
        self.discard()
        return "reset", reason

    def commit(
        self,
        tile_id: int,
        rgba: np.ndarray,
        ownership: np.ndarray,
        stats: dict[str, np.ndarray],
    ) -> None:
        if tile_id in self.entries:
            raise SnapshotError(f"tile {tile_id} was already committed")
        expected_shape = self.tile_shape(tile_id)
        if tuple(rgba.shape[:2]) != expected_shape or tuple(ownership.shape[:2]) != expected_shape:
            raise SnapshotError("tile shape does not match image geometry")
        data = _encode_tile(tile_id, rgba, ownership, stats)
        tile_path = self.tile_path(tile_id)
        _atomic_write(tile_path, data)
        entry = {
            "file": tile_path.name,
            "file_sha256": _sha256_bytes(data),
            "rgba_sha256": _sha256_bytes(np.ascontiguousarray(rgba).tobytes(order="C")),
            "ownership_sha256": _sha256_bytes(
                np.ascontiguousarray(ownership).tobytes(order="C")
            ),
        }
        self.entries[tile_id] = entry
        self._write_manifest()

    def read_committed(
        self, tile_id: int
    ) -> tuple[np.ndarray, np.ndarray, dict[str, np.ndarray]]:
        if tile_id not in self.entries:
            raise SnapshotError(f"tile {tile_id} is not committed")
        entry = self.entries[tile_id]
        tile_path = self.tile_path(tile_id)
        data = tile_path.read_bytes()
        if _sha256_bytes(data) != entry["file_sha256"]:
            raise SnapshotError(f"tile {tile_id} changed after manifest validation")
        return _decode_tile(
            data,
            tile_id,
            self.tile_shape(tile_id),
            self.scene_layer_count,
        )

    def load_all(
        self,
    ) -> dict[int, tuple[np.ndarray, np.ndarray, dict[str, np.ndarray]]]:
        return {tile_id: self.read_committed(tile_id) for tile_id in self.completed_ids()}

