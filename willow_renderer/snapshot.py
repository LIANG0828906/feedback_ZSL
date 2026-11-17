"""Crash-safe tiled rendering snapshots.

After every tile the whole snapshot state is committed atomically
(write-temp-then-rename, i.e. whole-file replacement).  The snapshot
records the scene content digest, the completed tile ids and a colour
digest per tile.  On restart the snapshot is only reused when every
check passes; otherwise it is discarded wholesale and rendering starts
from scratch - stale tile offsets or texture samples are never reused.
"""

import hashlib
import json
import os

import numpy as np

SNAPSHOT_JSON = "_snapshot.json"
PARTIAL_RGBA = "_partial_rgba.npy"
PARTIAL_OWNER = "_partial_owner.npy"


def _sha256(data):
    return hashlib.sha256(data).hexdigest()


def _atomic_write(path, writer):
    tmp = path + ".tmp"
    with open(tmp, "wb") as fh:
        writer(fh)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def tile_digest(rgba_tile, owner_tile):
    return _sha256(rgba_tile.tobytes() + owner_tile.tobytes())


class SnapshotManager:
    def __init__(self, outdir, scene_digest, width, height, tile_size):
        self.outdir = outdir
        self.scene_digest = scene_digest
        self.width = width
        self.height = height
        self.tile_size = tile_size
        self.rgba = np.zeros((height, width, 4), dtype=np.uint8)
        self.owner = np.full((height, width), 255, dtype=np.uint8)
        self.completed = []
        self.tile_digests = {}

    @property
    def json_path(self):
        return os.path.join(self.outdir, SNAPSHOT_JSON)

    @property
    def rgba_path(self):
        return os.path.join(self.outdir, PARTIAL_RGBA)

    @property
    def owner_path(self):
        return os.path.join(self.outdir, PARTIAL_OWNER)

    def exists(self):
        return (os.path.isfile(self.json_path)
                or os.path.isfile(self.rgba_path)
                or os.path.isfile(self.owner_path))

    def commit_tile(self, tile_id, tx, ty, rgba_tile, owner_tile):
        th, tw = rgba_tile.shape[0], rgba_tile.shape[1]
        self.rgba[ty:ty + th, tx:tx + tw] = rgba_tile
        self.owner[ty:ty + th, tx:tx + tw] = owner_tile
        self.completed.append(tile_id)
        self.tile_digests[str(tile_id)] = tile_digest(rgba_tile, owner_tile)
        self._commit()

    def _commit(self):
        _atomic_write(self.rgba_path, lambda fh: np.save(fh, self.rgba))
        _atomic_write(self.owner_path, lambda fh: np.save(fh, self.owner))
        payload = {
            "version": 1,
            "scene_digest": self.scene_digest,
            "width": self.width,
            "height": self.height,
            "tile_size": self.tile_size,
            "completed_tiles": list(self.completed),
            "tile_digests": dict(self.tile_digests),
            "rgba_sha256": _sha256(self.rgba.tobytes()),
            "owner_sha256": _sha256(self.owner.tobytes()),
        }
        text = json.dumps(payload, sort_keys=True, indent=1)
        _atomic_write(self.json_path, lambda fh: fh.write(text.encode("utf-8")))

    def try_restore(self, tiles):
        """Attempt to restore committed state.

        Returns (restored_tile_ids, discard_reason).  Any inconsistency
        yields a non-None discard_reason and no restored state.
        """
        try:
            if not (os.path.isfile(self.json_path)
                    and os.path.isfile(self.rgba_path)
                    and os.path.isfile(self.owner_path)):
                return [], "snapshot files incomplete"
            with open(self.json_path, "r", encoding="utf-8") as fh:
                payload = json.load(fh)
            if payload.get("scene_digest") != self.scene_digest:
                return [], "scene digest mismatch"
            if (payload.get("width") != self.width
                    or payload.get("height") != self.height
                    or payload.get("tile_size") != self.tile_size):
                return [], "image or tile geometry mismatch"
            rgba = np.load(self.rgba_path)
            owner = np.load(self.owner_path)
            if rgba.shape != (self.height, self.width, 4) or rgba.dtype != np.uint8:
                return [], "partial rgba buffer malformed"
            if owner.shape != (self.height, self.width) or owner.dtype != np.uint8:
                return [], "partial owner buffer malformed"
            if _sha256(rgba.tobytes()) != payload.get("rgba_sha256"):
                return [], "partial rgba digest mismatch"
            if _sha256(owner.tobytes()) != payload.get("owner_sha256"):
                return [], "partial owner digest mismatch"
            tile_digests = payload.get("tile_digests", {})
            by_id = {t[0]: t for t in tiles}
            restored = []
            for tile_id in payload.get("completed_tiles", []):
                if tile_id not in by_id:
                    return [], f"unknown tile id {tile_id}"
                _, tx, ty, tw, th = by_id[tile_id]
                rgba_tile = rgba[ty:ty + th, tx:tx + tw]
                owner_tile = owner[ty:ty + th, tx:tx + tw]
                if tile_digest(rgba_tile, owner_tile) != tile_digests.get(str(tile_id)):
                    return [], f"tile {tile_id} digest mismatch"
                restored.append(tile_id)
            if {str(t) for t in restored} != set(tile_digests.keys()):
                return [], "tile digest table inconsistent"
            self.rgba = rgba
            self.owner = owner
            self.completed = list(payload["completed_tiles"])
            self.tile_digests = dict(tile_digests)
            return restored, None
        except (OSError, ValueError, KeyError, json.JSONDecodeError) as exc:
            return [], f"snapshot unreadable: {exc}"

    def discard(self):
        for path in (self.json_path, self.rgba_path, self.owner_path):
            try:
                os.remove(path)
            except FileNotFoundError:
                pass
        self.rgba = np.zeros((self.height, self.width, 4), dtype=np.uint8)
        self.owner = np.full((self.height, self.width), 255, dtype=np.uint8)
        self.completed = []
        self.tile_digests = {}

    def finish(self):
        """Successful completion: drop the recovery state."""
        self.discard()
