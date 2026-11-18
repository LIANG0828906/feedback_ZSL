"""Atomic resumable snapshot storage.

Layout under ``snapshot_dir``:
  manifest.json        - completed tile ids, scene digest, per-tile digests
  tile_<id>.npz        - one completed tile (RGBA float + optical depth)

Every file is written to a temporary name and atomically replaced, and the
manifest is the last write per tile.  On load, any digest mismatch, parse
failure or missing tile file invalidates the whole snapshot and the caller
restarts from zero; stale block offsets or old scene data are never reused.
"""

from __future__ import annotations

import json
import os

import numpy as np

from .scene import scene_digest

MANIFEST_NAME = "manifest.json"


def _tile_path(snapshot_dir: str, tile: int) -> str:
    return os.path.join(snapshot_dir, f"tile_{tile:04d}.npz")


def _manifest_path(snapshot_dir: str) -> str:
    return os.path.join(snapshot_dir, MANIFEST_NAME)


def _atomic_write_bytes(path: str, payload: bytes) -> None:
    tmp = path + ".tmp"
    with open(tmp, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)


def color_digest(rgba: np.ndarray) -> str:
    quantized = np.round(np.clip(rgba, 0.0, 1.0) * 255.0).astype(np.uint8)
    return _sha(quantized.tobytes())


def _sha(payload: bytes) -> str:
    import hashlib

    return hashlib.sha256(payload).hexdigest()


class SnapshotStore:
    def __init__(self, snapshot_dir: str, scene: dict) -> None:
        self.snapshot_dir = snapshot_dir
        self.scene = scene
        self.digest = scene_digest(scene)
        self.manifest = {
            "schema_version": 1,
            "scene_digest": self.digest,
            "image_size": [int(scene["image"]["width"]),
                           int(scene["image"]["height"])],
            "tiles": {},
        }
        self.resumed = False

    def discard_all(self) -> None:
        if not os.path.isdir(self.snapshot_dir):
            return
        for name in os.listdir(self.snapshot_dir):
            os.remove(os.path.join(self.snapshot_dir, name))

    def load(self) -> set:
        """Return resumable tile ids; any corruption discards everything."""

        path = _manifest_path(self.snapshot_dir)
        if not os.path.exists(path):
            return set()
        try:
            with open(path, "r", encoding="utf-8") as handle:
                manifest = json.load(handle)
            if manifest.get("scene_digest") != self.digest:
                raise ValueError("scene digest mismatch")
            if manifest.get("image_size") != self.manifest["image_size"]:
                raise ValueError("image size mismatch")
            tiles = manifest.get("tiles", {})
            if not isinstance(tiles, dict):
                raise ValueError("tiles must be an object")
            completed = set()
            for tile_id, entry in tiles.items():
                tile_file = _tile_path(self.snapshot_dir, int(tile_id))
                if not os.path.exists(tile_file):
                    raise ValueError(f"missing tile file {tile_id}")
                with np.load(tile_file) as data:
                    rgba = data["rgba"]
                    if _sha(rgba.astype(np.float64).tobytes()) != entry["rgba_digest"]:
                        raise ValueError(f"tile {tile_id} digest mismatch")
                completed.add(int(tile_id))
            self.manifest["tiles"] = tiles
            self.resumed = True
            return completed
        except (OSError, ValueError, KeyError, TypeError,
                json.JSONDecodeError, EOFError, np.linalg.LinAlgError):
            self.discard_all()
            return set()

    def commit_tile(self, tile: int, rgba: np.ndarray,
                    optical_depth: np.ndarray, layer_stats: dict) -> None:
        os.makedirs(self.snapshot_dir, exist_ok=True)
        tile_file = _tile_path(self.snapshot_dir, tile)
        tmp_npz = tile_file + ".tmp.npz"
        with open(tmp_npz, "wb") as handle:
            np.savez_compressed(
                handle,
                rgba=rgba.astype(np.float64),
                optical_depth=optical_depth.astype(np.float64),
                layer_stats=np.asarray(
                    json.dumps(layer_stats, sort_keys=True), dtype=object))
        os.replace(tmp_npz, tile_file)

        rgba_digest = _sha(rgba.astype(np.float64).tobytes())
        depth_digest = _sha(optical_depth.astype(np.float64).tobytes())
        self.manifest["tiles"][str(tile)] = {
            "tile": tile,
            "rgba_digest": rgba_digest,
            "depth_digest": depth_digest,
            "color_digest_8bit": color_digest(rgba),
            "visible_fragments": sum(
                item["visible_fragments"] for item in layer_stats.values()),
        }
        manifest_bytes = json.dumps(
            self.manifest, sort_keys=True, separators=(",", ":")).encode("utf-8")
        _atomic_write_bytes(_manifest_path(self.snapshot_dir), manifest_bytes)

    def read_tile(self, tile: int):
        with np.load(_tile_path(self.snapshot_dir, tile),
                     allow_pickle=True) as data:
            layer_stats = json.loads(str(data["layer_stats"]))
            layer_stats = {int(key): value for key, value in
                           layer_stats.items()}
            return {
                "rgba": data["rgba"].astype(np.float64),
                "optical_depth": data["optical_depth"].astype(np.float64),
                "layer_stats": layer_stats,
            }
