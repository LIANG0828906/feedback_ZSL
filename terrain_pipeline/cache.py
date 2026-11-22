from __future__ import annotations

import json
import os
import shutil
import uuid
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .bake import ARTIFACT_ORDER, BLOCK_FORMAT
from .errors import E_CACHE, PipelineError
from .model import SNAPSHOT_FORMAT, BakedTile, SnapshotEntry
from .util import (
    SerialGate,
    atomic_write_bytes,
    atomic_write_json,
    read_json,
    sha256_bytes,
    sha256_file,
)

PENDING_FORMAT = "terrain-pending-run/1"
BLOCK_MANIFEST_NAME = "block.json"


@dataclass
class OpenResult:
    snapshot: dict[str, Any] | None
    pending: dict[str, Any] | None
    startup_reason: str | None
    recovered: dict[str, dict[str, Any]]


class SnapshotStore:
    """Content-addressed block cache plus atomic completed-snapshot metadata."""

    def __init__(self, cache_dir: str | Path) -> None:
        self.root = Path(cache_dir).resolve()
        self.blocks_dir = self.root / "blocks"
        self.snapshot_path = self.root / "snapshot.json"
        self.pending_path = self.root / "pending.json"
        self.diagnostics_dir = self.root / "diagnostics"
        self.blocks_dir.mkdir(parents=True, exist_ok=True)
        self.diagnostics_dir.mkdir(parents=True, exist_ok=True)
        self._gate = SerialGate(self.root)

    def open(
        self,
        project_hash: str,
        bake_version: str,
        expected_keys: dict[str, str],
    ) -> OpenResult:
        with self._gate():
            pending = self._read_pending()
            pending_valid = self._pending_matches(
                pending, project_hash, bake_version, expected_keys
            )
            if pending is not None and not pending_valid:
                self.pending_path.unlink(missing_ok=True)
                pending = None

            snapshot: dict[str, Any] | None = None
            reason: str | None = None
            if self.snapshot_path.is_file():
                try:
                    snapshot = read_json(self.snapshot_path)
                except (OSError, UnicodeDecodeError, json.JSONDecodeError):
                    snapshot = None
                    reason = "snapshot_unreadable"
                if snapshot is not None:
                    if snapshot.get("format") != SNAPSHOT_FORMAT:
                        reason = "snapshot_format_mismatch"
                    elif snapshot.get("project_hash") != project_hash:
                        reason = "global_content_hash_mismatch"
                    elif snapshot.get("bake_version") != bake_version:
                        reason = "bake_version_mismatch"
                    elif not self._snapshot_structure_matches(snapshot, expected_keys):
                        reason = "snapshot_tile_key_mismatch"
                    elif not self._verify_snapshot_artifacts(snapshot):
                        reason = "snapshot_artifact_corruption"
                    if reason is not None:
                        snapshot = None

            if snapshot is not None:
                keep = {
                    entry["cache_key"]
                    for entry in snapshot.get("tiles", [])
                    if isinstance(entry, dict) and isinstance(entry.get("cache_key"), str)
                }
                self.pending_path.unlink(missing_ok=True)
                self._gc_blocks(keep)
                return OpenResult(
                    snapshot=snapshot, pending=None, startup_reason=None, recovered={}
                )

            if snapshot is None and reason is None and self.snapshot_path.exists():
                reason = "snapshot_invalid"
            self.snapshot_path.unlink(missing_ok=True)
            recovered = self._recover_pending(pending, expected_keys) if pending_valid else {}
            keep = {item["cache_key"] for item in recovered.values()}
            self._gc_blocks(keep)
            return OpenResult(
                snapshot=None,
                pending=pending if pending_valid else None,
                startup_reason=reason,
                recovered=recovered,
            )

    def discard_all(self) -> None:
        with self._gate():
            self.snapshot_path.unlink(missing_ok=True)
            self.pending_path.unlink(missing_ok=True)
            for child in list(self.blocks_dir.iterdir()):
                if child.is_dir():
                    shutil.rmtree(child, ignore_errors=True)
                else:
                    child.unlink(missing_ok=True)

    def begin_pending(
        self,
        *,
        project_hash: str,
        bake_version: str,
        expected_keys: dict[str, str],
        changed: list[str],
        affected: list[str],
        trigger: list[dict[str, Any]],
        mode: str,
        startup_reason: str | None,
    ) -> dict[str, Any]:
        with self._gate():
            pending = self._read_pending()
            if self._pending_matches(pending, project_hash, bake_version, expected_keys):
                assert pending is not None
                return pending
            pending = {
                "format": PENDING_FORMAT,
                "project_hash": project_hash,
                "bake_version": bake_version,
                "tile_keys": dict(sorted(expected_keys.items())),
                "changed": list(changed),
                "affected": list(affected),
                "trigger": list(trigger),
                "mode": mode,
                "startup_reason": startup_reason,
                "committed": [],
            }
            atomic_write_json(self.pending_path, pending)
            return pending

    def recovered_from_pending(
        self,
        pending: dict[str, Any] | None,
        expected_keys: dict[str, str],
    ) -> dict[str, dict[str, Any]]:
        with self._gate():
            return self._recover_pending(pending, expected_keys)

    def commit_block(self, baked: BakedTile) -> SnapshotEntry:
        with self._gate():
            entry = self._write_block_locked(baked)
            self._mark_pending_committed(baked.tile_id)
            return entry

    def _write_block_locked(self, baked: BakedTile) -> SnapshotEntry:
        final_dir = self.blocks_dir / baked.cache_key
        if final_dir.is_dir():
            entry = self._read_block_entry(baked.cache_key)
            if (
                entry is not None
                and entry.tile_id == baked.tile_id
                and entry.content_hash == baked.content_hash
                and entry.block_manifest_sha256 == sha256_bytes(baked.block_manifest_bytes)
            ):
                return entry
            raise PipelineError(
                E_CACHE,
                "cache key collision or corrupt existing block directory",
                tile_id=baked.tile_id,
            )

        temp_name = f".tmp-{uuid.uuid4().hex}"
        temp_dir = self.blocks_dir / temp_name
        temp_dir.mkdir(parents=False, exist_ok=False)
        try:
            for artifact_name in ARTIFACT_ORDER:
                atomic_write_bytes(temp_dir / artifact_name, baked.artifacts[artifact_name])
            atomic_write_bytes(temp_dir / BLOCK_MANIFEST_NAME, baked.block_manifest_bytes)
            try:
                os.replace(temp_dir, final_dir)
            except OSError:
                if final_dir.is_dir():
                    shutil.rmtree(temp_dir, ignore_errors=True)
                else:
                    raise
        except BaseException:
            shutil.rmtree(temp_dir, ignore_errors=True)
            raise
        entry = self._read_block_entry(baked.cache_key)
        if entry is None:
            raise PipelineError(
                E_CACHE, "block commit could not be read back", tile_id=baked.tile_id
            )
        return entry

    def write_snapshot(self, snapshot: dict[str, Any]) -> None:
        with self._gate():
            atomic_write_json(self.snapshot_path, snapshot)
            self.pending_path.unlink(missing_ok=True)
            keep = {
                entry["cache_key"]
                for entry in snapshot.get("tiles", [])
                if isinstance(entry, dict) and isinstance(entry.get("cache_key"), str)
            }
            self._gc_blocks(keep)

    def read_snapshot(self) -> dict[str, Any] | None:
        if not self.snapshot_path.is_file():
            return None
        return read_json(self.snapshot_path)

    def read_block_manifest(self, cache_key: str) -> dict[str, Any]:
        path = self.blocks_dir / cache_key / BLOCK_MANIFEST_NAME
        if not path.is_file():
            raise PipelineError(
                E_CACHE,
                "block manifest is missing",
                details={"cache_key": cache_key, "path": path.as_posix()},
            )
        try:
            value = read_json(path)
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise PipelineError(
                E_CACHE,
                "block manifest is unreadable",
                details={"cache_key": cache_key, "path": path.as_posix()},
            ) from exc
        if not isinstance(value, dict):
            raise PipelineError(
                E_CACHE,
                "block manifest is not an object",
                details={"cache_key": cache_key},
            )
        return value

    def read_block_artifact(self, cache_key: str, artifact_name: str) -> bytes:
        path = self.blocks_dir / cache_key / artifact_name
        try:
            return path.read_bytes()
        except OSError as exc:
            raise PipelineError(
                E_CACHE,
                "block artifact is missing",
                details={"cache_key": cache_key, "artifact": artifact_name},
            ) from exc

    def write_diagnostics(self, value: dict[str, Any], name: str = "last_run.json") -> Path:
        path = self.diagnostics_dir / name
        atomic_write_json(path, value)
        return path

    def _read_pending(self) -> dict[str, Any] | None:
        if not self.pending_path.is_file():
            return None
        try:
            value = read_json(self.pending_path)
        except (OSError, UnicodeDecodeError, json.JSONDecodeError):
            return None
        return value if isinstance(value, dict) else None

    def _pending_matches(
        self,
        pending: dict[str, Any] | None,
        project_hash: str,
        bake_version: str,
        expected_keys: dict[str, str],
    ) -> bool:
        if pending is None:
            return False
        return (
            pending.get("format") == PENDING_FORMAT
            and pending.get("project_hash") == project_hash
            and pending.get("bake_version") == bake_version
            and pending.get("tile_keys") == dict(sorted(expected_keys.items()))
        )

    def _mark_pending_committed(self, tile_id: str) -> None:
        pending = self._read_pending()
        if pending is None:
            return
        committed = pending.get("committed", [])
        if not isinstance(committed, list):
            committed = []
        if tile_id not in committed:
            committed.append(tile_id)
            committed.sort()
            pending["committed"] = committed
            atomic_write_json(self.pending_path, pending)

    def _recover_pending(
        self,
        pending: dict[str, Any] | None,
        expected_keys: dict[str, str],
    ) -> dict[str, dict[str, Any]]:
        if pending is None:
            return {}
        result: dict[str, dict[str, Any]] = {}
        for tile_id in pending.get("committed", []):
            key = expected_keys.get(tile_id)
            if key is None:
                continue
            try:
                entry = self._read_block_entry(key)
            except PipelineError:
                entry = None
            if entry is not None and entry.tile_id == tile_id:
                result[tile_id] = entry.to_dict()
        return result

    def _read_block_entry(self, cache_key: str) -> SnapshotEntry | None:
        block_manifest_path = self.blocks_dir / cache_key / BLOCK_MANIFEST_NAME
        if not block_manifest_path.is_file():
            return None
        try:
            block_manifest = read_json(block_manifest_path)
        except (OSError, UnicodeDecodeError, json.JSONDecodeError):
            return None
        if not isinstance(block_manifest, dict) or block_manifest.get("format") != BLOCK_FORMAT:
            return None
        if block_manifest.get("cache_key") != cache_key:
            return None
        artifact_hashes = block_manifest.get("artifacts")
        if not isinstance(artifact_hashes, dict):
            return None
        for artifact_name in ARTIFACT_ORDER:
            expected_hash = artifact_hashes.get(artifact_name)
            artifact_path = self.blocks_dir / cache_key / artifact_name
            if not isinstance(expected_hash, str) or not artifact_path.is_file():
                return None
            if sha256_file(artifact_path) != expected_hash:
                return None
        bounds_raw = block_manifest.get("bounds")
        if not isinstance(bounds_raw, list) or len(bounds_raw) != 6:
            return None
        mesh = block_manifest.get("mesh", {})
        return SnapshotEntry(
            tile_id=str(block_manifest.get("tile_id")),
            cache_key=cache_key,
            content_hash=str(block_manifest.get("content_hash")),
            block_dir=f"blocks/{cache_key}",
            block_manifest_sha256=sha256_file(block_manifest_path),
            artifact_hashes={name: artifact_hashes[name] for name in ARTIFACT_ORDER},
            dependencies=tuple(block_manifest.get("dependencies", [])),
            vertex_count=int(mesh.get("vertex_count", 0)),
            instance_count=int(block_manifest.get("instance_count", 0)),
            bounds=tuple(float(value) for value in bounds_raw),
        )

    def _snapshot_structure_matches(
        self,
        snapshot: dict[str, Any],
        expected_keys: dict[str, str],
    ) -> bool:
        tiles = snapshot.get("tiles")
        if not isinstance(tiles, list):
            return False
        by_id: dict[str, dict[str, Any]] = {}
        for entry in tiles:
            if not isinstance(entry, dict):
                return False
            tile_id = entry.get("tile_id")
            key = entry.get("cache_key")
            if not isinstance(tile_id, str) or not isinstance(key, str):
                return False
            if tile_id in by_id:
                return False
            by_id[tile_id] = entry
        if set(by_id) != set(expected_keys):
            return False
        return all(by_id[tile_id].get("cache_key") == key for tile_id, key in expected_keys.items())

    def _verify_snapshot_artifacts(self, snapshot: dict[str, Any]) -> bool:
        for entry in snapshot.get("tiles", []):
            key = entry.get("cache_key")
            tile_id = entry.get("tile_id")
            if not isinstance(key, str) or not isinstance(tile_id, str):
                return False
            block = self._read_block_entry(key)
            if block is None:
                return False
            if block.tile_id != tile_id:
                return False
            if entry.get("block_manifest_sha256") != block.block_manifest_sha256:
                return False
            if entry.get("artifact_hashes") != block.artifact_hashes:
                return False
        return True

    def _gc_blocks(self, keep_keys: Iterable[str]) -> None:
        keep = set(keep_keys)
        for child in list(self.blocks_dir.iterdir()):
            if child.name in keep:
                continue
            if child.is_dir():
                shutil.rmtree(child, ignore_errors=True)
            else:
                child.unlink(missing_ok=True)
