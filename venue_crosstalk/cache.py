"""Content-addressed JSON cache for incremental analysis."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .utils import atomic_write_json, canonical_json, read_json, sha256_json

CACHE_SCHEMA_VERSION = 1


@dataclass(frozen=True)
class CacheLookup:
    payload: dict[str, Any] | None
    key: str
    hit: bool
    reason: str


class CacheStore:
    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.objects = self.root / "objects"
        self.objects.mkdir(parents=True, exist_ok=True)

    def _path(self, key: str) -> Path:
        return self.objects / key[:2] / f"{key}.json"

    def _key(self, node: str, scope: str, dependency_hash: str) -> str:
        return sha256_json(
            {
                "cache_schema": CACHE_SCHEMA_VERSION,
                "node": node,
                "scope": scope,
                "dependency_hash": dependency_hash,
            }
        )

    def get(
        self,
        node: str,
        scope: str,
        dependency_hash: str,
        *,
        force: bool = False,
    ) -> CacheLookup:
        key = self._key(node, scope, dependency_hash)
        if force:
            return CacheLookup(None, key, False, "forced-conservative-recompute")
        path = self._path(key)
        if not path.exists():
            return CacheLookup(None, key, False, "cache-miss")
        try:
            record = read_json(path)
        except (OSError, ValueError):
            try:
                path.unlink()
            except OSError:
                pass
            return CacheLookup(None, key, False, "cache-record-unreadable")
        if (
            record.get("cache_schema") != CACHE_SCHEMA_VERSION
            or record.get("node") != node
            or record.get("scope") != scope
            or record.get("dependency_hash") != dependency_hash
        ):
            return CacheLookup(None, key, False, "cache-metadata-mismatch")
        payload = record.get("payload")
        if not isinstance(payload, dict):
            return CacheLookup(None, key, False, "cache-payload-invalid")
        expected = record.get("payload_hash")
        actual = sha256_json(payload)
        if expected != actual:
            try:
                path.unlink()
            except OSError:
                pass
            return CacheLookup(None, key, False, "cache-hash-mismatch")
        return CacheLookup(payload, key, True, "dependency-and-content-hash-match")

    def put(
        self,
        node: str,
        scope: str,
        dependency_hash: str,
        payload: dict[str, Any],
    ) -> tuple[str, str]:
        key = self._key(node, scope, dependency_hash)
        record = {
            "cache_schema": CACHE_SCHEMA_VERSION,
            "node": node,
            "scope": scope,
            "dependency_hash": dependency_hash,
            "payload_hash": sha256_json(payload),
            "payload": payload,
        }
        atomic_write_json(self._path(key), record)
        return key, record["payload_hash"]

    def publish_atomic(self, workspace_root: str | Path, payload: dict[str, Any]) -> str:
        publish_dir = Path(workspace_root) / "publish"
        publish_dir.mkdir(parents=True, exist_ok=True)
        target = publish_dir / "snapshot.json"
        atomic_write_json(target, payload)
        return str(target)

    def snapshot_exists(self, workspace_root: str | Path) -> bool:
        return (Path(workspace_root) / "publish" / "snapshot.json").exists()

    def read_snapshot(self, workspace_root: str | Path) -> dict[str, Any] | None:
        path = Path(workspace_root) / "publish" / "snapshot.json"
        if not path.exists():
            return None
        try:
            snapshot = read_json(path)
        except (OSError, ValueError):
            return None
        return snapshot if isinstance(snapshot, dict) else None

    @staticmethod
    def record_file(payload: dict[str, Any]) -> bytes:
        return canonical_json(payload, newline=True).encode("utf-8")
