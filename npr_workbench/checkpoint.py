"""Crash-aware, atomically committed tile checkpoints."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import tempfile
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import NprError
from .model import Snapshot
from .tiles import BlockResult


class CheckpointError(NprError):
    def __init__(self, reason: str, path: str | Path | None = None) -> None:
        details = {"reason": reason}
        if path is not None:
            details["path"] = str(path)
        super().__init__("E_CHECKPOINT", f"checkpoint failure: {reason}", details)


@dataclass(frozen=True)
class RunIdentity:
    content_hash: str
    version: int
    camera_hash: str
    params_hash: str
    width: int
    height: int
    tile_width: int
    tile_height: int

    @classmethod
    def from_snapshot(cls, snapshot: Snapshot) -> "RunIdentity":
        return cls(
            content_hash=snapshot.content_hash,
            version=snapshot.version,
            camera_hash=snapshot.camera.pose_hash(),
            params_hash=snapshot.params.style_hash(),
            width=snapshot.params.exact_width,
            height=snapshot.params.exact_height,
            tile_width=snapshot.params.tile_width,
            tile_height=snapshot.params.tile_height,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "content_hash": self.content_hash,
            "version": self.version,
            "camera_hash": self.camera_hash,
            "params_hash": self.params_hash,
            "width": self.width,
            "height": self.height,
            "tile_width": self.tile_width,
            "tile_height": self.tile_height,
        }

    @classmethod
    def from_dict(cls, raw: Any) -> "RunIdentity":
        if not isinstance(raw, dict):
            raise ValueError("identity must be an object")
        return cls(
            content_hash=str(raw["content_hash"]),
            version=int(raw["version"]),
            camera_hash=str(raw["camera_hash"]),
            params_hash=str(raw["params_hash"]),
            width=int(raw["width"]),
            height=int(raw["height"]),
            tile_width=int(raw["tile_width"]),
            tile_height=int(raw["tile_height"]),
        )


@dataclass
class LoadedSession:
    identity: RunIdentity
    total_tiles: int
    blocks: dict[int, dict[str, Any]]
    commit_order: list[int]
    invalidated: bool
    invalid_reason: str | None

    @property
    def committed_tile_ids(self) -> tuple[int, ...]:
        return tuple(sorted(self.blocks))

    @property
    def complete(self) -> bool:
        return len(self.blocks) == self.total_tiles


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


class CheckpointStore:
    """One session directory holds one identity at a time.

    All commits are serialized by an in-process lock.  On disk, tile payloads
    are installed with ``os.replace`` before the manifest references them; a
    crash can therefore leave an unreferenced tile, but never a manifest that
    points at a half-written tile.
    """

    SCHEMA = 1

    def __init__(self, base_dir: str | Path, session_id: str = "primary") -> None:
        self.base_dir = Path(base_dir)
        self.session_id = session_id
        self.session_dir = self.base_dir / session_id
        self.tile_dir = self.session_dir / "tiles"
        self.manifest_path = self.session_dir / "manifest.json"
        self._lock = threading.RLock()

    def ensure_writable(self) -> None:
        try:
            self.tile_dir.mkdir(parents=True, exist_ok=True)
            probe = self.session_dir / ".write-probe"
            probe.write_text("ok", encoding="ascii")
            probe.unlink()
        except OSError as exc:
            raise CheckpointError(f"directory is not writable: {exc}", self.session_dir) from exc

    def _write_atomic(self, path: Path, payload: bytes) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
        temp_path = Path(temp_name)
        try:
            with os.fdopen(fd, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_path, path)
            try:
                dir_fd = os.open(path.parent, os.O_RDONLY)
                try:
                    os.fsync(dir_fd)
                finally:
                    os.close(dir_fd)
            except OSError:
                pass
        finally:
            if temp_path.exists():
                temp_path.unlink(missing_ok=True)

    def _manifest_payload(
        self,
        identity: RunIdentity,
        total_tiles: int,
        blocks: dict[int, dict[str, Any]],
        commit_order: list[int],
    ) -> dict[str, Any]:
        return {
            "schema": self.SCHEMA,
            "identity": identity.to_dict(),
            "total_tiles": int(total_tiles),
            "blocks": {str(tile_id): blocks[tile_id] for tile_id in sorted(blocks)},
            "commit_order": list(commit_order),
        }

    def _write_manifest(
        self,
        identity: RunIdentity,
        total_tiles: int,
        blocks: dict[int, dict[str, Any]],
        commit_order: list[int],
    ) -> None:
        payload = self._manifest_payload(identity, total_tiles, blocks, commit_order)
        encoded = (
            json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=True) + "\n"
        ).encode("utf-8")
        self._write_atomic(self.manifest_path, encoded)

    def _invalidate_locked(self, reason: str) -> None:
        if self.session_dir.exists():
            shutil.rmtree(self.session_dir)
        self.session_dir.mkdir(parents=True, exist_ok=True)
        self.tile_dir.mkdir(parents=True, exist_ok=True)
        (self.session_dir / "invalidated.json").write_text(
            json.dumps({"reason": reason}, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )

    def _load_manifest_locked(
        self, identity: RunIdentity, total_tiles: int
    ) -> tuple[dict[int, dict[str, Any]], list[int], bool, str | None]:
        if not self.manifest_path.exists():
            self.session_dir.mkdir(parents=True, exist_ok=True)
            self.tile_dir.mkdir(parents=True, exist_ok=True)
            return {}, [], False, None
        try:
            raw = json.loads(self.manifest_path.read_text(encoding="utf-8"))
            if int(raw.get("schema", -1)) != self.SCHEMA:
                raise ValueError("unsupported manifest schema")
            loaded_identity = RunIdentity.from_dict(raw.get("identity"))
            if loaded_identity != identity:
                raise ValueError("content hash / camera / parameter version mismatch")
            loaded_total = int(raw["total_tiles"])
            if loaded_total != total_tiles:
                raise ValueError("tile count mismatch")
            blocks: dict[int, dict[str, Any]] = {}
            for key, meta in dict(raw.get("blocks", {})).items():
                tile_id = int(key)
                if not isinstance(meta, dict):
                    raise ValueError(f"block {tile_id} metadata is invalid")
                tile_path = self.tile_dir / str(meta["file"])
                if not tile_path.is_file():
                    raise ValueError(f"block {tile_id} payload is missing")
                if _sha256_file(tile_path) != str(meta["sha256"]):
                    raise ValueError(f"block {tile_id} checksum mismatch")
                blocks[tile_id] = dict(meta)
            commit_order = [int(item) for item in raw.get("commit_order", [])]
            if sorted(commit_order) != sorted(blocks):
                raise ValueError("commit order does not match committed blocks")
            return blocks, commit_order, False, None
        except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as exc:
            self._invalidate_locked(str(exc))
            return {}, [], True, str(exc)

    def load_or_begin(self, identity: RunIdentity, total_tiles: int) -> LoadedSession:
        with self._lock:
            self.ensure_writable()
            blocks, commit_order, invalidated, reason = self._load_manifest_locked(
                identity, total_tiles
            )
            if not self.manifest_path.exists() or invalidated:
                self._write_manifest(identity, total_tiles, blocks, commit_order)
            return LoadedSession(
                identity=identity,
                total_tiles=total_tiles,
                blocks=blocks,
                commit_order=commit_order,
                invalidated=invalidated,
                invalid_reason=reason,
            )

    def invalidate(self, reason: str) -> None:
        with self._lock:
            self._invalidate_locked(reason)

    def commit_block(
        self,
        session: LoadedSession,
        block: BlockResult,
    ) -> dict[str, Any]:
        with self._lock:
            if block.content_hash != session.identity.content_hash:
                raise CheckpointError("block content hash differs from session identity")
            if block.version != session.identity.version:
                raise CheckpointError("block parameter version differs from session identity")
            if block.tile_id in session.blocks:
                return session.blocks[block.tile_id]
            file_name = f"tile_{block.tile_id:05d}.npz"
            tile_path = self.tile_dir / file_name
            fd, temp_name = tempfile.mkstemp(
                prefix=f".{file_name}.", suffix=".tmp", dir=self.tile_dir
            )
            temp_path = Path(temp_name)
            os.close(fd)
            try:
                with temp_path.open("wb") as handle:
                    np.savez(
                        handle,
                        base_rgb=block.base_rgb,
                        poster_rgb=block.poster_rgb,
                        levels=block.levels,
                        outline=block.outline.astype(np.uint8),
                        brush_rgba=block.brush_rgba,
                        styled_rgb=block.styled_rgb,
                        coverage=block.coverage.astype(np.uint8),
                        stroke_ids=np.asarray(block.stroke_ids, dtype=np.int64),
                        content_hash=np.asarray(block.content_hash),
                        version=np.asarray(block.version, dtype=np.int64),
                        tile_id=np.asarray(block.tile_id, dtype=np.int64),
                        x0=np.asarray(block.x0, dtype=np.int64),
                        y0=np.asarray(block.y0, dtype=np.int64),
                    )
                    handle.flush()
                    os.fsync(handle.fileno())
                checksum = _sha256_file(temp_path)
                os.replace(temp_path, tile_path)
            finally:
                temp_path.unlink(missing_ok=True)
            meta = {
                "tile_id": block.tile_id,
                "file": file_name,
                "sha256": checksum,
                "commit_seq": len(session.commit_order),
                "stroke_ids": list(block.stroke_ids),
                "stroke_point_count": int(block.stroke_point_count),
                "x0": block.x0,
                "y0": block.y0,
                "x1": block.x1,
                "y1": block.y1,
            }
            session.blocks[block.tile_id] = meta
            session.commit_order.append(block.tile_id)
            self._write_manifest(
                session.identity,
                session.total_tiles,
                session.blocks,
                session.commit_order,
            )
            return meta

    def load_block_payload(self, meta: dict[str, Any]) -> dict[str, np.ndarray]:
        tile_path = self.tile_dir / str(meta["file"])
        if not tile_path.is_file():
            raise CheckpointError(f"missing block file {tile_path}", tile_path)
        if _sha256_file(tile_path) != str(meta["sha256"]):
            raise CheckpointError(f"checksum mismatch for {tile_path}", tile_path)
        with np.load(tile_path, allow_pickle=False) as data:
            return {name: data[name] for name in data.files}

    def read_all_blocks(self, session: LoadedSession) -> dict[int, dict[str, np.ndarray]]:
        payloads: dict[int, dict[str, np.ndarray]] = {}
        for tile_id in sorted(session.blocks):
            payloads[tile_id] = self.load_block_payload(session.blocks[tile_id])
        return payloads

